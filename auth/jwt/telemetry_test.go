// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// countingPropagator is a TextMapPropagator that counts the calls to its
// Extract and Inject, and propagates nothing.
type countingPropagator struct {
	calls atomic.Int64
}

func (p *countingPropagator) Inject(context.Context, propagation.TextMapCarrier) {
	p.calls.Add(1)
}

func (p *countingPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	p.calls.Add(1)
	return ctx
}

func (p *countingPropagator) Fields() []string { return []string{"traceparent"} }

// countingExporter is a log Exporter that counts the records it is given.
type countingExporter struct {
	records atomic.Int64
}

func (e *countingExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.records.Add(int64(len(records)))
	return nil
}

func (e *countingExporter) Shutdown(context.Context) error { return nil }

func (e *countingExporter) ForceFlush(context.Context) error { return nil }

// This test sets OpenTelemetry's globals, so it doesn't run in parallel,
// and it restores them when it ends.
func TestVerifier_uses_no_OpenTelemetry_globals_and_creates_no_instrument(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}
	logs := &countingExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show auth/jwt never uses them, as the authentication proposal, #870, requires.
	{
		prevTP, prevMP, prevProp, prevLP := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator(), otel.GetLoggerProvider()
		t.Cleanup(func() {
			otel.SetTracerProvider(prevTP)
			otel.SetMeterProvider(prevMP)
			otel.SetTextMapPropagator(prevProp)
			otel.SetLoggerProvider(prevLP)
		})
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
		otel.SetLoggerProvider(lp)
	}

	synctest.Test(t, func(t *testing.T) {
		k1 := newSigner(t, "k1")
		keys := &keySet{}
		keys.add(k1.public())
		v, tel := newVerifier(t, keys)

		_, err := v.Verify(t.Context(), k1.sign(t, good("alice", 3*time.Hour)))
		require.NoError(t, err)
		keys.fail(errors.New("the secret store is down"))
		time.Sleep(time.Hour + time.Second)
		_, err = v.Verify(t.Context(), k1.sign(t, good("alice", time.Hour)))
		require.NoError(t, err)
		_, err = v.Verify(t.Context(), newSigner(t, "k2").sign(t, good("mallory", time.Hour)))
		require.Error(t, err)

		records := tel.Logs()
		require.Len(t, records, 1, "the Setup's LoggerProvider recorded no record")
		require.Equal(t, "github.com/z5labs/humus/auth/jwt", records[0].InstrumentationScope().Name)
		records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
			require.NotContains(t, kv.Value.String(), "alice", "a record carries the principal")
			return true
		})

		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		require.Empty(t, rm.ScopeMetrics, "auth/jwt created an instrument")
		require.Empty(t, tel.Spans(), "auth/jwt started a span")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
	require.Zero(t, logs.records.Load(), "the global LoggerProvider recorded records")
}
