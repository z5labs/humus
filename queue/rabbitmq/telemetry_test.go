// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
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

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// serviceScope is the scope the Service's spans, instruments and logs are
// recorded under.
const serviceScope = "github.com/z5labs/humus/queue/rabbitmq"

// processSpan is the name of the span of a delivery from queueName.
const processSpan = "process " + queueName

// The trace context a publisher injects, from the W3C Trace Context
// specification's example of a traceparent header.
const (
	publisherTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	publisherSpanID  = "00f067aa0ba902b7"
	traceparent      = "00-" + publisherTraceID + "-" + publisherSpanID + "-01"
)

// publisherSpanContext is the span context traceparent carries.
func publisherSpanContext(t *testing.T) trace.SpanContext {
	t.Helper()
	tid, err := trace.TraceIDFromHex(publisherTraceID)
	require.NoError(t, err)
	sid, err := trace.SpanIDFromHex(publisherSpanID)
	require.NoError(t, err)
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true})
}

// publisherHeaders returns the headers a publisher whose context carries
// publisherSpanContext and a baggage member tenant=acme injects with the
// W3C trace context and baggage propagator.
func publisherHeaders(t *testing.T) amqp.Table {
	t.Helper()
	m, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(m)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(trace.ContextWithRemoteSpanContext(t.Context(), publisherSpanContext(t)), b)

	carrier := propagation.MapCarrier{}
	propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}).Inject(ctx, carrier)
	require.Equal(t, traceparent, carrier.Get("traceparent"))
	headers := amqp.Table{}
	for _, k := range carrier.Keys() {
		headers[k] = carrier.Get(k)
	}
	return headers
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

// spanOf returns the span of the delivery with message ID id.
func spanOf(t *testing.T, spans []sdktrace.ReadOnlySpan, id string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if spanAttrs(s)["messaging.message.id"].AsString() == id {
			return s
		}
	}
	t.Fatalf("no span of the message %s", id)
	return nil
}

// metricNamed returns the metric name recorded under the Service's scope,
// failing the test when there is none.
func metricNamed(t *testing.T, tel *humustest.Telemetry, name string) metricdata.Metrics {
	t.Helper()
	rm, err := tel.Metrics(t.Context())
	require.NoError(t, err)
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != serviceScope {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("no metric %s under the scope %s", name, serviceScope)
	return metricdata.Metrics{}
}

// setAttrs returns the attributes of set by key, as strings.
func setAttrs(set attribute.Set) map[string]string {
	out := make(map[string]string)
	for _, kv := range set.ToSlice() {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// instrumentAttrs are the attributes both instruments record for a
// delivery to the destination orders, on the default exchange, from
// settings().
var instrumentAttrs = map[string]string{
	"messaging.system":           "rabbitmq",
	"messaging.operation.name":   "process",
	"messaging.destination.name": queueName,
	"server.address":             "rabbit",
	"server.port":                "5672",
}

// with returns instrumentAttrs with kv added.
func with(kv ...string) map[string]string {
	out := make(map[string]string, len(instrumentAttrs)+len(kv)/2)
	for k, v := range instrumentAttrs {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// inSpans records the span context and the baggage of each Process's
// context, by message ID.
type inSpans struct {
	mu      sync.Mutex
	spans   map[string]trace.SpanContext
	tenants map[string]string
}

func newInSpans() *inSpans {
	return &inSpans{spans: map[string]trace.SpanContext{}, tenants: map[string]string{}}
}

func (s *inSpans) processor(err error) queue.ProcessorFunc[rabbitmq.Message] {
	return func(ctx context.Context, m rabbitmq.Message) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.spans[m.MessageID] = trace.SpanContextFromContext(ctx)
		s.tenants[m.MessageID] = baggage.FromContext(ctx).Member("tenant").Value()
		return err
	}
}

func (s *inSpans) span(id string) trace.SpanContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spans[id]
}

func (s *inSpans) tenant(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenants[id]
}

func TestService_continues_the_publishers_trace_with_every_attribute(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		in := newInSpans()
		u := newUnit(t, settings(), in.processor(nil))
		served := u.serve(t.Context())
		u.ch.send(amqp.Delivery{
			Headers:       publisherHeaders(t),
			RoutingKey:    queueName,
			MessageId:     "traced",
			CorrelationId: "conversation-1",
			Body:          []byte("body"),
		})
		synctest.Wait()

		spans := spansNamed(u.tel, processSpan)
		require.Len(t, spans, 1)
		span := spans[0]
		sc := publisherSpanContext(t)
		require.Equal(t, trace.SpanKindConsumer, span.SpanKind())
		require.True(t, span.Parent().IsRemote(), "the parent is the publisher's, from the headers")
		require.Equal(t, sc.TraceID(), span.Parent().TraceID())
		require.Equal(t, sc.SpanID(), span.Parent().SpanID())
		require.Equal(t, sc.TraceID(), span.SpanContext().TraceID(), "the delivery continues the publisher's trace")
		require.Equal(t, span.SpanContext(), in.span("traced"), "Process's context carries the delivery's span")
		require.Equal(t, "acme", in.tenant("traced"), "the publisher's baggage is on Process's context")
		require.Equal(t, serviceScope, span.InstrumentationScope().Name)
		require.Equal(t, codes.Unset, span.Status().Code)

		a := spanAttrs(span)
		require.Equal(t, "rabbitmq", a["messaging.system"].AsString())
		require.Equal(t, "process", a["messaging.operation.type"].AsString())
		require.Equal(t, "process", a["messaging.operation.name"].AsString())
		require.Equal(t, queueName, a["messaging.destination.name"].AsString())
		require.Equal(t, queueName, a["messaging.rabbitmq.destination.routing_key"].AsString())
		require.Equal(t, int64(1), a["messaging.rabbitmq.message.delivery_tag"].AsInt64())
		require.Equal(t, "traced", a["messaging.message.id"].AsString())
		require.Equal(t, "conversation-1", a["messaging.message.conversation_id"].AsString())
		require.Equal(t, "rabbit", a["server.address"].AsString())
		require.Equal(t, int64(5672), a["server.port"].AsInt64())
		require.Len(t, a, 10, "no other attribute: %v", a)

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestService_leaves_out_the_attributes_a_delivery_has_no_value_for(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), func(context.Context, rabbitmq.Message) error { return nil })
		served := u.serve(t.Context())
		// Consumed straight from the queue, as basic.get or a shovel can
		// leave it: no routing key, no message ID and no correlation ID.
		u.ch.send(amqp.Delivery{})
		synctest.Wait()

		spans := spansNamed(u.tel, processSpan)
		require.Len(t, spans, 1)
		a := spanAttrs(spans[0])
		for _, k := range []attribute.Key{
			"messaging.rabbitmq.destination.routing_key",
			"messaging.message.id",
			"messaging.message.conversation_id",
		} {
			require.NotContains(t, a, k)
		}
		require.Equal(t, queueName, a["messaging.destination.name"].AsString())
		require.Len(t, a, 7, "no other attribute: %v", a)

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestService_a_delivery_with_no_trace_context_starts_a_new_trace(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		in := newInSpans()
		u := newUnit(t, settings(), in.processor(nil))

		// Serve's context carries a span and baggage of its own, neither of
		// which a delivery's span or Process's context takes.
		m, err := baggage.NewMember("tenant", "serve")
		require.NoError(t, err)
		b, err := baggage.New(m)
		require.NoError(t, err)
		ctx := baggage.ContextWithBaggage(trace.ContextWithSpanContext(t.Context(), publisherSpanContext(t)), b)
		served := u.serve(ctx)

		u.ch.send(amqp.Delivery{MessageId: "untraced", RoutingKey: queueName})
		synctest.Wait()

		span := spanOf(t, spansNamed(u.tel, processSpan), "untraced")
		require.False(t, span.Parent().IsValid(), "a delivery with no trace context starts a new trace")
		require.NotEqual(t, publisherSpanContext(t).TraceID(), span.SpanContext().TraceID(), "Serve's span is not its parent")
		require.Equal(t, span.SpanContext(), in.span("untraced"))
		require.Empty(t, in.tenant("untraced"), "Serve's baggage is not on Process's context")

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestService_ignores_a_trace_context_header_that_is_not_a_string(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		in := newInSpans()
		u := newUnit(t, settings(), in.processor(nil))
		served := u.serve(t.Context())
		u.ch.send(
			amqp.Delivery{MessageId: "bytes", RoutingKey: queueName, Headers: amqp.Table{"traceparent": []byte(traceparent)}},
			amqp.Delivery{MessageId: "table", RoutingKey: queueName, Headers: amqp.Table{"traceparent": amqp.Table{"v": traceparent}}},
			amqp.Delivery{MessageId: "number", RoutingKey: queueName, Headers: amqp.Table{"traceparent": int32(1)}},
		)
		synctest.Wait()

		spans := spansNamed(u.tel, processSpan)
		require.Len(t, spans, 3)
		for _, id := range []string{"bytes", "table", "number"} {
			span := spanOf(t, spans, id)
			require.False(t, span.Parent().IsValid(), "%s's traceparent is not a string, so it is ignored", id)
			require.NotEqual(t, publisherSpanContext(t).TraceID(), span.SpanContext().TraceID())
		}

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestService_records_a_processing_error_on_its_span(t *testing.T) {
	t.Parallel()

	for name, delivery := range map[string]queue.Delivery{"AtLeastOnce": queue.AtLeastOnce, "AtMostOnce": queue.AtMostOnce} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				bad := errors.New("cannot process")
				s := settings()
				s.Delivery = delivery
				u := newUnit(t, s, func(context.Context, rabbitmq.Message) error { return bad })
				served := u.serve(t.Context())
				u.ch.send(amqp.Delivery{MessageId: "failed", RoutingKey: queueName, Headers: publisherHeaders(t)})
				synctest.Wait()

				spans := spansNamed(u.tel, processSpan)
				require.Len(t, spans, 1)
				span := spans[0]
				require.Equal(t, codes.Error, span.Status().Code)
				require.Equal(t, "cannot process", span.Status().Description)
				require.Equal(t, "*errors.errorString", spanAttrs(span)["error.type"].AsString())
				require.Len(t, span.Events(), 1)
				require.Equal(t, "exception", span.Events()[0].Name)

				if delivery == queue.AtMostOnce {
					failures := records(u.tel.Logs(), "processing a message failed")
					require.Len(t, failures, 1)
					require.Equal(t, span.SpanContext().TraceID(), failures[0].TraceID(), "the ERROR record carries the delivery's span")
					require.Equal(t, span.SpanContext().SpanID(), failures[0].SpanID())
					require.NoError(t, u.svc.Shutdown(t.Context()))
				}
				err := <-served
				if delivery == queue.AtLeastOnce {
					require.ErrorIs(t, err, bad)
				} else {
					require.NoError(t, err)
				}
			})
		})
	}
}

func TestService_records_the_consumed_messages_and_the_process_duration(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		bad := errors.New("cannot process")
		s := settings()
		s.Delivery = queue.AtMostOnce
		u := newUnit(t, s, func(_ context.Context, m rabbitmq.Message) error {
			// Each Process takes a second, which synctest's clock makes
			// exact.
			time.Sleep(time.Second)
			if m.MessageID == "failed" {
				return bad
			}
			return nil
		})
		served := u.serve(t.Context())
		u.ch.send(
			amqp.Delivery{MessageId: "0", RoutingKey: queueName},
			amqp.Delivery{MessageId: "1", RoutingKey: queueName},
			amqp.Delivery{MessageId: "failed", RoutingKey: queueName},
		)
		// Long enough for all three to be processed, one after another.
		time.Sleep(time.Minute)
		synctest.Wait()
		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)

		consumed, ok := metricNamed(t, u.tel, "messaging.client.consumed.messages").Data.(metricdata.Sum[int64])
		require.True(t, ok, "messaging.client.consumed.messages is an int64 sum")
		require.True(t, consumed.IsMonotonic)
		require.Len(t, consumed.DataPoints, 1)
		require.Equal(t, int64(3), consumed.DataPoints[0].Value)
		require.Equal(t, instrumentAttrs, setAttrs(consumed.DataPoints[0].Attributes))

		durations, ok := metricNamed(t, u.tel, "messaging.process.duration").Data.(metricdata.Histogram[float64])
		require.True(t, ok, "messaging.process.duration is a float64 histogram")
		require.Len(t, durations.DataPoints, 2, "one series without error.type, one with")
		byErr := map[string]metricdata.HistogramDataPoint[float64]{}
		for _, dp := range durations.DataPoints {
			byErr[setAttrs(dp.Attributes)["error.type"]] = dp
		}
		succeeded := byErr[""]
		require.Equal(t, instrumentAttrs, setAttrs(succeeded.Attributes))
		require.Equal(t, uint64(2), succeeded.Count)
		require.InDelta(t, 2.0, succeeded.Sum, 1e-9, "each Process took a second")

		failed := byErr["*errors.errorString"]
		require.Equal(t, with("error.type", "*errors.errorString"), setAttrs(failed.Attributes))
		require.Equal(t, uint64(1), failed.Count)
		require.InDelta(t, 1.0, failed.Sum, 1e-9)
	})
}

func TestService_names_the_destination_as_semconv_defines_it_for_RabbitMQ(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), func(context.Context, rabbitmq.Message) error { return nil })
		served := u.serve(t.Context())
		u.ch.send(
			// Published to orders on the default exchange.
			amqp.Delivery{MessageId: "default", RoutingKey: queueName},
			// Published to a named exchange with a routing key bound to
			// orders.
			amqp.Delivery{MessageId: "named", Exchange: "shop", RoutingKey: "order.created"},
			// Published to a named exchange with the queue's name as its
			// routing key.
			amqp.Delivery{MessageId: "same", Exchange: "shop", RoutingKey: queueName},
		)
		synctest.Wait()
		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)

		spans := spansNamed(u.tel, processSpan)
		want := map[string]string{
			"default": "orders",
			"named":   "shop:order.created:orders",
			"same":    "shop:orders",
		}
		for id, name := range want {
			require.Equal(t, name, spanAttrs(spanOf(t, spans, id))["messaging.destination.name"].AsString(), id)
		}

		consumed, ok := metricNamed(t, u.tel, "messaging.client.consumed.messages").Data.(metricdata.Sum[int64])
		require.True(t, ok)
		var names []string
		for _, dp := range consumed.DataPoints {
			names = append(names, setAttrs(dp.Attributes)["messaging.destination.name"])
		}
		require.ElementsMatch(t, []string{"orders", "shop:order.created:orders", "shop:orders"}, names,
			"the instruments carry the same destination name, a series each")
	})
}

func TestService_records_no_body(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		body := "body-" + rand.Text()
		bad := errors.New("cannot process")
		s := settings()
		s.Delivery = queue.AtMostOnce
		u := newUnit(t, s, func(_ context.Context, m rabbitmq.Message) error {
			require.Equal(t, body, string(m.Body), "Process is given the body")
			if m.MessageID == "failed" {
				return bad
			}
			return nil
		})
		served := u.serve(t.Context())
		u.ch.send(
			amqp.Delivery{MessageId: "ok", RoutingKey: queueName, Body: []byte(body), Headers: publisherHeaders(t)},
			amqp.Delivery{MessageId: "failed", RoutingKey: queueName, Body: []byte(body)},
		)
		synctest.Wait()
		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)

		spans := u.tel.Spans()
		require.Len(t, spans, 2)
		for _, span := range spans {
			require.NotContains(t, span.Name(), body)
			require.NotContains(t, span.Status().Description, body)
			for _, kv := range span.Attributes() {
				require.NotContains(t, kv.Value.String(), body, "the body is not recorded, as %s", kv.Key)
			}
			for _, e := range span.Events() {
				for _, kv := range e.Attributes {
					require.NotContains(t, kv.Value.String(), body, "the body is not recorded, as %s of %s", kv.Key, e.Name)
				}
			}
		}

		rm, err := u.tel.Metrics(t.Context())
		require.NoError(t, err)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for _, set := range dataPointAttrs(m) {
					for k, v := range setAttrs(set) {
						require.NotContains(t, v, body, "the body is not recorded, as %s of %s", k, m.Name)
					}
				}
			}
		}
		for _, r := range u.tel.Logs() {
			require.NotContains(t, r.Body().AsString(), body)
			for k, v := range attrs(r) {
				require.NotContains(t, v, body, "the body is not logged, as %s", k)
			}
		}
	})
}

// dataPointAttrs returns the attribute sets of m's data points.
func dataPointAttrs(m metricdata.Metrics) []attribute.Set {
	var out []attribute.Set
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range data.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, dp := range data.DataPoints {
			out = append(out, dp.Attributes)
		}
	}
	return out
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

// This test sets OpenTelemetry's globals, so it doesn't run in parallel. It
// sets them back when it ends, but the delegates OpenTelemetry handed out
// before the first Set stay bound to this test's recorders for the rest of
// the process: harmless, since the package uses no globals.
func TestService_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the Service never uses them, as the RabbitMQ proposal, #765, requires.
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
		in := newInSpans()
		u := newUnit(t, settings(), in.processor(nil))
		served := u.serve(t.Context())
		u.ch.send(amqp.Delivery{MessageId: "traced", RoutingKey: queueName, Headers: publisherHeaders(t)})
		synctest.Wait()
		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)

		require.Len(t, spansNamed(u.tel, processSpan), 1, "the Setup's TracerProvider recorded the span")
		require.Equal(t, publisherSpanContext(t).TraceID(), in.span("traced").TraceID(), "the Setup's propagator extracted the trace")
		metricNamed(t, u.tel, "messaging.client.consumed.messages")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
}
