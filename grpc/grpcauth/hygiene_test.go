// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcauth_test

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

// sourceFiles parses the package's own source, test files excluded.
func sourceFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()

	names, err := filepath.Glob("*.go")
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

// TestPackage_exports_only_its_API checks that the package's only exported
// names, methods included, are Rule, Default, Method and Interceptors, as
// the authentication proposal, #870, and its revision 1 give it.
func TestPackage_exports_only_its_API(t *testing.T) {
	t.Parallel()

	_, files := sourceFiles(t)
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
	require.Equal(t, []string{"Default", "Interceptors", "Method", "Rule"}, exported)
}

// TestPackage_reads_no_environment_and_no_file checks that the package
// imports nothing that reads the environment or a file, and never names
// an Environment.
func TestPackage_reads_no_environment_and_no_file(t *testing.T) {
	t.Parallel()

	banned := map[string]bool{"os": true, "os/exec": true, "syscall": true, "io/fs": true, "io/ioutil": true}
	fset, files := sourceFiles(t)
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if banned[path] || strings.HasPrefix(path, "github.com/z5labs/bedrock/config") {
				t.Errorf("%s: imports %s, but the package reads no environment variable and no file", fset.Position(imp.Pos()), path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Environment" {
				t.Errorf("%s: names an Environment", fset.Position(n.Pos()))
			}
			return true
		})
	}
}

// TestPackage_sets_no_global checks that the package sets no other
// package's state: it assigns to no other package's variable and calls no
// other package's Set function, such as otel.SetTracerProvider, and that
// it imports no OpenTelemetry package that reaches the globals.
func TestPackage_sets_no_global(t *testing.T) {
	t.Parallel()

	fset, files := sourceFiles(t)
	for _, f := range files {
		imported := make(map[string]bool)
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if path == "go.opentelemetry.io/otel" || path == "go.opentelemetry.io/otel/log/global" {
				t.Errorf("%s: imports %s, which holds OpenTelemetry's globals", fset.Position(imp.Pos()), path)
			}
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imported[name] = true
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && imported[id.Name] {
							t.Errorf("%s: assigns to another package's %s", fset.Position(n.Pos()), sel.Sel.Name)
						}
					}
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && imported[id.Name] && strings.HasPrefix(sel.Sel.Name, "Set") {
					t.Errorf("%s: calls %s.%s", fset.Position(n.Pos()), id.Name, sel.Sel.Name)
				}
			}
			return true
		})
	}
}
