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
// the root package, or one of OpenTelemetry's trace and codes APIs, all of
// which the core module already requires, so job adds no module
// dependency. It imports neither OpenTelemetry's root package, which holds
// the globals, nor a logging or metrics API, since the package writes no log
// records and records no metrics. The source also has no go statement and
// no call to recover: the run runs on Serve's goroutine, and its panics
// aren't recovered.
func TestPackage_imports_only_what_a_job_needs(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		"github.com/z5labs/humus":        true,
		"go.opentelemetry.io/otel/codes": true,
		"go.opentelemetry.io/otel/trace": true,
	}
	banned := map[string]bool{
		"log":      true,
		"log/slog": true,
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
			case banned[path]:
				t.Errorf("%s: imports %s, but the package writes no log records", fset.Position(imp.Pos()), path)
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
