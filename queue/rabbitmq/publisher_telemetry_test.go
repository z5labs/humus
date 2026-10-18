// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file show what the Publisher reports, on the stand-in
// of publisher_standin_test.go, inside testing/synctest.

// tracedPublisher is a Publisher on a stand-in, whose Setup's telemetry is
// a humustest Telemetry.
type tracedPublisher struct {
	pub *rabbitmq.Publisher
	s   *pubStandIn
	tel *humustest.Telemetry
	sdk telemetry.SDK
}

func newTracedPublisher(t *testing.T) *tracedPublisher {
	t.Helper()
	return newTracedPublisherOf(t, publisherSettings())
}

// newTracedPublisherOf is newTracedPublisher with settings.
func newTracedPublisherOf(t *testing.T, settings rabbitmq.PublisherSettings) *tracedPublisher {
	t.Helper()
	tel := humustest.NewTelemetry()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	pub, err := rabbitmq.NewPublisher(&humus.Setup{Host: humustest.NewHost(t).Host(), Telemetry: sdk}, settings)
	require.NoError(t, err)
	s := newPubStandIn()
	rabbitmq.SetDial(pub, s.dial)
	return &tracedPublisher{pub: pub, s: s, tel: tel, sdk: sdk}
}

// caller returns a context carrying a span of the caller's own, and the
// baggage member tenant=acme, and that span.
func (tp *tracedPublisher) caller(t *testing.T) (context.Context, trace.Span) {
	t.Helper()
	m, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(m)
	require.NoError(t, err)
	ctx, span := tp.sdk.TracerProvider.Tracer("caller").Start(baggage.ContextWithBaggage(t.Context(), b), "caller")
	t.Cleanup(func() { span.End() })
	return ctx, span
}

// onlySpan returns the only span of tp named name.
func (tp *tracedPublisher) onlySpan(t *testing.T, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := spansNamed(tp.tel, name)
	require.Len(t, spans, 1, "spans named %q", name)
	return spans[0]
}

// sentAttrs are the attributes both instruments record for a publish of
// accepted, which is to the exchange orders with the routing key accepted,
// from publisherSettings().
var sentAttrs = map[string]string{
	"messaging.system":           "rabbitmq",
	"messaging.operation.name":   "send",
	"messaging.destination.name": "orders:accepted",
	"server.address":             "rabbit",
	"server.port":                "5672",
}

// sentWith returns sentAttrs with error.type set to errType.
func sentWith(errType string) map[string]string {
	out := make(map[string]string, len(sentAttrs)+1)
	for k, v := range sentAttrs {
		out[k] = v
	}
	out["error.type"] = errType
	return out
}

func TestPublisher_a_confirmed_publish_has_a_PRODUCER_span_with_every_attribute(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisher(t)
		tp.s.autoAck = true
		ctx, caller := tp.caller(t)
		m := accepted
		m.MessageID = "order-1"
		m.CorrelationID = "conversation-1"
		require.NoError(t, tp.pub.Publish(ctx, m))

		span := tp.onlySpan(t, "send orders")
		require.Equal(t, trace.SpanKindProducer, span.SpanKind())
		require.Equal(t, serviceScope, span.InstrumentationScope().Name)
		require.Equal(t, caller.SpanContext().SpanID(), span.Parent().SpanID(), "the caller's span is its parent")
		require.Equal(t, caller.SpanContext().TraceID(), span.SpanContext().TraceID())
		require.Equal(t, codes.Unset, span.Status().Code)
		require.Empty(t, span.Events())

		a := spanAttrs(span)
		require.Equal(t, "rabbitmq", a["messaging.system"].AsString())
		require.Equal(t, "send", a["messaging.operation.type"].AsString())
		require.Equal(t, "send", a["messaging.operation.name"].AsString())
		require.Equal(t, "orders:accepted", a["messaging.destination.name"].AsString())
		require.Equal(t, "accepted", a["messaging.rabbitmq.destination.routing_key"].AsString())
		require.Equal(t, "order-1", a["messaging.message.id"].AsString())
		require.Equal(t, "conversation-1", a["messaging.message.conversation_id"].AsString())
		require.Equal(t, "rabbit", a["server.address"].AsString())
		require.Equal(t, int64(5672), a["server.port"].AsInt64())
		require.Len(t, a, 9, "no other attribute: %v", a)
	})
}

func TestPublisher_a_publish_to_the_default_exchange_is_send_amq_default(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisher(t)
		tp.s.autoAck = true
		require.NoError(t, tp.pub.Publish(t.Context(), rabbitmq.Publishing{RoutingKey: "orders", Body: []byte("order 1")}))

		span := tp.onlySpan(t, "send amq.default")
		require.False(t, span.Parent().IsValid(), "a Publish with no span on its context starts a new trace")
		a := spanAttrs(span)
		require.Equal(t, "orders", a["messaging.destination.name"].AsString(), "the empty exchange is left out")
		require.Equal(t, "orders", a["messaging.rabbitmq.destination.routing_key"].AsString())
		for _, k := range []string{"messaging.message.id", "messaging.message.conversation_id"} {
			require.NotContains(t, a, k)
		}
		require.Len(t, a, 7, "no other attribute: %v", a)

		sent, ok := metricNamed(t, tp.tel, "messaging.client.sent.messages").Data.(metricdata.Sum[int64])
		require.True(t, ok)
		require.Len(t, sent.DataPoints, 1)
		require.Equal(t, "orders", setAttrs(sent.DataPoints[0].Attributes)["messaging.destination.name"])
	})
}

func TestPublisher_injects_its_spans_trace_context_into_a_copy_of_the_headers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisher(t)
		tp.s.autoAck = true
		ctx, _ := tp.caller(t)
		headers := map[string]any{"tenant-id": "acme", "attempt": int32(1)}
		m := accepted
		m.Headers = headers
		require.NoError(t, tp.pub.Publish(ctx, m))

		span := tp.onlySpan(t, "send orders")
		sc := span.SpanContext()
		p := <-tp.s.published
		require.Equal(t, "00-"+sc.TraceID().String()+"-"+sc.SpanID().String()+"-01", p.msg.Headers["traceparent"],
			"the consumer's span is a child of the publish's, not of the caller's")
		require.Equal(t, "tenant=acme", p.msg.Headers["baggage"], "the caller's baggage travels with the message")
		require.Equal(t, "acme", p.msg.Headers["tenant-id"])
		require.Equal(t, int32(1), p.msg.Headers["attempt"])
		require.Len(t, p.msg.Headers, 4)
		require.Equal(t, map[string]any{"tenant-id": "acme", "attempt": int32(1)}, headers, "the caller's map is unchanged")
	})
}

func TestPublisher_a_nil_Headers_map_gets_a_new_one_with_the_trace_context(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisher(t)
		tp.s.autoAck = true
		require.NoError(t, tp.pub.Publish(t.Context(), accepted))

		sc := tp.onlySpan(t, "send orders").SpanContext()
		p := <-tp.s.published
		require.Equal(t, amqp.Table{"traceparent": "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"}, p.msg.Headers)
	})
}

func TestPublisher_a_failed_publish_is_on_its_span_and_both_instruments(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		fail    func(*published)
		errType string
	}{
		"unroutable":    {fail: (*published).unroutable, errType: "312"},
		"nacked":        {fail: (*published).nack, errType: "nacked"},
		"404 NOT_FOUND": {fail: func(p *published) { p.ch.fail(&amqp.Error{Code: amqp.NotFound, Reason: "no exchange", Server: true}) }, errType: "404"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				// One attempt, so a nack is the Publish's result.
				settings := publisherSettings()
				settings.MaxAttempts = 1
				tp := newTracedPublisherOf(t, settings)

				// A confirmed publish first, so each instrument has a series
				// without error.type beside the failed one's.
				done := publishAsync(t.Context(), tp.pub, accepted)
				(<-tp.s.published).ack()
				require.NoError(t, <-done)

				done = publishAsync(t.Context(), tp.pub, accepted)
				p := <-tp.s.published
				// The broker takes two seconds to answer, which synctest's
				// clock makes exact.
				time.Sleep(2 * time.Second)
				tc.fail(p)
				err := <-done
				require.Error(t, err)

				spans := spansNamed(tp.tel, "send orders")
				require.Len(t, spans, 2)
				var failed sdktrace.ReadOnlySpan
				for _, s := range spans {
					if s.Status().Code == codes.Error {
						require.Nil(t, failed, "only one span failed")
						failed = s
					} else {
						require.NotContains(t, spanAttrs(s), "error.type")
					}
				}
				require.NotNil(t, failed)
				require.Equal(t, err.Error(), failed.Status().Description)
				require.Equal(t, tc.errType, spanAttrs(failed)["error.type"].AsString())
				require.Len(t, failed.Events(), 1)
				require.Equal(t, "exception", failed.Events()[0].Name)

				sent, ok := metricNamed(t, tp.tel, "messaging.client.sent.messages").Data.(metricdata.Sum[int64])
				require.True(t, ok, "messaging.client.sent.messages is an int64 sum")
				require.True(t, sent.IsMonotonic)
				require.Len(t, sent.DataPoints, 2, "one series without error.type, one with")
				sentByErr := map[string]metricdata.DataPoint[int64]{}
				for _, dp := range sent.DataPoints {
					sentByErr[setAttrs(dp.Attributes)["error.type"]] = dp
				}
				require.Equal(t, sentAttrs, setAttrs(sentByErr[""].Attributes))
				require.Equal(t, int64(1), sentByErr[""].Value)
				require.Equal(t, sentWith(tc.errType), setAttrs(sentByErr[tc.errType].Attributes))
				require.Equal(t, int64(1), sentByErr[tc.errType].Value)

				durations, ok := metricNamed(t, tp.tel, "messaging.client.operation.duration").Data.(metricdata.Histogram[float64])
				require.True(t, ok, "messaging.client.operation.duration is a float64 histogram")
				require.Len(t, durations.DataPoints, 2, "one series without error.type, one with")
				durByErr := map[string]metricdata.HistogramDataPoint[float64]{}
				for _, dp := range durations.DataPoints {
					durByErr[setAttrs(dp.Attributes)["error.type"]] = dp
				}
				require.Equal(t, sentAttrs, setAttrs(durByErr[""].Attributes))
				require.Equal(t, uint64(1), durByErr[""].Count)
				require.InDelta(t, 0.0, durByErr[""].Sum, 1e-9, "the confirm came at once")
				require.Equal(t, sentWith(tc.errType), setAttrs(durByErr[tc.errType].Attributes))
				require.Equal(t, uint64(1), durByErr[tc.errType].Count)
				require.InDelta(t, 2.0, durByErr[tc.errType].Sum, 1e-9, "from the publish to the broker's answer")
			})
		})
	}
}

func TestPublisher_a_Publish_that_fails_before_it_publishes_is_on_its_span_but_no_messaging_instrument(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisher(t)
		tp.s.hold = make(chan struct{}) // the dial never completes
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := tp.pub.Publish(ctx, accepted)
		require.ErrorIs(t, err, context.DeadlineExceeded)

		span := tp.onlySpan(t, "send orders")
		require.Equal(t, codes.Error, span.Status().Code)
		require.Equal(t, "context.deadlineExceededError", spanAttrs(span)["error.type"].AsString())

		rm, err := tp.tel.Metrics(t.Context())
		require.NoError(t, err)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if strings.HasPrefix(m.Name, "humus.") {
					// humus's own instruments count the attempt the breaker
					// admitted, which metrics_test.go shows.
					continue
				}
				require.Empty(t, dataPointAttrs(m), "nothing was published, but %s recorded it", m.Name)
			}
		}
	})
}

func TestPublisher_never_records_the_body(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const body = "card 4111-1111-1111-1111"
		settings := publisherSettings()
		settings.MaxAttempts = 1
		tp := newTracedPublisherOf(t, settings)
		m := accepted
		m.Body = []byte(body)

		for i, outcome := range []func(*published){(*published).ack, (*published).nack, (*published).unroutable} {
			done := publishAsync(t.Context(), tp.pub, m)
			outcome(<-tp.s.published)
			err := <-done
			if i == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), body)
			}
		}

		spans := tp.tel.Spans()
		require.Len(t, spans, 3)
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

		rm, err := tp.tel.Metrics(t.Context())
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
	})
}
