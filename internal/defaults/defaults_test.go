// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package defaults_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/bedrocktest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus/internal/defaults"
)

// TestTelemetry_writes_JSON_lines_to_stdout_and_starts_nothing builds the
// telemetry default inside a synctest bubble and never shuts it down. The
// bubble ends only once every goroutine started in it has exited, so it
// would fail if the default had started an exporter, a batch processor or a
// periodic reader: anything that ticks. Its records reach the Process's
// stdout at once, as JSON lines, and nothing reaches stderr.
func TestTelemetry_writes_JSON_lines_to_stdout_and_starts_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := bedrocktest.NewProcess(t)
		dial := func(context.Context, string, string) (net.Conn, error) {
			t.Error("the telemetry default dialled out")
			return nil, net.ErrClosed
		}

		sdk, err := defaults.Telemetry(t.Context(), p.Process(), dial)
		require.NoError(t, err)

		_, span := sdk.TracerProvider.Tracer("defaults_test").Start(t.Context(), "work")
		require.True(t, span.SpanContext().IsValid(), "spans get real IDs")
		span.End()

		var r log.Record
		r.SetSeverity(log.SeverityInfo)
		r.SetSeverityText("INFO")
		r.SetBody(attribute.StringValue("hello"))
		sdk.LoggerProvider.Logger("defaults_test").Emit(t.Context(), r)

		var lines []string
		for line := range strings.Lines(p.Stdout()) {
			var rec struct {
				Body string `json:"body"`
			}
			require.NoError(t, json.Unmarshal([]byte(line), &rec), "stdout line %q", line)
			lines = append(lines, rec.Body)
		}
		require.Equal(t, []string{"hello"}, lines)
		require.Empty(t, p.Stderr())
	})
}

func TestShutdownGracePeriod_fits_Kubernetes_termination_budget(t *testing.T) {
	// 30s terminationGracePeriodSeconds, less telemetry's 5s shutdown and a
	// margin for the process to exit.
	require.Equal(t, "20s", defaults.ShutdownGracePeriod.String())
}
