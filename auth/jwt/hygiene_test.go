// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt_test

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

// receiverName returns the name of a method's receiver type, generic or
// not, and through a pointer.
func receiverName(recv ast.Expr) string {
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	switch r := recv.(type) {
	case *ast.IndexExpr:
		recv = r.X
	case *ast.IndexListExpr:
		recv = r.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// TestPackage_exports_only_its_API checks the package's exported names,
// and the exported methods of its exported types, against revision 1's
// auth/jwt block of the authentication proposal, #870.
func TestPackage_exports_only_its_API(t *testing.T) {
	t.Parallel()

	_, files := sourceFiles(t)
	var exported, methods []string
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					exported = append(exported, d.Name.Name)
					continue
				}
				if recv := receiverName(d.Recv.List[0].Type); ast.IsExported(recv) {
					methods = append(methods, recv+"."+d.Name.Name)
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
	require.Equal(t, []string{
		"Algorithm", "CheckAlgorithms", "CheckAudience", "CheckIssuer", "CheckLeeway", "CheckMaxKeyAge",
		"CheckMinRefreshInterval", "DefaultSettings", "ES256", "ES384", "ES512", "EdDSA", "Key", "KeySet",
		"KeySetError", "NewRemoteKeySet", "NewVerifier", "PS256", "PS384", "PS512", "RS256", "RS384", "RS512", "RemoteKeySet", "Settings", "Verifier",
	}, exported)
	slices.Sort(methods)
	require.Equal(t, []string{"KeySetError.Error", "KeySetError.Unwrap", "RemoteKeySet.Keys", "Verifier.Verify"}, methods)
}

// TestPackage_keeps_go_jose_out_of_its_API checks that no exported
// declaration, an exported struct field or a method's signature among
// them, names a type of go-jose's: go-jose is a detail of the package, so
// its next major version doesn't change humus's API.
func TestPackage_keeps_go_jose_out_of_its_API(t *testing.T) {
	t.Parallel()

	fset, files := sourceFiles(t)
	for _, f := range files {
		jose := map[string]bool{}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if !strings.HasPrefix(path, "github.com/go-jose/") {
				continue
			}
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			jose[name] = true
		}
		if len(jose) == 0 {
			continue
		}
		names := func(n ast.Node) {
			ast.Inspect(n, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && jose[id.Name] {
						t.Errorf("%s: the API names go-jose's %s.%s", fset.Position(n.Pos()), id.Name, sel.Sel.Name)
					}
				}
				return true
			})
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				exported := d.Name.IsExported() && (d.Recv == nil || ast.IsExported(receiverName(d.Recv.List[0].Type)))
				if exported {
					names(d.Type)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					s, ok := spec.(*ast.TypeSpec)
					if !ok || !s.Name.IsExported() {
						continue
					}
					st, ok := s.Type.(*ast.StructType)
					if !ok {
						names(s.Type)
						continue
					}
					for _, field := range st.Fields.List {
						if len(field.Names) == 0 || field.Names[0].IsExported() {
							names(field.Type)
						}
					}
				}
			}
		}
	}
}

// TestPackage_reads_no_environment_and_no_file checks that the package
// imports nothing that reads the environment or a file, and never names a
// bedrock.Environment: the end user sources its Settings.
func TestPackage_reads_no_environment_and_no_file(t *testing.T) {
	t.Parallel()

	banned := map[string]string{
		"github.com/z5labs/bedrock/config": "the end user sources the settings",
		"os":                               "the package reads no environment variable and no file",
		"os/exec":                          "the package reads no environment variable and no file",
		"io/fs":                            "the package reads no file",
		"io/ioutil":                        "the package reads no file",
	}
	fset, files := sourceFiles(t)
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if why := banned[path]; why != "" {
				t.Errorf("%s: imports %s, but %s", fset.Position(imp.Pos()), path, why)
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

// TestPackage_sets_no_global checks that the package keeps no state of its
// own at package level but its constants' list and the checks' causes, and
// sets none of another package's: it assigns to no other package's
// variable and calls no other package's Set function, such as
// otel.SetTracerProvider.
func TestPackage_sets_no_global(t *testing.T) {
	t.Parallel()

	fset, files := sourceFiles(t)
	var vars []string
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
			d, ok := decl.(*ast.GenDecl)
			if !ok || d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				for _, n := range spec.(*ast.ValueSpec).Names {
					vars = append(vars, n.Name)
				}
			}
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
	slices.Sort(vars)
	require.Equal(t, []string{"algorithms", "errEmpty", "errNotPositive"}, vars)
}
