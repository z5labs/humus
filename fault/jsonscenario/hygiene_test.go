// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jsonscenario_test

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
// against the Proposed API of the failure injection proposal, #869: Codec,
// New, and Codec's Decode, Strip and Encode.
func TestPackage_exports_only_its_API(t *testing.T) {
	t.Parallel()

	_, files := sourceFiles(t, ".")
	var exported []string
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				name := d.Name.Name
				if d.Recv != nil {
					recv := d.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					typ := recv.(*ast.Ident)
					if !typ.IsExported() {
						continue
					}
					name = typ.Name + "." + name
				}
				exported = append(exported, name)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							exported = append(exported, s.Name.Name)
						}
						if st, ok := s.Type.(*ast.StructType); ok && s.Name.IsExported() {
							for _, field := range st.Fields.List {
								for _, n := range field.Names {
									if n.IsExported() {
										exported = append(exported, s.Name.Name+"."+n.Name)
									}
								}
							}
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
	require.Equal(t, []string{"Codec", "Codec.Decode", "Codec.Encode", "Codec.Strip", "New"}, exported)
}

// TestPackage_depends_only_on_fit_and_the_baggage_API checks that the
// package needs nothing beyond the standard library, fit and
// OpenTelemetry's baggage package, which the core module already depends
// on, and that, of the standard library, it imports nothing that reads the
// environment or a file.
func TestPackage_depends_only_on_fit_and_the_baggage_API(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		"github.com/z5labs/bedrock/fit":    true,
		"go.opentelemetry.io/otel/baggage": true,
	}
	banned := map[string]bool{"os": true, "os/exec": true, "io/fs": true, "io/ioutil": true, "unsafe": true, "log": true, "log/slog": true}
	fset, files := sourceFiles(t, ".")
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			first, _, _ := strings.Cut(path, "/")
			switch {
			case allowed[path]:
			case strings.Contains(first, "."):
				t.Errorf("%s: imports %s, which is neither fit, OpenTelemetry's baggage package nor in the standard library", fset.Position(imp.Pos()), path)
			case banned[path]:
				t.Errorf("%s: imports %s, but the package logs nothing and reads no environment variable and no file", fset.Position(imp.Pos()), path)
			}
		}
	}
}

// TestFault_doesnt_import_the_package checks that fault never uses
// jsonscenario, so the codec is opt-in, like any end user's.
func TestFault_doesnt_import_the_package(t *testing.T) {
	t.Parallel()

	fset, files := sourceFiles(t, "..")
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if path == "github.com/z5labs/humus/fault/jsonscenario" {
				t.Errorf("%s: fault imports %s", fset.Position(imp.Pos()), path)
			}
		}
	}
}

// TestPackage_keeps_no_state checks that the package declares no variable
// at package level other than the compile-time interface checks and its
// error causes, so a Codec holds its key and nothing is shared.
func TestPackage_keeps_no_state(t *testing.T) {
	t.Parallel()

	fset, files := sourceFiles(t, ".")
	for _, f := range files {
		for _, decl := range f.Decls {
			d, ok := decl.(*ast.GenDecl)
			if !ok || d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				for _, n := range spec.(*ast.ValueSpec).Names {
					if n.Name != "_" && !strings.HasPrefix(n.Name, "err") {
						t.Errorf("%s: package-level variable %s", fset.Position(n.Pos()), n.Name)
					}
				}
			}
		}
	}
}
