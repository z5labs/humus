// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// variableName matches a string that reads as an environment variable's
// name, such as "KUBECONFIG" or "KUBERNETES_SERVICE_HOST".
var variableName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*$`)

// sources parses the package's own source, test files excluded, and returns
// each file by name.
func sources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files[name] = f
	}
	// The package doc and the Client are both checked.
	require.Contains(t, files, "doc.go")
	require.Contains(t, files, "client.go")
	return fset, files
}

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
		// clientcmd reads kubeconfig files and KUBECONFIG; the end user
		// fills the Connection.
		"k8s.io/client-go/tools/clientcmd": "the end user fills the Connection",
	}
	// bedrock/config and every package under it source settings, which is
	// the end user's to do.
	const config = "github.com/z5labs/bedrock/config"

	fset, files := sources(t)
	for _, f := range files {
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if why := banned[p]; why != "" {
				t.Errorf("%s: imports %s, but %s", fset.Position(imp.Pos()), p, why)
			}
			if p == config || strings.HasPrefix(p, config+"/") {
				t.Errorf("%s: imports %s, but the end user sources the settings", fset.Position(imp.Pos()), p)
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

// TestPackage_sets_no_global checks the package's own source, test files
// excluded: it has no init function, and it calls none of the functions
// that set a global of client-go's, klog's or OpenTelemetry's, nor
// rest.InClusterConfig, which reads the environment and the disk itself.
// The lint config denies the same calls in every module; this test holds
// the package to it whatever name it imports them under.
func TestPackage_sets_no_global(t *testing.T) {
	t.Parallel()

	banned := map[string]map[string]string{
		"k8s.io/klog/v2": {
			"SetLogger":            "klog's logger is global",
			"SetLoggerWithOptions": "klog's logger is global",
			"SetOutput":            "klog's output is global",
			"SetOutputBySeverity":  "klog's output is global",
			"InitFlags":            "klog's flags are global",
			"LogToStderr":          "klog's output is global",
		},
		"k8s.io/client-go/features": {
			"ReplaceFeatureGates": "client-go's feature gates are global",
		},
		"k8s.io/client-go/util/workqueue": {
			"SetProvider": "the work queue's metrics provider is global",
		},
		"k8s.io/client-go/tools/leaderelection": {
			"SetProvider": "leader election's metrics provider is global",
		},
		"k8s.io/client-go/tools/metrics": {
			"Register": "client-go's metrics are global",
		},
		"k8s.io/client-go/rest": {
			"InClusterConfig": "it reads the environment and the disk; the end user fills the Connection",
		},
		"go.opentelemetry.io/otel": {
			"SetTracerProvider":    "OpenTelemetry's providers are passed in",
			"SetMeterProvider":     "OpenTelemetry's providers are passed in",
			"SetTextMapPropagator": "OpenTelemetry's propagator is passed in",
			"GetTracerProvider":    "OpenTelemetry's providers are passed in",
			"GetMeterProvider":     "OpenTelemetry's providers are passed in",
			"GetTextMapPropagator": "OpenTelemetry's propagator is passed in",
		},
	}

	fset, files := sources(t)
	for _, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
				t.Errorf("%s: has an init function", fset.Position(fn.Pos()))
			}
		}

		// The banned functions, by the name each package is imported under
		// in this file.
		byName := map[string]map[string]string{}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			funcs := banned[p]
			if funcs == nil {
				continue
			}
			name := path.Base(p)
			if p == "k8s.io/klog/v2" {
				name = "klog"
			}
			if imp.Name != nil {
				name = imp.Name.Name
			}
			byName[name] = funcs
		}

		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if why := byName[x.Name][sel.Sel.Name]; why != "" {
				t.Errorf("%s: uses %s.%s, but %s", fset.Position(sel.Pos()), x.Name, sel.Sel.Name, why)
			}
			return true
		})
	}
}
