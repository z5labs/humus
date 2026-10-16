// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build conformance

package telemetry_test

import (
	"crypto/rand"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// leaks are OpenTelemetry environment variables the Go SDK and its
// exporters read for any option they are not given. Each is set to a value
// that, if it were read, would change what New builds in a way the test
// below sees.
var leaks = map[string]string{
	"OTEL_SERVICE_NAME":        "leaked",
	"OTEL_RESOURCE_ATTRIBUTES": "leaked.key=leaked,service.name=leaked",

	"OTEL_TRACES_SAMPLER":     "always_off",
	"OTEL_TRACES_SAMPLER_ARG": "0",

	"OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT":        "1",
	"OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT": "1",
	"OTEL_SPAN_EVENT_COUNT_LIMIT":            "0",
	"OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT":       "0",

	"OTEL_LOGRECORD_ATTRIBUTE_COUNT_LIMIT":        "1",
	"OTEL_LOGRECORD_ATTRIBUTE_VALUE_LENGTH_LIMIT": "1",

	"OTEL_BSP_SCHEDULE_DELAY":         "1",
	"OTEL_BSP_EXPORT_TIMEOUT":         "1",
	"OTEL_BSP_MAX_QUEUE_SIZE":         "1",
	"OTEL_BSP_MAX_EXPORT_BATCH_SIZE":  "1",
	"OTEL_BLRP_SCHEDULE_DELAY":        "1",
	"OTEL_BLRP_EXPORT_TIMEOUT":        "1",
	"OTEL_BLRP_MAX_QUEUE_SIZE":        "1",
	"OTEL_BLRP_MAX_EXPORT_BATCH_SIZE": "1",
	"OTEL_METRIC_EXPORT_INTERVAL":     "1",
	"OTEL_METRIC_EXPORT_TIMEOUT":      "1",
	"OTEL_METRICS_EXEMPLAR_FILTER":    "always_on",

	"OTEL_EXPORTER_OTLP_ENDPOINT":                       "http://leaked:1",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":                "http://leaked:1/traces",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":               "http://leaked:1/metrics",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":                  "http://leaked:1/logs",
	"OTEL_EXPORTER_OTLP_INSECURE":                       "false",
	"OTEL_EXPORTER_OTLP_CERTIFICATE":                    "/nonexistent/ca.pem",
	"OTEL_EXPORTER_OTLP_HEADERS":                        "x-leaked=1",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS":                 "x-leaked=1",
	"OTEL_EXPORTER_OTLP_METRICS_HEADERS":                "x-leaked=1",
	"OTEL_EXPORTER_OTLP_LOGS_HEADERS":                   "x-leaked=1",
	"OTEL_EXPORTER_OTLP_COMPRESSION":                    "gzip",
	"OTEL_EXPORTER_OTLP_TIMEOUT":                        "1",
	"OTEL_EXPORTER_OTLP_PROTOCOL":                       "http/json",
	"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "delta",
}

// TestConformance_environment sets the variables in leaks in the real
// environment and shows that none of them changes what New builds. It
// runs only with -tags conformance, on its own: it changes the process's
// environment, so it is not parallel.
func TestConformance_environment(t *testing.T) {
	for name, value := range leaks {
		t.Setenv(name, value)
	}

	for _, protocol := range []telemetry.Protocol{telemetry.ProtocolGRPC, telemetry.ProtocolHTTPProtobuf} {
		t.Run(string(protocol), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				testEnvironmentIsIgnored(t, protocol)
			})
		})
	}
}

func testEnvironmentIsIgnored(t *testing.T, protocol telemetry.Protocol) {
	host := humustest.NewHost(t)
	rcv := startReceiver(t, listen(t, host, "otlp:4317"), protocol)
	dialer := newCountingDialer(host)

	s := telemetry.DefaultSettings()
	s.ServiceName = "carts"
	s.Traces = telemetry.ExporterOTLP
	s.Metrics = telemetry.ExporterOTLP
	s.Logs = telemetry.ExporterOTLP
	s.OTLP.Endpoint = "otlp:4317"
	s.OTLP.Protocol = protocol
	tenant := rand.Text()
	s.OTLP.Headers = map[string]string{"x-tenant": tenant}

	sdk, err := telemetry.New(t.Context(), s, dialer.Dial, nil)
	require.NoError(t, err)

	long := strings.Repeat("x", 64)
	ctx, span := sdk.TracerProvider.Tracer("carts").Start(t.Context(), "create cart")
	span.SetAttributes(attribute.String("cart.id", long), attribute.Int("cart.items", 2))
	span.AddEvent("priced")
	span.End()
	counter, err := sdk.MeterProvider.Meter("carts").Int64Counter("carts.created")
	require.NoError(t, err)
	counter.Add(ctx, 1)
	var r log.Record
	r.SetSeverity(log.SeverityInfo)
	r.SetBody(attribute.StringValue("cart created"))
	r.AddAttributes(attribute.String("cart.id", long), attribute.Int("cart.items", 2))
	sdk.LoggerProvider.Logger("carts").Emit(ctx, r)

	require.True(t, span.SpanContext().IsSampled(), "OTEL_TRACES_SAMPLER is not read")

	// The defaults export logs after a second and spans after five; the
	// leaked schedules would have exported everything after a millisecond.
	time.Sleep(500 * time.Millisecond)
	synctest.Wait()
	_, _, _, requests := rcv.snapshot()
	require.Empty(t, requests, "OTEL_BSP_*, OTEL_BLRP_* and OTEL_METRIC_EXPORT_* are not read")

	require.NoError(t, sdk.Shutdown(t.Context()))

	spans, metrics, logs, requests := rcv.snapshot()
	want := map[string]string{"service.name": "carts"}

	require.Len(t, spans, 1)
	require.Equal(t, want, resourceAttributes(spans[0].GetResource()), "OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES are not read")
	got := spans[0].GetScopeSpans()[0].GetSpans()[0]
	require.Len(t, got.GetAttributes(), 2, "OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT is not read")
	require.Equal(t, long, got.GetAttributes()[0].GetValue().GetStringValue(), "OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT is not read")
	require.Len(t, got.GetEvents(), 1, "OTEL_SPAN_EVENT_COUNT_LIMIT is not read")

	require.Len(t, metrics, 1)
	require.Equal(t, want, resourceAttributes(metrics[0].GetResource()))
	sum := metrics[0].GetScopeMetrics()[0].GetMetrics()[0].GetSum()
	require.Equal(t, metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, sum.GetAggregationTemporality(),
		"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE is not read")

	require.Len(t, logs, 1)
	require.Equal(t, want, resourceAttributes(logs[0].GetResource()))
	rec := logs[0].GetScopeLogs()[0].GetLogRecords()[0]
	require.Len(t, rec.GetAttributes(), 2, "OTEL_LOGRECORD_ATTRIBUTE_COUNT_LIMIT is not read")
	require.Equal(t, long, rec.GetAttributes()[0].GetValue().GetStringValue(), "OTEL_LOGRECORD_ATTRIBUTE_VALUE_LENGTH_LIMIT is not read")

	require.Len(t, requests, 3)
	for _, req := range requests {
		require.Equal(t, []string{tenant}, req.headers["x-tenant"], "the %s request", req.signal)
		require.NotContains(t, req.headers, "x-leaked", "OTEL_EXPORTER_OTLP_HEADERS is not read")
		require.Empty(t, req.encoding, "OTEL_EXPORTER_OTLP_COMPRESSION is not read")
	}
	for _, addr := range dialer.Calls() {
		require.Equal(t, "otlp:4317", addr, "OTEL_EXPORTER_OTLP_ENDPOINT is not read")
	}
}
