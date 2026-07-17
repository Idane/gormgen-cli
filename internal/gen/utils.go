package gen

import (
	"bytes"
	_ "database/sql"
	_ "database/sql/driver"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
	_ "gorm.io/gorm"
	"gorm.io/gorm/schema"
)

var allowedInterfaces = []types.Type{
	loadNamedType("", "database/sql", "Scanner"),
	loadNamedType("", "database/sql/driver", "Valuer"),
	loadNamedType("", "gorm.io/gorm", "Valuer"),
	loadNamedType("", "gorm.io/gorm/schema", "SerializerInterface"),
}

type ExtractedSQL struct {
	Raw    string
	Where  string
	Select string
}

func extractSQL(comment string, methodName string) ExtractedSQL {
	comment = strings.TrimSpace(comment)

	if index := strings.Index(comment, "\n\n"); index != -1 {
		if strings.Contains(comment[index+2:], methodName) {
			comment = comment[:index]
		} else {
			comment = comment[index+2:]
		}
	}

	sql := strings.TrimPrefix(comment, methodName)
	if strings.HasPrefix(sql, "where(") && strings.HasSuffix(sql, ")") {
		content := strings.TrimSuffix(strings.TrimPrefix(sql, "where("), ")")
		content = strings.Trim(content, "\"")
		content = strings.TrimSpace(content)
		return ExtractedSQL{Where: content}
	} else if strings.HasPrefix(sql, "select(") && strings.HasSuffix(sql, ")") {
		content := strings.TrimSuffix(strings.TrimPrefix(sql, "select("), ")")
		content = strings.Trim(content, "\"")
		content = strings.TrimSpace(content)
		return ExtractedSQL{Select: content}
	}
	return ExtractedSQL{Raw: sql}
}

// ImplementsAllowedInterfaces reports whether typ or *typ implements any allowed interface.
func ImplementsAllowedInterfaces(typ types.Type) bool {
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	for _, t := range allowedInterfaces {
		iface, _ := t.Underlying().(*types.Interface)
		if types.Implements(typ, iface) || types.Implements(types.NewPointer(typ), iface) {
			return true
		}
	}
	return false
}

var (
	goModDirMu    sync.Mutex
	goModDirCache = map[string]string{}
)

func findGoModDir(filename string) string {
	dir := filepath.Dir(filename)

	goModDirMu.Lock()
	defer goModDirMu.Unlock()
	if cached, ok := goModDirCache[dir]; ok {
		return cached
	}

	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Dir = dir
	out, _ := cmd.Output()
	result := filepath.Dir(string(out))

	goModDirCache[dir] = result
	return result
}

var (
	pkgPathMu    sync.Mutex
	pkgPathCache = map[string]string{}
)

// getCurrentPackagePath gets the full import path of the current file's package
func getCurrentPackagePath(filename string) string {
	dir := filepath.Dir(filename)

	pkgPathMu.Lock()
	defer pkgPathMu.Unlock()
	if cached, ok := pkgPathCache[dir]; ok {
		return cached
	}

	cfg := &packages.Config{
		Mode: packages.NeedName,
		Dir:  findGoModDir(filename),
	}

	var result string
	if pkgs, err := packages.Load(cfg, dir); err == nil && len(pkgs) > 0 && pkgs[0].PkgPath != "" {
		result = pkgs[0].PkgPath
	}

	pkgPathCache[dir] = result
	return result
}

// packages.Load is expensive (it type-checks the target package and its whole
// dependency graph), and the generator resolves hundreds of fields against a
// handful of distinct packages. Cache each loaded package by (modRoot, pkgPath)
// so every package is loaded at most once per run.
var (
	loadedTypesMu    sync.Mutex
	loadedTypesCache = map[string]*types.Package{}

	loadedStructMu    sync.Mutex
	loadedStructCache = map[string]map[string]*ast.StructType{}
)

func pkgCacheKey(modRoot, pkgPath string) string {
	return modRoot + "\x00" + pkgPath
}

// loadNamedType returns a named type from a package, caching the loaded package.
func loadNamedType(modRoot, pkgPath, name string) types.Type {
	key := pkgCacheKey(modRoot, pkgPath)

	loadedTypesMu.Lock()
	pkg, ok := loadedTypesCache[key]
	if !ok {
		cfg := &packages.Config{
			Mode: packages.NeedTypes | packages.NeedName | packages.NeedDeps,
			Dir:  modRoot,
		}
		if pkgs, err := packages.Load(cfg, pkgPath); err == nil && len(pkgs) > 0 {
			pkg = pkgs[0].Types
		}
		loadedTypesCache[key] = pkg // cache misses (nil) too, to avoid re-loading
	}
	loadedTypesMu.Unlock()

	if pkg == nil {
		return nil
	}
	if obj := pkg.Scope().Lookup(name); obj != nil {
		return obj.Type()
	}
	return nil
}

// loadNamedStructType loads a struct type definition from an external package by
// name, caching the package's struct declarations by (modRoot, pkgPath).
func loadNamedStructType(modRoot, pkgPath, name string) (*ast.StructType, error) {
	key := pkgCacheKey(modRoot, pkgPath)

	loadedStructMu.Lock()
	structs, ok := loadedStructCache[key]
	if !ok {
		cfg := &packages.Config{
			Mode: packages.NeedSyntax | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedName,
			Dir:  modRoot,
		}

		pkgs, err := packages.Load(cfg, pkgPath)
		if err != nil {
			loadedStructMu.Unlock()
			return nil, fmt.Errorf("failed to load package %q from %v: %w", pkgPath, modRoot, err)
		}
		if len(pkgs) == 0 {
			loadedStructMu.Unlock()
			return nil, fmt.Errorf("no packages found for path %q from %v", pkgPath, modRoot)
		}

		structs = map[string]*ast.StructType{}
		for _, pkg := range pkgs {
			for _, syntax := range pkg.Syntax {
				for _, decl := range syntax.Decls {
					gen, ok := decl.(*ast.GenDecl)
					if !ok {
						continue
					}
					for _, spec := range gen.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						if st, ok := ts.Type.(*ast.StructType); ok {
							structs[ts.Name.Name] = st
						}
					}
				}
			}
		}
		loadedStructCache[key] = structs
	}
	loadedStructMu.Unlock()

	if st, ok := structs[name]; ok {
		return st, nil
	}
	return nil, fmt.Errorf("struct %s not found in package %s", name, pkgPath)
}

// generateDBName generates database column name using GORM's NamingStrategy and COLUMN tag.
func generateDBName(fieldName, gormTag string) string {
	tagSettings := schema.ParseTagSetting(reflect.StructTag(gormTag).Get("gorm"), ";")
	if tagSettings["COLUMN"] != "" {
		return tagSettings["COLUMN"]
	}

	// Use GORM's NamingStrategy with IdentifierMaxLength: 64
	ns := schema.NamingStrategy{IdentifierMaxLength: 64}
	return ns.ColumnName("", fieldName)
}

// mergeImports appends imports from src into dst if not already present (by Path)
func mergeImports(dst *[]Import, src []Import) {
	existing := map[string]bool{}
	for _, i := range *dst {
		existing[i.Path] = true
	}
	for _, i := range src {
		if !existing[i.Path] {
			*dst = append(*dst, i)
			existing[i.Path] = true
		}
	}
}

// shouldSkipFile checks if a file contains the generated code header and should be skipped
func shouldSkipFile(filePath string) bool {
	if !strings.HasSuffix(filePath, ".go") {
		return true
	}

	content, err := os.ReadFile(filePath)
	return err == nil && bytes.Contains(content, []byte(codeGenHint))
}

// strLit returns the unquoted string if expr is a string literal; otherwise "".
func strLit(expr ast.Expr) string {
	if bl, ok := expr.(*ast.BasicLit); ok && bl.Kind == token.STRING {
		if s, err := strconv.Unquote(bl.Value); err == nil {
			return s
		}
	}

	return ""
}

func stripGeneric(s string) string {
	if i := strings.Index(s, "["); i >= 0 {
		return s[:i]
	}
	return s
}
