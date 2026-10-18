// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jsoncodec_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// openapiPath is the import path of the only package outside the standard
// library jsoncodec may import.
const openapiPath = "github.com/z5labs/humus/openapi/v3"

// sourceFiles parses the source of the package in dir, test files
// excluded.
func sourceFiles(t *testing.T, dir string) (*token.FileSet, []*ast.File) {
	t.Helper()

	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files = append(files, f)
	}
	require.NotEmpty(t, files)
	return fset, files
}

// TestPackage_exports_only_its_API checks the package's exported names
// against the API revision 3 of the OpenAPI proposal, #760, gives it.
func TestPackage_exports_only_its_API(t *testing.T) {
	t.Parallel()

	_, files := sourceFiles(t, ".")
	var exported []string
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.IsExported() && d.Recv == nil {
					exported = append(exported, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							exported = append(exported, s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								exported = append(exported, n.Name)
							}
						}
					}
				}
			}
		}
	}
	slices.Sort(exported)
	require.Equal(t, []string{"Decoder", "Encoder", "Problem"}, exported)
}

// TestPackage_imports_only_the_standard_library_and_openapi checks that
// jsoncodec is built on openapi's exported API and the standard library
// alone, and that, of the standard library, it imports nothing that reads
// the environment or a file.
func TestPackage_imports_only_the_standard_library_and_openapi(t *testing.T) {
	t.Parallel()

	banned := map[string]bool{"os": true, "os/exec": true, "io/fs": true, "io/ioutil": true, "unsafe": true}
	fset, files := sourceFiles(t, ".")
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			first, _, _ := strings.Cut(path, "/")
			switch {
			case path == openapiPath:
			case strings.Contains(first, "."):
				t.Errorf("%s: imports %s, which is neither openapi nor in the standard library", fset.Position(imp.Pos()), path)
			case banned[path]:
				t.Errorf("%s: imports %s, but the package reads no environment variable and no file", fset.Position(imp.Pos()), path)
			}
		}
	}
}

// TestOpenapi_doesnt_import_the_package checks that openapi never uses
// jsoncodec, so its codecs are opt-in, like any third party's.
func TestOpenapi_doesnt_import_the_package(t *testing.T) {
	t.Parallel()

	fset, files := sourceFiles(t, "..")
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if strings.HasPrefix(path, openapiPath+"/") {
				t.Errorf("%s: openapi imports %s", fset.Position(imp.Pos()), path)
			}
		}
	}
}

// TestPackage_sets_no_global checks that the package keeps no state of its
// own at package level, and assigns to no other package's variable.
func TestPackage_sets_no_global(t *testing.T) {
	t.Parallel()

	fset, files := sourceFiles(t, ".")
	for _, f := range files {
		imported := make(map[string]bool)
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imported[name] = true
		}

		for _, decl := range f.Decls {
			if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.VAR {
				t.Errorf("%s: declares a package-level variable", fset.Position(d.Pos()))
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if id, ok := sel.X.(*ast.Ident); ok && imported[id.Name] {
					t.Errorf("%s: assigns to another package's %s", fset.Position(assign.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
}
