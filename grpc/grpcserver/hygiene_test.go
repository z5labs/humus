// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

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
// name, such as "ADDR" or "GRPC_ADDR".
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

// TestPackage_exports_only_Settings_its_checks_the_Service_and_Main
// checks that the package's only exported names, methods included, are
// Settings, CheckAddr and CheckTLS, the Service with NewService and its
// methods, and Defaults and Main: in particular, there is no
// DefaultSettings and no App.
func TestPackage_exports_only_Settings_its_checks_the_Service_and_Main(t *testing.T) {
	t.Parallel()

	_, files := sourceFiles(t)
	var exported []string
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.IsExported() {
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
	require.Equal(t, []string{
		"Addr", "CheckAddr", "CheckTLS", "Defaults", "GetServiceInfo", "Main",
		"NewService", "RegisterService", "Serve", "Service", "Settings", "Shutdown",
	}, exported)
}

// TestPackage_reads_no_environment_and_no_file checks that the package
// imports neither bedrock/config, nor os, os/exec, syscall, io/fs or
// io/ioutil, so it can read no environment variable and no file, and that
// it never names an Environment. No string in it reads as a variable's
// name: the end user names every variable.
func TestPackage_reads_no_environment_and_no_file(t *testing.T) {
	t.Parallel()

	banned := map[string]string{
		"os":        "the package reads no environment variable and no file",
		"os/exec":   "the package reads no environment variable and no file",
		"syscall":   "the package reads no environment variable and no file",
		"io/fs":     "the package reads no file",
		"io/ioutil": "the package reads no file",
	}
	// bedrock/config and every package under it source settings, which is
	// the end user's to do.
	const config = "github.com/z5labs/bedrock/config"

	fset, files := sourceFiles(t)
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if why := banned[path]; why != "" {
				t.Errorf("%s: imports %s, but %s", fset.Position(imp.Pos()), path, why)
			}
			if path == config || strings.HasPrefix(path, config+"/") {
				t.Errorf("%s: imports %s, but the end user sources the settings", fset.Position(imp.Pos()), path)
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
