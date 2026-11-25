// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// variableName matches a string that reads as an environment variable's
// name, such as "BASE_URL" or "PAYMENTS_BASE_URL".
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

// TestPackage_reads_no_environment_and_no_file checks the package's own
// source: it imports neither bedrock/config, nor os, io/fs or os/exec, so it
// can read no environment variable and no file, and it never names an
// Environment. No string in it reads as a variable's name: the end user
// names every variable.
func TestPackage_reads_no_environment_and_no_file(t *testing.T) {
	t.Parallel()

	banned := map[string]string{
		"github.com/z5labs/bedrock/config": "the end user sources the settings",
		"os":                               "the package reads no environment variable and no file",
		"os/exec":                          "the package reads no environment variable and no file",
		"io/fs":                            "the package reads no file",
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

// TestPackage_imports_none_of_the_HTTP_1_1_and_HTTP_2_runtimes checks that
// the package's own source imports no package under the core module's
// http/ directory, such as httpserver or httpclient. A package it doesn't
// import is one none of its exported names can name a type of, so the
// package's API is its own. Shared code lives in the core's internal/ tree
// instead.
func TestPackage_imports_none_of_the_HTTP_1_1_and_HTTP_2_runtimes(t *testing.T) {
	t.Parallel()

	const runtimes = "github.com/z5labs/humus/http"
	fset, files := sourceFiles(t)
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if path == runtimes || strings.HasPrefix(path, runtimes+"/") {
				t.Errorf("%s: imports %s, so its API could name one of its types", fset.Position(imp.Pos()), path)
			}
		}
	}
}
