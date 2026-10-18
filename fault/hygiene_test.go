// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package fault_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPackage_emits_no_log_record_creates_no_instrument_and_uses_no_global
// checks the package's own source, test files excluded. It imports no
// logging package and no metrics API, so it emits no log record and
// creates no instrument, and it doesn't import go.opentelemetry.io/otel,
// whose functions are OpenTelemetry's globals. Its one event goes on the
// span already in a request's context.
func TestPackage_emits_no_log_record_creates_no_instrument_and_uses_no_global(t *testing.T) {
	banned := map[string]string{
		"go.opentelemetry.io/otel":            "its functions are OpenTelemetry's globals",
		"go.opentelemetry.io/otel/log":        "fault emits no log record",
		"go.opentelemetry.io/otel/log/global": "fault emits no log record, and uses no global",
		"go.opentelemetry.io/otel/metric":     "fault creates no instrument",
		"log":                                 "fault emits no log record",
		"log/slog":                            "fault emits no log record",
		"os":                                  "fault reads no environment variable: the end user sources its Settings",
		"github.com/z5labs/bedrock/config":    "the end user sources its Settings",
	}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		require.NoError(t, err)
		checked++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			if why := banned[path]; why != "" {
				t.Errorf("%s: imports %s, but %s", fset.Position(imp.Pos()), path, why)
			}
		}
	}
	require.NotZero(t, checked)
}
