package translator

import (
	"bufio"
	"code.google.com/p/go.tools/go/types"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"strings"
	"time"
)

type Translator struct {
	BuildContext *build.Context
	TypesConfig  *types.Config
	GetModTime   func(string) time.Time
	StoreArchive func(*GopherPackage) error
	FileSet      *token.FileSet
	Packages     map[string]*GopherPackage
}

type GopherPackage struct {
	*build.Package
	SrcModTime     time.Time
	JavaScriptCode []byte
}

type ErrorList []error

func (err ErrorList) Error() string {
	return err[0].Error()
}

var PkgObjUpToDate = fmt.Errorf("Package object already up-to-date.")

func (t *Translator) getPackage(importPath string, srcDir string) (*GopherPackage, error) {
	if pkg, found := t.Packages[importPath]; found {
		return pkg, nil
	}

	otherPkg, err := t.BuildContext.Import(importPath, srcDir, build.AllowBinary)
	if err != nil {
		return nil, err
	}
	pkg := &GopherPackage{Package: otherPkg}
	t.Packages[importPath] = pkg
	if err := t.BuildPackage(pkg); err != nil && err != PkgObjUpToDate {
		return nil, err
	}
	return pkg, nil
}

func (t *Translator) BuildPackage(pkg *GopherPackage) error {
	if pkg.ImportPath == "unsafe" {
		t.TypesConfig.Packages["unsafe"] = types.Unsafe
		return nil
	}

	pkg.SrcModTime = t.GetModTime("") // gopherjs itself

	for _, importedPkgPath := range pkg.Imports {
		compiledPkg, err := t.getPackage(importedPkgPath, pkg.Dir)
		if err != nil {
			return err
		}
		if compiledPkg.SrcModTime.After(pkg.SrcModTime) {
			pkg.SrcModTime = compiledPkg.SrcModTime
		}
	}

	for _, name := range pkg.GoFiles {
		fileModTime := t.GetModTime(pkg.Dir + "/" + name)
		if fileModTime.After(pkg.SrcModTime) {
			pkg.SrcModTime = fileModTime
		}
	}

	pkgObjModTime := t.GetModTime(pkg.PkgObj)
	if pkgObjModTime.Unix() != 0 && !pkg.SrcModTime.After(pkgObjModTime) && pkg.PkgObj != "" {
		// package object is up to date, load from disk if library
		if pkg.IsCommand() {
			return PkgObjUpToDate
		}

		objFile, err := t.BuildContext.OpenFile(pkg.PkgObj)
		if err != nil {
			return err
		}
		defer objFile.Close()

		r := bufio.NewReader(objFile)
		for {
			line, err := r.ReadSlice('\n')
			if err != nil && err != bufio.ErrBufferFull {
				return err
			}
			if len(line) == 3 && string(line) == "$$\n" {
				break
			}
			pkg.JavaScriptCode = append(pkg.JavaScriptCode, line...)
		}

		t.TypesConfig.Packages[pkg.ImportPath], err = types.GcImportData(t.TypesConfig.Packages, pkg.PkgObj, pkg.ImportPath, r)
		if err != nil {
			return err
		}

		return PkgObjUpToDate
	}

	files := make([]*ast.File, 0)
	var errList ErrorList
	for _, name := range pkg.GoFiles {
		fullName := pkg.Dir + "/" + name
		r, err := t.BuildContext.OpenFile(fullName)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(t.FileSet, fullName, r, 0)
		if err != nil {
			if list, isList := err.(scanner.ErrorList); isList {
				for _, entry := range list {
					errList = append(errList, entry)
				}
				continue
			}
			errList = append(errList, err)
			continue
		}
		files = append(files, file)
	}
	if errList != nil {
		return errList
	}

	t.TypesConfig.Import = func(imports map[string]*types.Package, path string) (*types.Package, error) {
		_, err := t.getPackage(path, pkg.Dir)
		if err != nil {
			return nil, err
		}
		return imports[path], nil
	}
	var err error
	pkg.JavaScriptCode, err = translatePackage(pkg.ImportPath, files, t.FileSet, t.TypesConfig)
	if err != nil {
		return err
	}

	if !pkg.IsCommand() {
		return t.StoreArchive(pkg)
	}

	var jsCode []byte
	jsCode = []byte(strings.TrimSpace(prelude))
	jsCode = append(jsCode, '\n')

	var initCalls []byte
	allTypeNames := []*types.TypeName{types.New("error").(*types.Named).Obj()}
	loaded := make(map[*types.Package]bool)
	var loadPackage func(*GopherPackage) error
	loadPackage = func(gopherPkg *GopherPackage) error {
		for _, imp := range t.TypesConfig.Packages[gopherPkg.ImportPath].Imports() {
			if imp.Path() == "unsafe" || imp.Path() == "reflect" || imp.Path() == "go/doc" {
				continue
			}
			if _, alreadyLoaded := loaded[imp]; alreadyLoaded {
				continue
			}
			loaded[imp] = true

			impPkg, err := t.getPackage(imp.Path(), pkg.Dir)
			if err != nil {
				return err
			}

			if err := loadPackage(impPkg); err != nil {
				return err
			}
		}

		jsCode = append(jsCode, []byte("Go$packages[\""+gopherPkg.ImportPath+"\"] = (function() {\n")...)
		jsCode = append(jsCode, gopherPkg.JavaScriptCode...)
		scope := t.TypesConfig.Packages[gopherPkg.ImportPath].Scope()
		for _, name := range scope.Names() {
			if typeName, isTypeName := scope.Lookup(name).(*types.TypeName); isTypeName {
				allTypeNames = append(allTypeNames, typeName)
			}
			if name == "init" {
				initCalls = append(initCalls, []byte("Go$packages[\""+gopherPkg.ImportPath+"\"].init();\n")...)
			}
		}
		jsCode = append(jsCode, []byte("})();\n")...)

		return nil
	}
	if err := loadPackage(pkg); err != nil {
		return err
	}

	for _, t := range allTypeNames {
		if in, isInterface := t.Type().Underlying().(*types.Interface); isInterface {
			if in.MethodSet().Len() == 0 {
				continue
			}
			implementedBy := make(map[string]bool, 0)
			for _, other := range allTypeNames {
				_, otherIsInterface := other.Type().Underlying().(*types.Interface)
				otherType := other.Type()
				if _, isStruct := otherType.Underlying().(*types.Struct); isStruct {
					otherType = types.NewPointer(otherType)
				}
				if !otherIsInterface && types.IsAssignableTo(otherType, in) {
					implementedBy[fmt.Sprintf("Go$packages[\"%s\"].%s", other.Pkg().Path(), other.Name())] = true
				}
			}
			list := make([]string, 0, len(implementedBy))
			for ref := range implementedBy {
				list = append(list, ref)
			}
			if t.Name() == "error" {
				jsCode = append(jsCode, []byte(fmt.Sprintf("Go$error.Go$implementedBy = [%s];\n", strings.Join(list, ", ")))...)
				continue
			}
			jsCode = append(jsCode, []byte(fmt.Sprintf("Go$packages[\"%s\"].%s.Go$implementedBy = [%s];\n", t.Pkg().Path(), t.Name(), strings.Join(list, ", ")))...)
		}
	}

	jsCode = append(jsCode, initCalls...)
	jsCode = append(jsCode, []byte("Go$packages[\""+pkg.ImportPath+"\"].main();\n")...)

	pkg.JavaScriptCode = jsCode

	return nil
}
