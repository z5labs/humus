// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry_test

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

// TestPackage_reads_no_environment checks the package's own source: it does
// not import bedrock/config, never names an Environment, neither the type
// bedrock.Environment nor a Process's field, and never calls os.Getenv,
// os.LookupEnv or os.Environ. Lint forbids the os calls
// everywhere; this test is the package's own promise.
func TestPackage_reads_no_environment(t *testing.T) {
	t.Parallel()

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

		imports := map[string]string{} // local name to path
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			require.NotEqual(t, "github.com/z5labs/bedrock/config", path, "%s imports bedrock/config", name)
			local := filepath.Base(path)
			if imp.Name != nil {
				local = imp.Name.Name
			}
			imports[local] = path
		}

		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Environment" {
				t.Errorf("%s: names an Environment", fset.Position(sel.Pos()))
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && imports[pkg.Name] == "os" {
				switch sel.Sel.Name {
				case "Getenv", "LookupEnv", "Environ":
					t.Errorf("%s: calls os.%s", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	require.NotZero(t, checked)
}
