// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run whole Apps inside a synctest bubble, on a
// humustest Host, against kfake. A producing App is a job whose BAGGAGE,
// set with humustest.WithEnv, carries tenant=acme and a scenario written
// with jsonscenario; a consuming App gets records whose baggage header
// carries the same.

// scenarioKey is the baggage member the tests' codec reads.
const scenarioKey = "acme.fault"

// ordersPoint is the injection point of topic.
const ordersPoint = "kafka:" + topic

// paymentsTopic is a second topic, for a call to two points.
const paymentsTopic = "payments"

// injectedErrorType is the error.type of an injected failure.
const injectedErrorType = "github.com/z5labs/humus/fault.InjectedError"

// The span events of failure injection.
const (
	evaluationEvent = "humus.fit.evaluation"
	stripEvent      = "humus.fit.strip"
)

// abortOrders aborts every call to topic.
var abortOrders = fit.Scenario{Rules: []fit.Rule{{Point: ordersPoint, Fault: fit.Fault{Abort: true}}}}

// sparesOrders aborts a point other than topic's.
var sparesOrders = fit.Scenario{Rules: []fit.Rule{{Point: "kafka:ledger", Fault: fit.Fault{Abort: true}}}}

// faultsCodec returns the tests' jsonscenario Codec.
func faultsCodec(t *testing.T) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	return c
}

// withFaults is the humus.Option that turns failure injection on, with
// the tests' codec.
func withFaults(t *testing.T) humus.Option {
	t.Helper()
	codec := faultsCodec(t)
	return humus.WithFaults(func(context.Context, *humus.Setup) (fault.Settings, error) {
		d := fault.DefaultSettings()
		d.Decoder = codec
		return d, nil
	})
}

// scenarioBaggage returns the baggage tenant=acme with s written into it,
// as a W3C baggage header's value.
func scenarioBaggage(t *testing.T, s fit.Scenario) string {
	t.Helper()
	m, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(m)
	require.NoError(t, err)
	b, err = faultsCodec(t).Encode(b, s)
	require.NoError(t, err)
	return b.String()
}

// producesUnder runs an App whose only Service is a job that calls run
// with a Producer of settings, with BAGGAGE set to bag, recording its
// telemetry in tel, and returns once the App has exited, requiring it to
// exit 0. faults turns failure injection on.
func producesUnder(t *testing.T, h *humustest.Host, tel *humustest.Telemetry, faults bool, bag string, settings kafka.ProducerSettings, run func(ctx context.Context, p *kafka.Producer) error) {
	t.Helper()
	opts := []humus.Option{humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())}
	if faults {
		opts = append(opts, withFaults(t))
	}
	_, status := humustest.Start(t, produces(settings, run),
		humustest.WithApp(opts...),
		humustest.WithEnv(map[string]string{"BAGGAGE": bag}),
	)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

// events returns span's events named name.
func events(span sdktrace.ReadOnlySpan, name string) []sdktrace.Event {
	var out []sdktrace.Event
	for _, e := range span.Events() {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// eventAttrs returns e's attributes by key.
func eventAttrs(e sdktrace.Event) map[attribute.Key]attribute.Value {
	out := make(map[attribute.Key]attribute.Value)
	for _, kv := range e.Attributes {
		out[kv.Key] = kv.Value
	}
	return out
}

// storedBaggage parses the baggage header of r.
func storedBaggage(t *testing.T, r *kgo.Record) baggage.Baggage {
	t.Helper()
	values := headerValues(r, "baggage")
	require.Len(t, values, 1)
	b, err := baggage.Parse(values[0])
	require.NoError(t, err)
	return b
}

func TestProducer_an_aborted_call_sends_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		tel := humustest.NewTelemetry()
		var err error
		producesUnder(t, h, tel, true, scenarioBaggage(t, abortOrders), producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx, value(0), value(1))
			return nil
		})

		require.ErrorIs(t, err, fit.ErrInjected)
		var injected *fault.InjectedError
		require.ErrorAs(t, err, &injected)
		require.Equal(t, ordersPoint, injected.Point)
		require.Empty(t, readAll(t, h, c, topic), "nothing reaches the topic")

		spans := spansNamed(tel, sendSpan)
		require.Len(t, spans, 2)
		for _, span := range spans {
			evs := events(span, evaluationEvent)
			require.Len(t, evs, 1, "one evaluation per record")
			a := eventAttrs(evs[0])
			require.Equal(t, ordersPoint, a["humus.fit.point"].AsString())
			require.True(t, a["humus.fit.abort"].AsBool())
			require.Equal(t, injectedErrorType, spanAttrs(span)["error.type"].AsString())
			require.Equal(t, codes.Error, span.Status().Code)
		}

		// Neither messaging instrument counts a call that sent nothing,
		// and the breaker counts it as a failure.
		names := metricNames(t, tel, serviceScope)
		require.NotContains(t, names, "messaging.client.sent.messages")
		require.NotContains(t, names, "messaging.client.operation.duration")
		calls, ok := metricNamed(t, tel, serviceScope, "humus.circuit.calls").Data.(metricdata.Sum[int64])
		require.True(t, ok)
		require.Len(t, calls.DataPoints, 1)
		require.Equal(t, int64(1), calls.DataPoints[0].Value)
		require.Equal(t, "failure", setAttrs(calls.DataPoints[0].Attributes)["humus.circuit.outcome"])
	})
}

func TestProducer_aborted_calls_open_the_breaker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		tel := humustest.NewTelemetry()
		var aborted []error
		var clean, scenario error
		producesUnder(t, h, tel, true, scenarioBaggage(t, abortOrders), producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			// The default policy opens after 5 failures in a row.
			for i := range 5 {
				aborted = append(aborted, p.Produce(ctx, value(i)))
			}
			// A call whose context carries no scenario is rejected too.
			clean = p.Produce(baggage.ContextWithoutBaggage(ctx), value(5))
			// A call the breaker rejects evaluates nothing.
			scenario = p.Produce(ctx, value(6))
			return nil
		})

		for _, err := range aborted {
			require.ErrorIs(t, err, fit.ErrInjected)
		}
		require.ErrorIs(t, clean, circuit.ErrRejected)
		require.NotErrorIs(t, clean, fit.ErrInjected)
		require.ErrorIs(t, scenario, circuit.ErrRejected)
		require.Empty(t, readAll(t, h, c, topic))

		spans := spansNamed(tel, sendSpan)
		require.Len(t, spans, 7)
		evaluated := 0
		for _, span := range spans {
			evaluated += len(events(span, evaluationEvent))
		}
		require.Equal(t, 5, evaluated, "only the calls the breaker admitted were evaluated")
	})
}

func TestProducer_a_call_waits_the_longest_delay_of_its_records(t *testing.T) {
	t.Run("the delay passes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1, kfake.SeedTopics(1, paymentsTopic))
			s := fit.Scenario{Rules: []fit.Rule{
				{Point: ordersPoint, Fault: fit.Fault{Delay: 2 * time.Second}},
				{Point: "kafka:" + paymentsTopic, Fault: fit.Fault{Delay: 5 * time.Second}},
			}}

			tel := humustest.NewTelemetry()
			var err error
			var elapsed time.Duration
			producesUnder(t, h, tel, true, scenarioBaggage(t, s), producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
				start := time.Now()
				err = p.Produce(ctx, value(0), kafka.Record{Topic: paymentsTopic, Value: []byte("1")})
				elapsed = time.Since(start)
				return nil
			})

			require.NoError(t, err)
			require.GreaterOrEqual(t, elapsed, 5*time.Second, "the longest Delay, not the sum")
			require.Less(t, elapsed, 7*time.Second, "the longest Delay, not the sum")
			require.Len(t, readAll(t, h, c, topic), 1)
			require.Len(t, readAll(t, h, c, paymentsTopic), 1)
		})
	})

	t.Run("DeliveryTimeout cuts it short", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			s := fit.Scenario{Rules: []fit.Rule{{Point: ordersPoint, Fault: fit.Fault{Delay: 5 * time.Second}}}}

			ps := producerSettings(c.ListenAddrs()...)
			ps.DeliveryTimeout = 2 * time.Second
			tel := humustest.NewTelemetry()
			var err error
			var elapsed time.Duration
			producesUnder(t, h, tel, true, scenarioBaggage(t, s), ps, func(ctx context.Context, p *kafka.Producer) error {
				start := time.Now()
				err = p.Produce(ctx, value(0))
				elapsed = time.Since(start)
				return nil
			})

			require.Equal(t, 2*time.Second, elapsed)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotErrorIs(t, err, fit.ErrInjected)
			var injected *fault.InjectedError
			require.ErrorAs(t, err, &injected)
			require.Equal(t, ordersPoint, injected.Point)
			require.Equal(t, circuit.Failure, kafka.Classify(t.Context(), err))
			require.Empty(t, readAll(t, h, c, topic))
			require.Equal(t, injectedErrorType, spanAttrs(spanNamed(t, tel, sendSpan))["error.type"].AsString())
		})
	})
}

func TestProducer_strips_the_scenario_unless_ForwardScenario_is_set(t *testing.T) {
	for _, forward := range []bool{false, true} {
		name := "ForwardScenario false"
		if forward {
			name = "ForwardScenario true"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				c := cluster(t, h, 1)

				ps := producerSettings(c.ListenAddrs()...)
				ps.ForwardScenario = forward
				tel := humustest.NewTelemetry()
				var err error
				producesUnder(t, h, tel, true, scenarioBaggage(t, sparesOrders), ps, func(ctx context.Context, p *kafka.Producer) error {
					err = p.Produce(ctx, value(0))
					return nil
				})
				require.NoError(t, err)

				stored := readAll(t, h, c, topic)
				require.Len(t, stored, 1)
				b := storedBaggage(t, stored[0])
				require.Equal(t, "acme", b.Member("tenant").Value())
				require.Equal(t, forward, b.Member(scenarioKey).Key() != "", "the scenario member")

				span := spanNamed(t, tel, sendSpan)
				require.Len(t, events(span, evaluationEvent), 1, "the record's own context keeps the scenario")
				strips := events(span, stripEvent)
				if forward {
					require.Empty(t, strips)
					return
				}
				require.Len(t, strips, 1)
				require.Empty(t, strips[0].Attributes)
			})
		})
	}
}

// consumeUnder runs an App whose only Service consumes s with p, until
// every record is processed, then stops it with SIGTERM and requires it to
// exit 0. faults turns failure injection on.
func consumeUnder(t *testing.T, h *humustest.Host, tel *humustest.Telemetry, faults bool, s kafka.Settings, p queue.ProcessorFunc[kafka.Message]) {
	t.Helper()
	opts := []humus.Option{humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())}
	if faults {
		opts = append(opts, withFaults(t))
	}
	proc, status := humustest.Start(t, consumes(s, p), humustest.WithApp(opts...))
	synctest.Wait()
	proc.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

// scenarioHeaders returns a producer's headers whose baggage is tenant=acme
// and s.
func scenarioHeaders(t *testing.T, s fit.Scenario) []kgo.RecordHeader {
	t.Helper()
	return []kgo.RecordHeader{{Key: "baggage", Value: []byte(scenarioBaggage(t, s))}}
}

// processed is what Process saw of one record.
type processed struct {
	offset   int64
	scenario bool
	tenant   string
}

// processedBy returns a Processor that records what it sees in out.
func processedBy(t *testing.T, out *[]processed) queue.ProcessorFunc[kafka.Message] {
	codec := faultsCodec(t)
	return func(ctx context.Context, m kafka.Message) error {
		_, carried, _ := fit.ScenarioFromContext(ctx, codec)
		*out = append(*out, processed{
			offset:   m.Offset,
			scenario: carried,
			tenant:   baggage.FromContext(ctx).Member("tenant").Value(),
		})
		return nil
	}
}

func TestService_strips_the_scenario_of_a_record_it_does_not_trust(t *testing.T) {
	testCases := []struct {
		name     string
		trust    func(m kafka.Message) bool
		scenario bool
	}{
		{name: "TrustScenario nil", trust: nil, scenario: false},
		{name: "TrustScenario trusts the topic", trust: func(m kafka.Message) bool { return m.Topic == topic }, scenario: true},
		{name: "TrustScenario trusts another topic", trust: func(m kafka.Message) bool { return m.Topic == paymentsTopic }, scenario: false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				c := cluster(t, h, 1)
				// A record with a scenario, then one with none.
				produceRecords(t, h, c, []*kgo.Record{
					{Topic: topic, Value: []byte("0"), Headers: scenarioHeaders(t, abortOrders)},
					{Topic: topic, Value: []byte("1"), Headers: producerHeaders(t, producerSpanContext(t))},
				})

				s := settings(c.ListenAddrs()...)
				var asked []int64
				if tc.trust != nil {
					s.TrustScenario = func(m kafka.Message) bool {
						asked = append(asked, m.Offset)
						return tc.trust(m)
					}
				}
				var seen []processed
				tel := humustest.NewTelemetry()
				consumeUnder(t, h, tel, true, s, processedBy(t, &seen))

				require.Equal(t, []processed{
					{offset: 0, scenario: tc.scenario, tenant: "acme"},
					{offset: 1, scenario: false, tenant: "acme"},
				}, seen)
				if tc.trust != nil {
					require.Equal(t, []int64{0}, asked, "asked only about the record that carries a scenario")
				}

				spans := spansNamed(tel, processSpan)
				strips := events(spanAt(t, spans, 0), stripEvent)
				if tc.scenario {
					require.Empty(t, strips)
				} else {
					require.Len(t, strips, 1)
					require.Empty(t, strips[0].Attributes)
				}
				require.Empty(t, events(spanAt(t, spans, 1), stripEvent))
			})
		})
	}
}

func TestService_does_not_recover_a_panic_in_TrustScenario(t *testing.T) {
	t.Parallel()

	// A panic in TrustScenario crashes the process, as one in a Processor
	// does, which a whole App can't show without crashing the test: so the
	// strip is called on its own, as the record's goroutine calls it.
	d := fault.DefaultSettings()
	d.Decoder = faultsCodec(t)
	faults, err := fault.New(d)
	require.NoError(t, err)
	s := settings("localhost:9092")
	s.TrustScenario = func(kafka.Message) bool { panic("trust") }
	svc, err := kafka.NewService(&humus.Setup{Host: humus.OSHost(), Telemetry: telemetry.Noop(), Faults: faults}, s,
		queue.ProcessorFunc[kafka.Message](func(context.Context, kafka.Message) error { return nil }))
	require.NoError(t, err)

	b, err := baggage.Parse(scenarioBaggage(t, abortOrders))
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), b)
	require.PanicsWithValue(t, "trust", func() { kafka.StripRecord(svc, ctx, kafka.Message{Topic: topic}) })
}

func TestFaults_off_evaluates_and_strips_nothing(t *testing.T) {
	t.Run("the Producer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)

			tel := humustest.NewTelemetry()
			var err error
			producesUnder(t, h, tel, false, scenarioBaggage(t, abortOrders), producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
				err = p.Produce(ctx, value(0))
				return nil
			})
			require.NoError(t, err)

			stored := readAll(t, h, c, topic)
			require.Len(t, stored, 1)
			b := storedBaggage(t, stored[0])
			require.NotEmpty(t, b.Member(scenarioKey).Key(), "the scenario goes as any baggage does")
			span := spanNamed(t, tel, sendSpan)
			require.Empty(t, events(span, evaluationEvent))
			require.Empty(t, events(span, stripEvent))
		})
	})

	t.Run("the Service", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := cluster(t, h, 1)
			produceRecords(t, h, c, []*kgo.Record{
				{Topic: topic, Value: []byte("0"), Headers: scenarioHeaders(t, abortOrders)},
			})

			s := settings(c.ListenAddrs()...)
			asked := false
			s.TrustScenario = func(kafka.Message) bool {
				asked = true
				return false
			}
			var seen []processed
			tel := humustest.NewTelemetry()
			consumeUnder(t, h, tel, false, s, processedBy(t, &seen))

			require.Equal(t, []processed{{offset: 0, scenario: true, tenant: "acme"}}, seen)
			require.False(t, asked, "TrustScenario is never asked")
			require.Empty(t, events(spanNamed(t, tel, processSpan), stripEvent))
		})
	})
}
