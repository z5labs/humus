// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

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
// and the exported methods of its exported types, against the API the
// authentication proposal, #870, and its revision 1 give it: nothing for
// openapi/v3 or grpcauth, which read a Requirement through internal/authn.
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
		"APIKeyCookie", "APIKeyHeader", "APIKeyQuery", "All", "Anonymous", "Answers", "AuthorizationCodeFlow",
		"Basic", "Bearer", "ClientCredentialsFlow", "DeviceAuthorizationFlow", "Disabled", "ErrForbidden",
		"ErrUnauthenticated", "Flows", "MutualTLS", "NewHandler", "OAuth2", "OpenIDConnect", "Require",
		"RequireScopes", "Requirement", "Scheme", "Scoped",
	}, exported)
	slices.Sort(methods)
	require.Equal(t, []string{"Scheme.Description", "Scheme.Format", "Scheme.From"}, methods)
}

// TestPackage_uses_only_the_standard_library_and_OpenTelemetry checks that
// the package imports nothing but the standard library, OpenTelemetry's
// API, humus for its Setup, and the engine it shares with openapi and
// grpcauth, and nothing that reads the environment or a file.
func TestPackage_uses_only_the_standard_library_and_OpenTelemetry(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		"github.com/z5labs/humus":                true,
		"github.com/z5labs/humus/internal/authn": true,
	}
	banned := map[string]bool{"os": true, "os/exec": true, "io/fs": true, "io/ioutil": true}

	fset, files := sourceFiles(t)
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			pos := fset.Position(imp.Pos())
			switch {
			case banned[path]:
				t.Errorf("%s: imports %s, but the package reads no environment variable and no file", pos, path)
			case allowed[path], !strings.Contains(strings.Split(path, "/")[0], "."):
			case strings.HasPrefix(path, "go.opentelemetry.io/otel/") && !strings.Contains(path, "/sdk"):
			default:
				t.Errorf("%s: imports %s, which is neither the standard library nor OpenTelemetry's API", pos, path)
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
// own at package level, but its two sentinel errors, and sets none of
// another package's: it assigns to no other package's variable and calls
// no other package's Set function, such as otel.SetTracerProvider.
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
	require.Equal(t, []string{"ErrForbidden", "ErrUnauthenticated"}, vars)
}
