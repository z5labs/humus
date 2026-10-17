// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// The scopes the Service's metrics are recorded under: its own, and kotel's.
const (
	serviceScope = "github.com/z5labs/humus/queue/kafka"
	kotelScope   = "github.com/twmb/franz-go/plugin/kotel"
)

// processSpan is the name of the span of a record of topic.
const processSpan = "process " + topic

// The trace context a producer injects, from the W3C Trace Context
// specification's example of a traceparent header.
const (
	producerTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	producerSpanID  = "00f067aa0ba902b7"
)

// producerHeaders returns the headers a producer whose context carries sc
// and a baggage member tenant=acme injects with the W3C trace context and
// baggage propagator.
func producerHeaders(t *testing.T, sc trace.SpanContext) []kgo.RecordHeader {
	t.Helper()
	m, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(m)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(trace.ContextWithRemoteSpanContext(t.Context(), sc), b)

	carrier := propagation.MapCarrier{}
	propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}).Inject(ctx, carrier)
	require.NotEmpty(t, carrier.Get("traceparent"))
	require.NotEmpty(t, carrier.Get("baggage"))

	keys := carrier.Keys()
	slices.Sort(keys)
	headers := make([]kgo.RecordHeader, 0, len(keys))
	for _, k := range keys {
		headers = append(headers, kgo.RecordHeader{Key: k, Value: []byte(carrier.Get(k))})
	}
	return headers
}

// producerSpanContext is the span context producerHeaders injects.
func producerSpanContext(t *testing.T) trace.SpanContext {
	t.Helper()
	tid, err := trace.TraceIDFromHex(producerTraceID)
	require.NoError(t, err)
	sid, err := trace.SpanIDFromHex(producerSpanID)
	require.NoError(t, err)
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
}

// consumeAll runs an App whose only Service consumes s with p, recording
// its telemetry in tel, until every record is processed, then stops it
// with SIGTERM and returns its exit status's code.
func consumeAll(t *testing.T, h *humustest.Host, tel *humustest.Telemetry, s kafka.Settings, p queue.ProcessorFunc[kafka.Message]) int {
	t.Helper()
	proc, status := humustest.Start(t, consumes(s, p),
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
	synctest.Wait()
	proc.Signal(syscall.SIGTERM)
	st := <-status
	require.Empty(t, proc.Stderr())
	return st.Code()
}

// spansNamed returns the spans of tel named name.
func spansNamed(tel *humustest.Telemetry, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range tel.Spans() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// spanAttrs returns span's attributes by key.
func spanAttrs(span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := make(map[attribute.Key]attribute.Value)
	for _, kv := range span.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

// spanAt returns the span of the record at offset in spans.
func spanAt(t *testing.T, spans []sdktrace.ReadOnlySpan, offset int64) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if spanAttrs(s)["messaging.kafka.offset"].AsInt64() == offset {
			return s
		}
	}
	t.Fatalf("no span of the record at offset %d", offset)
	return nil
}

// metricNamed returns the metric name recorded under scope, failing the
// test when there is none.
func metricNamed(t *testing.T, tel *humustest.Telemetry, scope, name string) metricdata.Metrics {
	t.Helper()
	rm, err := tel.Metrics(t.Context())
	require.NoError(t, err)
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != scope {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("no metric %s under the scope %s", name, scope)
	return metricdata.Metrics{}
}

// metricNames returns the names of the metrics recorded under scope,
// sorted.
func metricNames(t *testing.T, tel *humustest.Telemetry, scope string) []string {
	t.Helper()
	rm, err := tel.Metrics(t.Context())
	require.NoError(t, err)
	var names []string
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != scope {
			continue
		}
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}
	slices.Sort(names)
	return names
}

// sum adds up the data points of the int64 counter name under scope.
func sum(t *testing.T, tel *humustest.Telemetry, scope, name string) int64 {
	t.Helper()
	data, ok := metricNamed(t, tel, scope, name).Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s is an int64 sum", name)
	var n int64
	for _, dp := range data.DataPoints {
		n += dp.Value
	}
	return n
}

// setAttrs returns the attributes of set by key, as strings.
func setAttrs(set attribute.Set) map[string]string {
	out := make(map[string]string)
	for _, kv := range set.ToSlice() {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// histogramPoints returns the data points of the float64 histogram name
// under the Service's scope.
func histogramPoints(t *testing.T, tel *humustest.Telemetry, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	data, ok := metricNamed(t, tel, serviceScope, name).Data.(metricdata.Histogram[float64])
	require.True(t, ok, "%s is a float64 histogram", name)
	return data.DataPoints
}

// partitionAttrs are the attributes every instrument of the Service records
// for partition 0 of topic.
var partitionAttrs = map[string]string{
	"messaging.system":                   "kafka",
	"messaging.consumer.group.name":      group,
	"messaging.destination.name":         topic,
	"messaging.destination.partition.id": "0",
}

// with returns partitionAttrs with kv added.
func with(kv ...string) map[string]string {
	out := make(map[string]string, len(partitionAttrs)+len(kv)/2)
	for k, v := range partitionAttrs {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestService_continues_the_producers_trace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		sc := producerSpanContext(t)
		produceRecords(t, h, c, []*kgo.Record{
			{Topic: topic, Key: []byte("key-0"), Value: []byte("0"), Headers: producerHeaders(t, sc)},
			{Topic: topic, Key: []byte("key-1"), Value: []byte("1")},
		})

		var (
			mu      sync.Mutex
			inSpan  = map[int64]trace.SpanContext{}
			tenants = map[int64]string{}
		)
		tel := humustest.NewTelemetry()
		code := consumeAll(t, h, tel, settings(c.ListenAddrs()...), func(ctx context.Context, m kafka.Message) error {
			mu.Lock()
			defer mu.Unlock()
			inSpan[m.Offset] = trace.SpanContextFromContext(ctx)
			tenants[m.Offset] = baggage.FromContext(ctx).Member("tenant").Value()
			return nil
		})
		require.Zero(t, code)

		spans := spansNamed(tel, processSpan)
		require.Len(t, spans, 2)

		traced := spanAt(t, spans, 0)
		require.Equal(t, trace.SpanKindConsumer, traced.SpanKind())
		require.True(t, traced.Parent().IsRemote(), "the parent is the producer's, from the headers")
		require.Equal(t, sc.TraceID(), traced.Parent().TraceID())
		require.Equal(t, sc.SpanID(), traced.Parent().SpanID())
		require.Equal(t, sc.TraceID(), traced.SpanContext().TraceID(), "the record continues the producer's trace")
		require.Equal(t, traced.SpanContext(), inSpan[0], "Process's context carries the record's span")
		require.Equal(t, "acme", tenants[0], "the producer's baggage is on Process's context")

		untraced := spanAt(t, spans, 1)
		require.False(t, untraced.Parent().IsValid(), "a record with no headers starts a new trace")
		require.NotEqual(t, sc.TraceID(), untraced.SpanContext().TraceID())
		require.Equal(t, untraced.SpanContext(), inSpan[1])
		require.Empty(t, tenants[1])
	})
}

func TestService_record_span_attributes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 2)

		tel := humustest.NewTelemetry()
		require.Zero(t, consumeAll(t, h, tel, settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error { return nil }))

		span := spanAt(t, spansNamed(tel, processSpan), 1)
		a := spanAttrs(span)
		require.Equal(t, "kafka", a["messaging.system"].AsString())
		require.Equal(t, "process", a["messaging.operation.type"].AsString())
		require.Equal(t, "process", a["messaging.operation.name"].AsString())
		require.Equal(t, topic, a["messaging.destination.name"].AsString())
		require.Equal(t, "0", a["messaging.destination.partition.id"].AsString())
		require.Equal(t, group, a["messaging.consumer.group.name"].AsString())
		require.Equal(t, int64(1), a["messaging.kafka.offset"].AsInt64())
		require.Len(t, a, 7, "no other attribute: %v", a)
		for k, v := range a {
			require.NotContains(t, string(k), "key", "the record's key is not recorded")
			require.NotEqual(t, "key-1", v.String(), "the record's key is not recorded, as %s", k)
		}
		require.Equal(t, codes.Unset, span.Status().Code)
		require.Equal(t, serviceScope, span.InstrumentationScope().Name)
	})
}

func TestService_records_franz_go_broker_metrics_with_kotel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 2)
		produceTo(t, h, c, map[int32]int{0: 5, 1: 5})

		tel := humustest.NewTelemetry()
		require.Zero(t, consumeAll(t, h, tel, settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error { return nil }))

		require.Len(t, tel.Spans(), 10)
		require.Len(t, spansNamed(tel, processSpan), 10, "ten records over two partitions are ten spans")
		require.Equal(t, int64(10), sum(t, tel, kotelScope, "messaging.kafka.fetch_records.count"))

		fetched, ok := metricNamed(t, tel, kotelScope, "messaging.kafka.fetch_records.count").Data.(metricdata.Sum[int64])
		require.True(t, ok)
		for _, dp := range fetched.DataPoints {
			a := setAttrs(dp.Attributes)
			require.NotEmpty(t, a["node_id"])
			require.Equal(t, topic, a["topic"])
		}
		connects, ok := metricNamed(t, tel, kotelScope, "messaging.kafka.connects.count").Data.(metricdata.Sum[int64])
		require.True(t, ok)
		for _, dp := range connects.DataPoints {
			require.NotEmpty(t, setAttrs(dp.Attributes)["node_id"])
		}
	})
}

// TestService_kotel_instrument_names lists the names of the kotel
// instruments a consumer records. humus renames none of them, so a kotel
// upgrade that renames one is a breaking change to queue/kafka, and fails
// here instead of merging.
func TestService_kotel_instrument_names(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 3)

		tel := humustest.NewTelemetry()
		require.Zero(t, consumeAll(t, h, tel, settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error { return nil }))

		require.Equal(t, []string{
			"messaging.kafka.connects.count",
			"messaging.kafka.disconnects.count",
			"messaging.kafka.fetch_bytes.count",
			"messaging.kafka.fetch_records.count",
			"messaging.kafka.read_bytes.count",
			"messaging.kafka.read_errors.count",
			"messaging.kafka.write_bytes",
		}, metricNames(t, tel, kotelScope))
		require.Equal(t, []string{
			"messaging.client.consumed.messages",
			"messaging.client.operation.duration",
			"messaging.process.duration",
		}, metricNames(t, tel, serviceScope))
	})
}

func TestService_records_a_processing_error(t *testing.T) {
	for _, d := range []queue.Delivery{queue.AtLeastOnce, queue.AtMostOnce} {
		t.Run(string(d), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				c := cluster(t, h, 1)
				produce(t, h, c, 3)

				s := settings(c.ListenAddrs()...)
				s.Delivery = d
				bad := errors.New("cannot process")
				tel := humustest.NewTelemetry()
				proc, status := humustest.Start(t, consumes(s, func(_ context.Context, m kafka.Message) error {
					if m.Offset == 1 {
						return bad
					}
					return nil
				}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
				synctest.Wait()
				proc.Signal(syscall.SIGTERM)
				st := <-status
				if d == queue.AtLeastOnce {
					require.ErrorIs(t, st.Cause(), bad)
				} else {
					require.Zero(t, st.Code(), st.Cause())
				}

				spans := spansNamed(tel, processSpan)
				failed := spanAt(t, spans, 1)
				require.Equal(t, codes.Error, failed.Status().Code)
				require.Equal(t, "cannot process", failed.Status().Description)
				require.Equal(t, "*errors.errorString", spanAttrs(failed)["error.type"].AsString())
				var exception bool
				for _, e := range failed.Events() {
					if e.Name == "exception" {
						exception = true
					}
				}
				require.True(t, exception, "the error is recorded on the span as an exception event")
				require.Equal(t, codes.Unset, spanAt(t, spans, 0).Status().Code)

				var failures, successes uint64
				for _, dp := range histogramPoints(t, tel, "messaging.process.duration") {
					a := setAttrs(dp.Attributes)
					if _, ok := a["error.type"]; ok {
						require.Equal(t, with("messaging.operation.name", "process", "error.type", "*errors.errorString"), a)
						failures += dp.Count
						continue
					}
					successes += dp.Count
				}
				require.Equal(t, uint64(1), failures)
				require.NotZero(t, successes)
			})
		})
	}
}

func TestService_records_the_messaging_instruments(t *testing.T) {
	t.Run("each record and each commit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produce(t, h, c, 3)

			tel := humustest.NewTelemetry()
			require.Zero(t, consumeAll(t, h, tel, settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error { return nil }))

			consumed, ok := metricNamed(t, tel, serviceScope, "messaging.client.consumed.messages").Data.(metricdata.Sum[int64])
			require.True(t, ok)
			require.True(t, consumed.IsMonotonic)
			require.Len(t, consumed.DataPoints, 1)
			require.Equal(t, int64(3), consumed.DataPoints[0].Value)
			require.Equal(t, with("messaging.operation.name", "process"), setAttrs(consumed.DataPoints[0].Attributes))

			processed := histogramPoints(t, tel, "messaging.process.duration")
			require.Len(t, processed, 1)
			require.Equal(t, uint64(3), processed[0].Count)
			require.Equal(t, with("messaging.operation.name", "process"), setAttrs(processed[0].Attributes))

			commits := histogramPoints(t, tel, "messaging.client.operation.duration")
			require.Len(t, commits, 1)
			require.NotZero(t, commits[0].Count)
			require.Equal(t, with("messaging.operation.name", "commit"), setAttrs(commits[0].Attributes))
		})
	})

	t.Run("a commit that fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produce(t, h, c, 3)
			answerOffsetCommit(c, kerr.UnknownMemberID.Code, false)

			tel := humustest.NewTelemetry()
			require.Zero(t, consumeAll(t, h, tel, settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error { return nil }))

			var failed, succeeded bool
			for _, dp := range histogramPoints(t, tel, "messaging.client.operation.duration") {
				a := setAttrs(dp.Attributes)
				require.Equal(t, "commit", a["messaging.operation.name"])
				if a["error.type"] != "" {
					failed = true
					require.Equal(t, with("messaging.operation.name", "commit", "error.type", a["error.type"]), a)
					continue
				}
				succeeded = true
			}
			require.True(t, failed, "the refused commit is recorded with error.type")
			require.True(t, succeeded, "the later commit is recorded without it")
		})
	})
}

// countingPropagator counts its calls, so a test can show it was never
// used.
type countingPropagator struct {
	calls atomic.Int64
}

func (p *countingPropagator) Inject(context.Context, propagation.TextMapCarrier) { p.calls.Add(1) }

func (p *countingPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	p.calls.Add(1)
	return ctx
}

func (p *countingPropagator) Fields() []string { return []string{"traceparent"} }

// This test sets OpenTelemetry's globals, so it doesn't run in parallel,
// and it restores them when it ends.
func TestService_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the Service never uses them, kotel's Meter included, as the queue proposal, #683, requires.
	{
		prevTP, prevMP, prevProp := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
		t.Cleanup(func() {
			otel.SetTracerProvider(prevTP)
			otel.SetMeterProvider(prevMP)
			otel.SetTextMapPropagator(prevProp)
		})
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
	}

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produceRecords(t, h, c, []*kgo.Record{
			{Topic: topic, Value: []byte("0"), Headers: producerHeaders(t, producerSpanContext(t))},
		})

		tel := humustest.NewTelemetry()
		require.Zero(t, consumeAll(t, h, tel, settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error { return nil }))
		require.Len(t, spansNamed(tel, processSpan), 1, "the Setup's TracerProvider recorded the span")
		require.NotEmpty(t, metricNames(t, tel, kotelScope), "the Setup's MeterProvider recorded kotel's instruments")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
}
