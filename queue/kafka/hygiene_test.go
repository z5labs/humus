// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

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
// name, such as "BROKERS" or "KAFKA_BROKERS".
var variableName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*$`)

// TestPackage_reads_no_environment_and_no_file checks the package's own
// source, test files excluded: it imports neither bedrock/config, nor os,
// os/exec, syscall, io/fs or io/ioutil, so it can read no environment
// variable and no file, and it never names an Environment. No string in it
// reads as a variable's name: the end user names every variable.
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

	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	checked := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		checked++

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
	require.NotZero(t, checked)
}

// TestPackage_uses_kotels_Meter_alone checks the package's own source, test
// files excluded: of kotel, it uses NewMeter and MeterProvider and nothing
// else. kotel's tracer and NewKotel are not used: the record's span and the
// propagation through headers are humus's own.
func TestPackage_uses_kotels_Meter_alone(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{"NewMeter": true, "MeterProvider": true, "Meter": true}
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	used := map[string]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)

		local := ""
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if path != "github.com/twmb/franz-go/plugin/kotel" {
				continue
			}
			local = "kotel"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		if local == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == local {
				used[sel.Sel.Name] = true
				if !allowed[sel.Sel.Name] {
					t.Errorf("%s: uses kotel.%s, but only kotel's Meter is used", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	require.True(t, used["NewMeter"], "the package builds kotel's Meter")
}
