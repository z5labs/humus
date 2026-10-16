// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestModule_sets_the_OpenTelemetry_error_handler_once checks the module's
// own source, test files excluded: App.Run's is the only call to
// otel.SetErrorHandler in humus, and no package imports a logging bridge,
// since the lifecycle logs with the OpenTelemetry Logs API directly.
func TestModule_sets_the_OpenTelemetry_error_handler_once(t *testing.T) {
	fset := token.NewFileSet()
	var calls []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "docs") {
			return filepath.SkipDir
		}
		if d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		imports := map[string]string{} // local name to path
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(p, "go.opentelemetry.io/contrib/bridges/") {
				t.Errorf("%s imports the logging bridge %s", path, p)
			}
			local := filepath.Base(p)
			if imp.Name != nil {
				local = imp.Name.Name
			}
			imports[local] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetErrorHandler" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && imports[pkg.Name] == "go.opentelemetry.io/otel" {
				calls = append(calls, fset.Position(sel.Pos()).String())
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	require.Len(t, calls, 1, "otel.SetErrorHandler is called at %v", calls)
	require.True(t, strings.HasPrefix(calls[0], "app.go:"), "otel.SetErrorHandler is called at %s, want App.Run", calls[0])
}
