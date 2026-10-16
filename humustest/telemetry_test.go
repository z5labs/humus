// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// build returns the SDK tel's Builder builds, shut down when the test ends.
func build(t *testing.T, tel *humustest.Telemetry) telemetry.SDK {
	t.Helper()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sdk.Shutdown(context.Background()) })
	return sdk
}

// emit logs one record with the given severity and body through sdk.
func emit(ctx context.Context, sdk telemetry.SDK, sev log.Severity, body string) {
	var r log.Record
	r.SetSeverity(sev)
	r.SetBody(attribute.StringValue(body))
	sdk.LoggerProvider.Logger("test").Emit(ctx, r)
}

// counterValue returns the sum of the int64 counter named name in rm.
func counterValue(t *testing.T, rm metricdata.ResourceMetrics, name string) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s is a %T", name, m.Data)
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	t.Fatalf("no metric named %s", name)
	return 0
}

func TestTelemetry_records_a_span_a_counter_and_a_log_record_inside_synctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		sdk := build(t, tel)

		ctx, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "work")
		counter, err := sdk.MeterProvider.Meter("test").Int64Counter("work.done")
		require.NoError(t, err)
		counter.Add(ctx, 3)
		emit(ctx, sdk, log.SeverityInfo, "working")

		require.Empty(t, tel.Spans(), "a span is recorded once it ends")
		span.End()

		spans := tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, "work", spans[0].Name())

		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		require.Equal(t, int64(3), counterValue(t, rm, "work.done"))

		logs := tel.Logs()
		require.Len(t, logs, 1)
		require.Equal(t, "working", logs[0].Body().AsString())
		require.Equal(t, log.SeverityInfo, logs[0].Severity())

		// synctest.Test fails if a goroutine started in the bubble is still
		// running when it returns; shutting down here lets it check.
		require.NoError(t, sdk.Shutdown(t.Context()))
	})
}

func TestTelemetry_a_record_logged_in_a_span_carries_its_trace_and_span_IDs(t *testing.T) {
	tel := humustest.NewTelemetry()
	sdk := build(t, tel)

	ctx, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "work")
	emit(ctx, sdk, log.SeverityInfo, "inside")
	span.End()
	emit(t.Context(), sdk, log.SeverityInfo, "outside")

	sc := span.SpanContext()
	require.True(t, sc.IsValid())
	require.True(t, sc.IsSampled(), "every span is sampled")

	logs := tel.Logs()
	require.Len(t, logs, 2)
	require.Equal(t, sc.TraceID(), logs[0].TraceID())
	require.Equal(t, sc.SpanID(), logs[0].SpanID())
	require.Equal(t, sc.TraceFlags(), logs[0].TraceFlags())
	require.False(t, logs[1].TraceID().IsValid(), "a record logged outside a span has no trace ID")
}

func TestTelemetry_what_was_recorded_before_Shutdown_is_returned_after_it(t *testing.T) {
	tel := humustest.NewTelemetry()
	sdk := build(t, tel)

	ctx, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "work")
	emit(ctx, sdk, log.SeverityInfo, "before")
	span.End()

	require.NoError(t, sdk.Shutdown(t.Context()))

	spans := tel.Spans()
	require.Len(t, spans, 1)
	require.Equal(t, "work", spans[0].Name())
	logs := tel.Logs()
	require.Len(t, logs, 1)
	require.Equal(t, "before", logs[0].Body().AsString())

	// Shutdown stopped the providers: nothing emitted after it is recorded.
	_, after := sdk.TracerProvider.Tracer("test").Start(t.Context(), "after")
	after.End()
	emit(t.Context(), sdk, log.SeverityInfo, "after")
	require.Len(t, tel.Spans(), 1)
	require.Len(t, tel.Logs(), 1)

	_, err := tel.Metrics(t.Context())
	require.Error(t, err, "the reader is shut down with its provider")
}

func TestTelemetry_records_every_severity(t *testing.T) {
	tel := humustest.NewTelemetry()
	sdk := build(t, tel)

	sevs := []log.Severity{log.SeverityTrace, log.SeverityDebug, log.SeverityInfo, log.SeverityWarn, log.SeverityError, log.SeverityFatal}
	for _, sev := range sevs {
		require.True(t, sdk.LoggerProvider.Logger("test").Enabled(t.Context(), log.EnabledParameters{Severity: sev}), sev.String())
		emit(t.Context(), sdk, sev, sev.String())
	}

	logs := tel.Logs()
	require.Len(t, logs, len(sevs))
	for i, sev := range sevs {
		require.Equal(t, sev, logs[i].Severity())
		require.Equal(t, sev.String(), logs[i].Body().AsString())
	}
}

func TestTelemetry_Spans_and_Logs_return_copies(t *testing.T) {
	tel := humustest.NewTelemetry()
	sdk := build(t, tel)

	_, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "work")
	span.End()
	var r log.Record
	r.SetBody(attribute.StringValue("original"))
	r.AddAttributes(attribute.String("k", "v"))
	sdk.LoggerProvider.Logger("test").Emit(t.Context(), r)

	spans := tel.Spans()
	spans[0] = nil
	logs := tel.Logs()
	logs[0].SetBody(attribute.StringValue("changed"))
	logs[0].AddAttributes(attribute.String("k2", "v2"))

	require.NotNil(t, tel.Spans()[0])
	again := tel.Logs()
	require.Equal(t, "original", again[0].Body().AsString())
	require.Equal(t, 1, again[0].AttributesLen())
}

func TestTelemetry_Builder_uses_W3C_trace_context_and_baggage(t *testing.T) {
	tel := humustest.NewTelemetry()
	sdk := build(t, tel)

	require.ElementsMatch(t, []string{"traceparent", "tracestate", "baggage"}, sdk.Propagator.Fields())

	member, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	bag, err := baggage.New(member)
	require.NoError(t, err)
	ctx, span := sdk.TracerProvider.Tracer("test").Start(baggage.ContextWithBaggage(t.Context(), bag), "work")
	defer span.End()

	carrier := propagation.MapCarrier{}
	sdk.Propagator.Inject(ctx, carrier)
	require.NotEmpty(t, carrier.Get("traceparent"))
	require.Equal(t, "tenant=acme", carrier.Get("baggage"))

	got := sdk.Propagator.Extract(context.Background(), carrier)
	require.Equal(t, span.SpanContext().TraceID(), trace.SpanContextFromContext(got).TraceID())
}

func TestTelemetry_Builder_returns_the_same_SDK_each_call(t *testing.T) {
	tel := humustest.NewTelemetry()
	one := build(t, tel)
	two := build(t, tel)

	_, span := one.TracerProvider.Tracer("test").Start(t.Context(), "one")
	span.End()
	_, span = two.TracerProvider.Tracer("test").Start(t.Context(), "two")
	span.End()

	require.Len(t, tel.Spans(), 2)
}

func TestTelemetry_is_safe_for_concurrent_use(t *testing.T) {
	tel := humustest.NewTelemetry()
	sdk := build(t, tel)
	counter, err := sdk.MeterProvider.Meter("test").Int64Counter("work.done")
	require.NoError(t, err)

	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			ctx, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "work")
			counter.Add(ctx, 1)
			emit(ctx, sdk, log.SeverityInfo, "working")
			span.End()
		})
		wg.Go(func() {
			_ = tel.Spans()
			_ = tel.Logs()
			if _, err := tel.Metrics(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	require.Len(t, tel.Spans(), n)
	require.Len(t, tel.Logs(), n)
	rm, err := tel.Metrics(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(n), counterValue(t, rm, "work.done"))
}
