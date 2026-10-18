// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"encoding/json"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/bedrock/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
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

// sendSpan is the name of the span of a record produced to topic.
const sendSpan = "send " + topic

// dlqTopic is the topic deadLetterInit produces its dead letters to.
const dlqTopic = "orders.dlq"

// order is what deadLetterInit decodes a record's value into.
type order struct {
	ID string `json:"id"`
}

// deadLetterInit is the Init of the producing proposal's Summary: a
// consumer whose Processor produces a record it can't decode to a
// dead-letter topic, with the Producer, before the record is committed.
func deadLetterInit(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	cs, ps, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}
	dlq, err := kafka.NewProducer(s, ps)
	if err != nil {
		return nil, err
	}
	svc, err := kafka.NewService(s, cs, queue.ProcessorFunc[kafka.Message](func(ctx context.Context, m kafka.Message) error {
		var o order
		if err := json.Unmarshal(m.Value, &o); err != nil {
			// A dead letter, stored before the record is committed.
			return dlq.Produce(ctx, kafka.Record{Topic: dlqTopic, Key: m.Key, Value: m.Value, Headers: m.Headers})
		}
		return nil
	}))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// readSettings sources the consumer's Settings and the Producer's
// ProducerSettings with config.Struct, the brokers shared between them.
func readSettings(ctx context.Context, s *humus.Setup) (kafka.Settings, kafka.ProducerSettings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	list := func(v string) []string { return strings.Split(v, ",") }
	type both struct {
		consumer kafka.Settings
		producer kafka.ProducerSettings
	}
	b, err := config.Struct(func(r *config.Resolver) both {
		cs, ps := kafka.DefaultSettings(), kafka.DefaultProducerSettings()
		cs.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
		cs.GroupID = r.Get(env("KAFKA_GROUP_ID").Validate(kafka.CheckGroupID))
		cs.Topics = r.Get(env("KAFKA_TOPICS").Map(list).Validate(kafka.CheckTopics))
		ps.Brokers = cs.Brokers
		return both{consumer: cs, producer: ps}
	}).Read(ctx)
	return b.consumer, b.producer, err
}

// spanNamed returns the one span of tel named name.
func spanNamed(t *testing.T, tel *humustest.Telemetry, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := spansNamed(tel, name)
	require.Len(t, spans, 1, "spans named %s", name)
	return spans[0]
}

// headerValues returns the values of r's headers with key, in order.
func headerValues(r *kgo.Record, key string) []string {
	var out []string
	for _, h := range r.Headers {
		if h.Key == key {
			out = append(out, string(h.Value))
		}
	}
	return out
}

// producesWith runs an App whose only Service is a job that calls run with
// a Producer of settings, recording its telemetry in tel, and returns once
// the App has exited, requiring it to exit 0.
func producesWith(t *testing.T, h *humustest.Host, tel *humustest.Telemetry, settings kafka.ProducerSettings, run func(ctx context.Context, p *kafka.Producer) error) {
	t.Helper()
	_, status := humustest.Start(t, produces(settings, run),
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

// sendAttrs are the attributes both of the Producer's instruments record
// for a record produced to partition 0 of topic.
var sendAttrs = map[string]string{
	"messaging.system":                   "kafka",
	"messaging.operation.name":           "send",
	"messaging.destination.name":         topic,
	"messaging.destination.partition.id": "0",
}

// withSend returns sendAttrs with kv added.
func withSend(kv ...string) map[string]string {
	out := make(map[string]string, len(sendAttrs)+len(kv)/2)
	for k, v := range sendAttrs {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestInit_produces_a_dead_letter_before_committing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1, kfake.SeedTopics(1, dlqTopic))
		// A record whose value isn't an order, with a producer's trace
		// context and baggage in its headers.
		produceRecords(t, h, c, []*kgo.Record{
			{Topic: topic, Key: []byte("key-0"), Value: []byte("0"), Headers: producerHeaders(t, producerSpanContext(t))},
		})

		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, deadLetterInit,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"KAFKA_BROKERS":  strings.Join(c.ListenAddrs(), ","),
				"KAFKA_GROUP_ID": "orders-consumer",
				"KAFKA_TOPICS":   "orders",
			}),
		)
		synctest.Wait() // processed, dead-lettered and committed
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.Equal(t, int64(1), committed(c)[0])
		dead := readAll(t, h, c, dlqTopic)
		require.Len(t, dead, 1)
		require.Equal(t, "0", string(dead[0].Value))

		send, process := spanNamed(t, tel, "send "+dlqTopic), spanNamed(t, tel, processSpan)
		require.Equal(t, process.SpanContext().SpanID(), send.Parent().SpanID())
		require.Equal(t, process.SpanContext().TraceID(), send.SpanContext().TraceID(), "one trace runs through the dead letter")
		require.Equal(t, trace.SpanKindProducer, send.SpanKind())
		require.True(t, send.EndTime().Before(process.EndTime()) || send.EndTime().Equal(process.EndTime()),
			"the dead letter was stored before Process returned")

		// The dead letter carries the send span's context, not the copied
		// one, and the consumed record's baggage.
		traceparents := headerValues(dead[0], "traceparent")
		require.Len(t, traceparents, 1)
		require.Contains(t, traceparents[0], send.SpanContext().SpanID().String())
		require.NotContains(t, traceparents[0], producerSpanID)
		require.Equal(t, []string{"tenant=acme"}, headerValues(dead[0], "baggage"))
	})
}

func TestProducer_the_consumers_process_span_is_a_child_of_the_send_span(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		produced := humustest.NewTelemetry()
		producesWith(t, h, produced, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			return p.Produce(ctx, value(0))
		})
		send := spanNamed(t, produced, sendSpan)

		consumed := humustest.NewTelemetry()
		require.Zero(t, consumeAll(t, h, consumed, settings(c.ListenAddrs()...), func(context.Context, kafka.Message) error { return nil }))
		process := spanNamed(t, consumed, processSpan)

		require.True(t, process.Parent().IsRemote())
		require.Equal(t, send.SpanContext().TraceID(), process.Parent().TraceID())
		require.Equal(t, send.SpanContext().SpanID(), process.Parent().SpanID())
	})
}

func TestProducer_send_span_attributes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		parent := producerSpanContext(t)

		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			ctx = trace.ContextWithSpanContext(ctx, parent)
			if err := p.Produce(ctx, kafka.Record{Topic: topic, Key: []byte("key-0"), Value: []byte("0")}); err != nil {
				return err
			}
			return p.Produce(ctx, kafka.Record{Topic: topic, Key: []byte("key-1"), Value: []byte("1")})
		})

		spans := spansNamed(tel, sendSpan)
		require.Len(t, spans, 2)
		span := spans[1]
		require.Equal(t, trace.SpanKindProducer, span.SpanKind())
		require.Equal(t, parent.SpanID(), span.Parent().SpanID(), "a child of the span in Produce's ctx")
		require.Equal(t, parent.TraceID(), span.SpanContext().TraceID())
		require.Equal(t, serviceScope, span.InstrumentationScope().Name)

		a := spanAttrs(span)
		require.Equal(t, "kafka", a["messaging.system"].AsString())
		require.Equal(t, "send", a["messaging.operation.type"].AsString())
		require.Equal(t, "send", a["messaging.operation.name"].AsString())
		require.Equal(t, topic, a["messaging.destination.name"].AsString())
		require.Equal(t, "0", a["messaging.destination.partition.id"].AsString())
		require.Equal(t, int64(1), a["messaging.kafka.offset"].AsInt64())
		require.Len(t, a, 6, "no other attribute: %v", a)
		for k, v := range a {
			require.NotContains(t, string(k), "key", "the record's key is not recorded")
			require.NotEqual(t, "key-1", v.String(), "the record's key is not recorded, as %s", k)
		}
		require.Equal(t, codes.Unset, span.Status().Code)
		require.Empty(t, span.Events())
	})
}

func TestProducer_replaces_the_propagators_headers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		m, err := baggage.NewMember("tenant", "globex")
		require.NoError(t, err)
		b, err := baggage.New(m)
		require.NoError(t, err)

		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			copied := []kafka.Header{{Key: "content-type", Value: []byte("text/plain")}}
			for _, hdr := range producerHeaders(t, producerSpanContext(t)) {
				copied = append(copied, kafka.Header{Key: hdr.Key, Value: hdr.Value})
			}
			copied = append(copied, kafka.Header{Key: "tracestate", Value: []byte("vendor=old")}, kafka.Header{Key: "content-type", Value: []byte("again")})
			return p.Produce(baggage.ContextWithBaggage(ctx, b), kafka.Record{Topic: topic, Value: []byte("0"), Headers: copied})
		})
		send := spanNamed(t, tel, sendSpan)

		got := readAll(t, h, c, topic)
		require.Len(t, got, 1)
		require.Equal(t, []string{"text/plain", "again"}, headerValues(got[0], "content-type"), "other headers are kept, in order")
		traceparents := headerValues(got[0], "traceparent")
		require.Len(t, traceparents, 1)
		require.Contains(t, traceparents[0], send.SpanContext().SpanID().String())
		require.Empty(t, headerValues(got[0], "tracestate"), "a field the span's context doesn't write is removed too")
		require.Equal(t, []string{"tenant=globex"}, headerValues(got[0], "baggage"), "ctx's baggage, not the copied one")
	})
}

func TestProducer_send_span_outlives_its_call(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		_, release := holdProduce(t, c)

		var (
			err           error
			endedAtReturn int
		)
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			err = p.Produce(ctx, value(0))
			endedAtReturn = len(spansNamed(tel, sendSpan))
			time.Sleep(2 * time.Second)
			release() // the broker answers at 3s
			return nil
		})

		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Zero(t, endedAtReturn, "the span was still running when the call returned")
		span := spanNamed(t, tel, sendSpan)
		took := span.EndTime().Sub(span.StartTime())
		require.GreaterOrEqual(t, took, 3*time.Second, "the span ended once the broker answered")
		require.Less(t, took, kafka.DefaultProducerSettings().DeliveryTimeout)
		require.Equal(t, codes.Unset, span.Status().Code, "the record was acknowledged")
		require.Equal(t, int64(0), spanAttrs(span)["messaging.kafka.offset"].AsInt64())
	})
}

func TestProducer_records_a_failed_record_on_its_span(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		holdProduce(t, c) // released when the test ends

		ps := producerSettings(c.ListenAddrs()...)
		ps.DeliveryTimeout = 2 * time.Second
		var err error
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx, value(0))
			return nil
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)

		span := spanNamed(t, tel, sendSpan)
		require.Equal(t, codes.Error, span.Status().Code)
		require.Equal(t, 2*time.Second, span.EndTime().Sub(span.StartTime()))
		a := spanAttrs(span)
		require.NotEmpty(t, a["error.type"].AsString())
		_, acknowledged := a["messaging.kafka.offset"]
		require.False(t, acknowledged, "a failed record has no offset")
		events := span.Events()
		require.Len(t, events, 1)
		require.Equal(t, "exception", events[0].Name)
	})
}

func TestProducer_records_the_messaging_instruments(t *testing.T) {
	t.Run("each acknowledged record", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)

			tel := humustest.NewTelemetry()
			producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
				return p.Produce(ctx, value(0), value(1), value(2))
			})

			sent, ok := metricNamed(t, tel, serviceScope, "messaging.client.sent.messages").Data.(metricdata.Sum[int64])
			require.True(t, ok)
			require.True(t, sent.IsMonotonic)
			require.Len(t, sent.DataPoints, 1)
			require.Equal(t, int64(3), sent.DataPoints[0].Value)
			require.Equal(t, sendAttrs, setAttrs(sent.DataPoints[0].Attributes))

			durations := histogramPoints(t, tel, "messaging.client.operation.duration")
			require.Len(t, durations, 1)
			require.Equal(t, uint64(3), durations[0].Count)
			require.Equal(t, withSend("messaging.operation.type", "send"), setAttrs(durations[0].Attributes))

			require.Equal(t, []string{
				"messaging.client.operation.duration",
				"messaging.client.sent.messages",
			}, metricNames(t, tel, serviceScope))
		})
	})

	t.Run("a failed record", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			holdProduce(t, c) // released when the test ends

			ps := producerSettings(c.ListenAddrs()...)
			ps.DeliveryTimeout = 2 * time.Second
			var err error
			tel := humustest.NewTelemetry()
			producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
				err = p.Produce(ctx, value(0))
				return nil
			})
			require.Error(t, err)

			sent, ok := metricNamed(t, tel, serviceScope, "messaging.client.sent.messages").Data.(metricdata.Sum[int64])
			require.True(t, ok)
			require.Len(t, sent.DataPoints, 1)
			require.Equal(t, int64(1), sent.DataPoints[0].Value)
			a := setAttrs(sent.DataPoints[0].Attributes)
			require.NotEmpty(t, a["error.type"])
			require.Equal(t, withSend("error.type", a["error.type"]), a)

			durations := histogramPoints(t, tel, "messaging.client.operation.duration")
			require.Len(t, durations, 1)
			require.Equal(t, uint64(1), durations[0].Count)
			require.Equal(t, 2.0, durations[0].Sum, "from Produce taking the record to its result, in seconds")
			require.Equal(t, withSend("messaging.operation.type", "send", "error.type", a["error.type"]), setAttrs(durations[0].Attributes))
		})
	})
}

func TestProducer_records_kotels_produce_counters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			return p.Produce(ctx, value(0), value(1), value(2))
		})

		require.Equal(t, int64(3), sum(t, tel, kotelScope, "messaging.kafka.produce_records.count"))
		require.NotZero(t, sum(t, tel, kotelScope, "messaging.kafka.produce_bytes.count"))
		for _, name := range []string{"messaging.kafka.produce_records.count", "messaging.kafka.produce_bytes.count"} {
			data, ok := metricNamed(t, tel, kotelScope, name).Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range data.DataPoints {
				a := setAttrs(dp.Attributes)
				require.NotEmpty(t, a["node_id"], name)
				require.Equal(t, topic, a["topic"], name)
			}
		}
	})
}

// connects returns kotel's messaging.kafka.connects.count of tel, by
// broker, or nil when nothing has connected yet.
func connects(ctx context.Context, tel *humustest.Telemetry) map[string]int64 {
	rm, err := tel.Metrics(ctx)
	if err != nil {
		return nil
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			data, ok := m.Data.(metricdata.Sum[int64])
			if sm.Scope.Name != kotelScope || m.Name != "messaging.kafka.connects.count" || !ok {
				continue
			}
			for _, dp := range data.DataPoints {
				out[setAttrs(dp.Attributes)["node_id"]] += dp.Value
			}
		}
	}
	return out
}

func TestProducer_a_caller_giving_up_keeps_the_connection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		held, release := holdProduce(t, c)

		var (
			err, next error
			took      time.Duration
			whileHeld map[string]int64
		)
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			heldConnects := make(chan map[string]int64, 1)
			go func() {
				<-held
				// Every connection the call needs is open: franz-go's
				// produce connection to the broker carries the held
				// request.
				heldConnects <- connects(ctx, tel)
				time.Sleep(3 * time.Second)
				release() // the broker answers at 3s
			}()
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			start := time.Now()
			err = p.Produce(ctx, value(0))
			took = time.Since(start)
			time.Sleep(3 * time.Second) // the broker has answered
			whileHeld = <-heldConnects
			// A cancelled produce request would have closed the
			// connection, and this call would dial it again.
			next = p.Produce(context.WithoutCancel(ctx), value(1))
			return nil
		})

		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, time.Second, took)
		require.NoError(t, next)
		require.Len(t, readAll(t, h, c, topic), 2, "the held record was stored all the same")
		for _, span := range spansNamed(tel, sendSpan) {
			require.Equal(t, codes.Unset, span.Status().Code, "both records were acknowledged")
		}

		// franz-go opens one connection to the broker for its produce
		// requests, and one for everything else. A cancelled produce
		// request would close the first and dial it again.
		require.NotZero(t, whileHeld["0"])
		require.Equal(t, whileHeld["0"], connects(t.Context(), tel)["0"],
			"the broker's connections were not dialled again once the call gave up")
	})
}

func TestProducer_with_a_done_ctx_records_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		var noTopic, done error
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			noTopic = p.Produce(ctx, kafka.Record{Value: []byte("no topic")})
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			done = p.Produce(ctx, value(0))
			return nil
		})
		require.Error(t, noTopic)
		require.ErrorIs(t, done, context.Canceled)

		require.Empty(t, spansNamed(tel, sendSpan))
		require.Empty(t, spansNamed(tel, "send "))
		require.Empty(t, metricNames(t, tel, serviceScope), "neither instrument counts a record that wasn't sent")
	})
}

// This test sets OpenTelemetry's globals, so it doesn't run in parallel.
// See TestService_uses_no_OpenTelemetry_globals.
func TestProducer_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the Producer never uses them, kotel's Meter included, as the producing proposal, #764, requires.
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

		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			return p.Produce(ctx, value(0))
		})
		require.Len(t, spansNamed(tel, sendSpan), 1, "the Setup's TracerProvider recorded the span")
		require.NotEmpty(t, metricNames(t, tel, serviceScope), "the Setup's MeterProvider recorded the instruments")
		require.NotEmpty(t, metricNames(t, tel, kotelScope), "the Setup's MeterProvider recorded kotel's instruments")
		got := readAll(t, h, c, topic)
		require.Len(t, got, 1)
		require.Len(t, headerValues(got[0], "traceparent"), 1, "the Setup's propagator injected the span's context")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
}
