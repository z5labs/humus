// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
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
func TestNewHandler_uses_no_OpenTelemetry_globals_and_creates_no_instrument(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}
	logs := &countingExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show auth never uses them, as the authentication proposal, #870, requires.
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
		bearerScheme := auth.Bearer("bearer", func(context.Context, string) (user, error) {
			return user{}, errors.New("the directory is down")
		})
		s := serve(t, func(s *humus.Setup) (http.Handler, error) {
			return auth.NewHandler(s, principals(nil), answers, auth.Require(bearerScheme))
		}, nil)

		require.Equal(t, http.StatusServiceUnavailable, s.get("/carts", bearer(rand.Text())).status)
		require.Equal(t, http.StatusUnauthorized, s.get("/carts").status)
		s.stop()
		require.Len(t, records(s.tel), 1, "the Setup's LoggerProvider recorded no record")
		require.Len(t, serverSpans(s.tel), 2, "the Setup's TracerProvider recorded no span")

		rm, err := s.tel.Metrics(t.Context())
		require.NoError(t, err)
		for _, sm := range rm.ScopeMetrics {
			require.NotEqual(t, loggerName, sm.Scope.Name, "auth created an instrument")
		}
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
	require.Zero(t, logs.records.Load(), "the global LoggerProvider recorded records")
}
