// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

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
// name, such as "QUEUE" or "RABBITMQ_QUEUE".
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
	var checked []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		checked = append(checked, name)

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
	// The package doc and the settings are both checked.
	require.Subset(t, checked, []string{"doc.go", "settings.go"})
}

// TestPackage_dials_only_through_the_Host checks the package's own source,
// test files excluded: it never calls amqp091's Dial, DialConfig, DialTLS,
// DialTLS_ExternalAuth or DefaultDial, which take or build a URL and dial
// the real network, nor ParseURI, and it imports neither net/url nor
// amqp091's logger setter. The broker is reached only through the Host.
func TestPackage_dials_only_through_the_Host(t *testing.T) {
	t.Parallel()

	const amqp = "github.com/rabbitmq/amqp091-go"
	banned := map[string]bool{
		"Dial":                 true,
		"DialConfig":           true,
		"DialTLS":              true,
		"DialTLS_ExternalAuth": true,
		"DefaultDial":          true,
		"ParseURI":             true,
		"SetLogger":            true,
	}

	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var checked []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		checked = append(checked, name)

		// The names amqp091 is imported under in this file.
		aliases := map[string]bool{}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			switch {
			case path == "net/url":
				t.Errorf("%s: imports net/url, but the package builds no URL", fset.Position(imp.Pos()))
			case path == amqp && imp.Name != nil:
				aliases[imp.Name.Name] = true
			case path == amqp:
				aliases["amqp091"] = true
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && aliases[x.Name] && banned[sel.Sel.Name] {
				t.Errorf("%s: uses %s.%s, but the broker is reached only through the Host", fset.Position(sel.Pos()), x.Name, sel.Sel.Name)
			}
			return true
		})
	}
	require.Contains(t, checked, "connect.go")
}
