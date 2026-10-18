// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

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
// name, such as "CARTS_ADDR" or "HTTPS_PROXY".
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

// TestPackage_exports_only_Settings_its_checks_and_the_Client
// checks that the package's only exported names, methods included, are
// Settings, DefaultSettings and the checks, Classification, and the Client
// with NewClient, Invoke, NewStream and Connected: in particular, the Client has no Close,
// and there is no App and no Main.
func TestPackage_exports_only_Settings_its_checks_and_the_Client(t *testing.T) {
	t.Parallel()

	_, files := sourceFiles(t)
	var exported []string
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				// A method of an unexported type, such as an error's
				// Error, is no part of the package's API.
				if d.Name.IsExported() && receiverExported(d) {
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
		"CheckAddr", "CheckBackoffBase", "CheckBackoffMax",
		"CheckBreakerOpenFor", "CheckBreakerProbeTimeout", "CheckBreakerProbes", "CheckBreakerWindow",
		"CheckMaxAttempts",
		"CheckRetryBudgetMaxTokens", "CheckRetryBudgetTokenRatio", "CheckTLS", "CheckTimeout",
		"Classification", "Client", "Connected", "DefaultSettings",
		"Invoke", "NewClient", "NewStream", "Settings",
	}, exported)
}

// receiverExported reports whether d is a function, or a method of an
// exported type.
func receiverExported(d *ast.FuncDecl) bool {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return true
	}
	typ := d.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	id, ok := typ.(*ast.Ident)
	return !ok || id.IsExported()
}

// TestPackage_reads_no_environment_and_no_file checks that the package
// imports neither bedrock/config, nor os, os/exec, syscall, io/fs or
// io/ioutil, so it can read no environment variable and no file, and that
// it never names an Environment, so it never reads a bedrock.Environment.
// No string in it reads as a variable's name: the end user names every
// variable, and the Client reads none of the proxy variables.
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
				if n.Sel.Name == "Environment" || n.Sel.Name == "ProxyFromEnvironment" {
					t.Errorf("%s: names %s", fset.Position(n.Pos()), n.Sel.Name)
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
