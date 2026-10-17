// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPackage_imports_only_what_a_job_needs checks the package's own
// source, test files excluded. Every import is a standard library package,
// bedrock, the root package, or one of OpenTelemetry's trace, codes and
// propagation APIs, all of which the core module already requires, so job
// adds no module dependency. It imports neither OpenTelemetry's root
// package, which holds the globals, nor a logging or metrics API, since the
// package writes no log records and records no metrics, nor contrib's
// envcar, which reads the OS process's environment rather than the
// Process's: the carrier over the environment is job's own. The source also has no go statement and
// no call to recover: the run runs on Serve's goroutine, and its panics
// aren't recovered.
func TestPackage_imports_only_what_a_job_needs(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		"github.com/z5labs/bedrock":            true,
		"github.com/z5labs/humus":              true,
		"go.opentelemetry.io/otel/codes":       true,
		"go.opentelemetry.io/otel/propagation": true,
		"go.opentelemetry.io/otel/trace":       true,
	}
	banned := map[string]string{
		"log":      "the package writes no log records",
		"log/slog": "the package writes no log records",
		"go.opentelemetry.io/contrib/propagators/envcar": "the carrier reads the Process's environment, not the OS process's",
	}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		checked++

		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			first, _, _ := strings.Cut(path, "/")
			stdlib := !strings.Contains(first, ".")
			switch {
			case banned[path] != "":
				t.Errorf("%s: imports %s, but %s", fset.Position(imp.Pos()), path, banned[path])
			case !stdlib && !allowed[path]:
				t.Errorf("%s: imports %s, which a job doesn't need", fset.Position(imp.Pos()), path)
			}
		}

		ast.Inspect(f, func(n ast.Node) bool {
			if g, ok := n.(*ast.GoStmt); ok {
				t.Errorf("%s: starts a goroutine", fset.Position(g.Pos()))
			}
			if r, ok := n.(*ast.CallExpr); ok {
				if id, ok := r.Fun.(*ast.Ident); ok && id.Name == "recover" {
					t.Errorf("%s: recovers a panic", fset.Position(r.Pos()))
				}
			}
			return true
		})
	}
	require.NotZero(t, checked)
}
