// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// variableName matches a string that reads as an environment variable's
// name, such as "ADDR" or "HTTP_READ_TIMEOUT".
var variableName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*$`)

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
// OpenAPI proposal, #760, gives it.
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
		"API", "APIOption", "CatchAll", "Decoder", "Delete", "EmptyBody", "EmptyDecoder", "EmptyEncoder",
		"Encoder", "Error", "Fallback", "Get", "Info", "NewAPI", "Operation", "Option", "Param", "ParamFrom",
		"ParamValue", "Patch", "Path", "PathParam", "PathParameter", "Post", "Put", "Request", "RequestError",
		"RequestErrorAnswer", "RequestErrors", "Root", "Schema", "Status", "Unmatched",
	}, exported)
	slices.Sort(methods)
	require.Equal(t, []string{
		"API.ServeHTTP", "Path.Lit", "Path.Param", "Path.String", "Request.Body", "RequestError.Error", "RequestError.Unwrap",
	}, methods)
}

// TestPackage_reads_no_environment_and_no_file checks that the package
// imports neither bedrock/config, nor os, os/exec, io/fs or io/ioutil, so it
// can read no environment variable and no file, and that it never names an
// Environment. No string in it reads as a variable's name.
func TestPackage_reads_no_environment_and_no_file(t *testing.T) {
	t.Parallel()

	banned := map[string]string{
		"github.com/z5labs/bedrock/config": "the package has no settings",
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
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if n.Sel.Name == "Environment" {
					t.Errorf("%s: names an Environment", fset.Position(n.Pos()))
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(n.Value)
				require.NoError(t, err)
				for _, word := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == ':' }) {
					if variableName.MatchString(word) && strings.Contains(word, "_") {
						t.Errorf("%s: names a variable, %s", fset.Position(n.Pos()), word)
					}
				}
				if variableName.MatchString(s) {
					t.Errorf("%s: names a variable, %s", fset.Position(n.Pos()), s)
				}
			}
			return true
		})
	}
}

// TestPackage_sets_no_global checks that the package keeps no state of its
// own at package level, and sets none of another package's: it declares no
// package-level variable, assigns to no other package's variable, such as
// http.DefaultClient, calls no other package's Set function, such as
// otel.SetTracerProvider or slog.SetDefault, and registers nothing on
// net/http's DefaultServeMux.
func TestPackage_sets_no_global(t *testing.T) {
	t.Parallel()

	fset, files := sourceFiles(t)
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
		// A package imported by a path ending in a major version is
		// named after the element before it.
		imported["chi"] = true
		imported["semconv"] = true

		for _, decl := range f.Decls {
			if d, ok := decl.(*ast.GenDecl); ok && d.Tok == token.VAR {
				t.Errorf("%s: declares a package-level variable", fset.Position(d.Pos()))
			}
		}

		isPackage := func(e ast.Expr) (*ast.SelectorExpr, bool) {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return nil, false
			}
			id, ok := sel.X.(*ast.Ident)
			return sel, ok && imported[id.Name]
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := isPackage(lhs); ok {
						t.Errorf("%s: assigns to another package's %s", fset.Position(n.Pos()), sel.Sel.Name)
					}
				}
			case *ast.CallExpr:
				sel, ok := isPackage(n.Fun)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				if strings.HasPrefix(name, "Set") || (sel.X.(*ast.Ident).Name == "http" && (name == "Handle" || name == "HandleFunc")) {
					t.Errorf("%s: calls %s.%s", fset.Position(n.Pos()), sel.X.(*ast.Ident).Name, name)
				}
			}
			return true
		})
	}
}
