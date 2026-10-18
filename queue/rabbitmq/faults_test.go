// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/fit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run whole Apps inside a synctest bubble, with the
// stand-ins of publisher_standin_test.go and standin_test.go in place of a
// broker. A publishing App is a job whose BAGGAGE, set with
// humustest.WithEnv, carries tenant=acme and a scenario written with
// jsonscenario; a consuming App gets deliveries whose baggage header
// carries the same.

// scenarioKey is the baggage member the tests' codec reads.
const scenarioKey = "acme.fault"

// acceptedPoint is the injection point of a publish of accepted.
const acceptedPoint = "rabbitmq:orders:accepted"

// sendSpan is the name of the span of a publish of accepted.
const sendSpan = "send orders"

// injectedErrorType is the error.type of an injected failure.
const injectedErrorType = "github.com/z5labs/humus/fault.InjectedError"

// The span events of failure injection.
const (
	evaluationEvent = "humus.fit.evaluation"
	stripEvent      = "humus.fit.strip"
)

// abortAccepted aborts every publish of accepted.
var abortAccepted = fit.Scenario{Rules: []fit.Rule{{Point: acceptedPoint, Fault: fit.Fault{Abort: true}}}}

// sparesAccepted aborts a point other than accepted's.
var sparesAccepted = fit.Scenario{Rules: []fit.Rule{{Point: "rabbitmq:ledger", Fault: fit.Fault{Abort: true}}}}

// delayAccepted delays every publish of accepted by d.
func delayAccepted(d time.Duration) fit.Scenario {
	return fit.Scenario{Rules: []fit.Rule{{Point: acceptedPoint, Fault: fit.Fault{Delay: d}}}}
}

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

// appOptions returns the App's options: a humustest Host, tel's telemetry
// and, when faults is true, failure injection.
func appOptions(t *testing.T, tel *humustest.Telemetry, faults bool) []humus.Option {
	t.Helper()
	opts := []humus.Option{humus.WithHost(humustest.NewHost(t).Host()), humus.WithTelemetry(tel.Builder())}
	if faults {
		opts = append(opts, withFaults(t))
	}
	return opts
}

// publishesUnder runs an App whose only Service is a job that calls run
// with a Publisher of settings on s, with BAGGAGE set to bag, recording
// its telemetry in tel, and returns once the App has exited, requiring it
// to exit 0. faults turns failure injection on.
func publishesUnder(t *testing.T, tel *humustest.Telemetry, faults bool, bag string, settings rabbitmq.PublisherSettings, s *pubStandIn, run func(ctx context.Context, p *rabbitmq.Publisher)) {
	t.Helper()
	init := func(_ context.Context, setup *humus.Setup) ([]humus.Service, error) {
		pub, err := rabbitmq.NewPublisher(setup, settings)
		if err != nil {
			return nil, err
		}
		rabbitmq.SetDial(pub, s.dial)
		return []humus.Service{job.NewService(setup, func(ctx context.Context) error {
			run(ctx, pub)
			return nil
		})}, nil
	}
	_, status := humustest.Start(t, init,
		humustest.WithApp(appOptions(t, tel, faults)...),
		humustest.WithEnv(map[string]string{"BAGGAGE": bag}),
	)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

// autoAcking returns a stand-in whose channels confirm each publish.
func autoAcking() *pubStandIn {
	s := newPubStandIn()
	s.autoAck = true
	return s
}

// spanEvents returns span's events named name.
func spanEvents(span sdktrace.ReadOnlySpan, name string) []sdktrace.Event {
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

// onlySpanNamed returns the only span of tel named name.
func onlySpanNamed(t *testing.T, tel *humustest.Telemetry, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := spansNamed(tel, name)
	require.Len(t, spans, 1, "spans named %q", name)
	return spans[0]
}

// hasMetric reports whether tel recorded a metric named name under the
// package's scope.
func hasMetric(t *testing.T, tel *humustest.Telemetry, name string) bool {
	t.Helper()
	rm, err := tel.Metrics(t.Context())
	require.NoError(t, err)
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != serviceScope {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

// countsBy returns the data points of the Counter named name, summed by
// the value of key.
func countsBy(t *testing.T, tel *humustest.Telemetry, name, key string) map[string]int64 {
	t.Helper()
	sum, ok := metricNamed(t, tel, name).Data.(metricdata.Sum[int64])
	require.True(t, ok)
	out := map[string]int64{}
	for _, dp := range sum.DataPoints {
		out[setAttrs(dp.Attributes)[key]] += dp.Value
	}
	return out
}

// publishedBaggage parses the baggage header of m.
func publishedBaggage(t *testing.T, m *published) baggage.Baggage {
	t.Helper()
	v, ok := m.msg.Headers["baggage"].(string)
	require.True(t, ok, "the baggage header is a string")
	b, err := baggage.Parse(v)
	require.NoError(t, err)
	return b
}

func TestPublisher_an_aborted_Publish_sends_nothing_and_is_retried_as_a_lost_attempt(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		s := autoAcking()
		tel := humustest.NewTelemetry()
		var err error
		publishesUnder(t, tel, true, scenarioBaggage(t, abortAccepted), publisherSettings(), s, func(ctx context.Context, p *rabbitmq.Publisher) {
			err = p.Publish(ctx, accepted)
		})

		require.ErrorIs(t, err, fit.ErrInjected)
		var injected *fault.InjectedError
		require.ErrorAs(t, err, &injected)
		require.Equal(t, acceptedPoint, injected.Point)
		require.EqualError(t, err, `rabbitmq: publishing to exchange "orders" with routing key "accepted": fit: injected failure at rabbitmq:orders:accepted`)
		outcome, _ := rabbitmq.Classify(t.Context(), err)
		require.Equal(t, retry.Retry, outcome, "decided as a lost attempt is")
		require.Equal(t, circuit.Failure, rabbitmq.ClassifyBreaker(t.Context(), err), "counted as a lost attempt is")
		require.Empty(t, s.published, "nothing was sent")
		require.Zero(t, s.dials(), "nothing was sent")

		span := onlySpanNamed(t, tel, sendSpan)
		evs := spanEvents(span, evaluationEvent)
		require.Len(t, evs, 3, "one evaluation per attempt, MaxAttempts in all")
		for _, ev := range evs {
			a := eventAttrs(ev)
			require.Equal(t, acceptedPoint, a["humus.fit.point"].AsString())
			require.True(t, a["humus.fit.abort"].AsBool())
		}
		require.Equal(t, injectedErrorType, spanAttrs(span)["error.type"].AsString())
		require.Equal(t, codes.Error, span.Status().Code)

		// Neither messaging instrument counts an attempt that sent nothing,
		// and the breaker and the retries count each as a lost one.
		require.False(t, hasMetric(t, tel, "messaging.client.sent.messages"))
		require.False(t, hasMetric(t, tel, "messaging.client.operation.duration"))
		require.Equal(t, map[string]int64{"failure": 3}, countsBy(t, tel, "humus.circuit.calls", "humus.circuit.outcome"))
		require.Equal(t, map[string]int64{"again": 2, "exhausted": 1}, countsBy(t, tel, "humus.retry.decisions", "humus.retry.decision"))
	})
}

func TestPublisher_aborted_attempts_open_the_breaker(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		s := autoAcking()
		ps := publisherSettings()
		ps.MaxAttempts = 1
		tel := humustest.NewTelemetry()
		var aborted []error
		var clean, scenario error
		publishesUnder(t, tel, true, scenarioBaggage(t, abortAccepted), ps, s, func(ctx context.Context, p *rabbitmq.Publisher) {
			// The default policy opens after 5 failures in a row.
			for range 5 {
				aborted = append(aborted, p.Publish(ctx, accepted))
			}
			// A Publish whose context carries no scenario is rejected too.
			clean = p.Publish(baggage.ContextWithoutBaggage(ctx), accepted)
			// An attempt the breaker rejects evaluates nothing.
			scenario = p.Publish(ctx, accepted)
		})

		for _, err := range aborted {
			require.ErrorIs(t, err, fit.ErrInjected)
		}
		require.ErrorIs(t, clean, circuit.ErrRejected)
		require.NotErrorIs(t, clean, fit.ErrInjected)
		require.ErrorIs(t, scenario, circuit.ErrRejected)
		require.Empty(t, s.published)
		require.Zero(t, s.dials())

		spans := spansNamed(tel, sendSpan)
		require.Len(t, spans, 7)
		evaluated := 0
		for _, span := range spans {
			evaluated += len(spanEvents(span, evaluationEvent))
		}
		require.Equal(t, 5, evaluated, "only the attempts the breaker admitted were evaluated")
		require.Equal(t, map[string]int64{"failure": 5, "rejected": 2}, countsBy(t, tel, "humus.circuit.calls", "humus.circuit.outcome"))
		require.Len(t, records(tel.Logs(), "circuit breaker open"), 1)
	})
}

func TestPublisher_a_delay_is_waited_inside_Timeout(t *testing.T) {
	t.Parallel()

	t.Run("the delay passes", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			s := autoAcking()
			tel := humustest.NewTelemetry()
			var err error
			var elapsed time.Duration
			publishesUnder(t, tel, true, scenarioBaggage(t, delayAccepted(2*time.Second)), publisherSettings(), s, func(ctx context.Context, p *rabbitmq.Publisher) {
				start := time.Now()
				err = p.Publish(ctx, accepted)
				elapsed = time.Since(start)
			})

			require.NoError(t, err)
			require.Equal(t, 2*time.Second, elapsed)
			require.Len(t, s.published, 1)
			require.Equal(t, map[string]int64{"success": 1}, countsBy(t, tel, "humus.circuit.calls", "humus.circuit.outcome"))
		})
	})

	t.Run("Timeout cuts it short: a Failure", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			s := autoAcking()
			ps := publisherSettings()
			ps.Timeout = 2 * time.Second
			tel := humustest.NewTelemetry()
			var err error
			var elapsed time.Duration
			publishesUnder(t, tel, true, scenarioBaggage(t, delayAccepted(5*time.Second)), ps, s, func(ctx context.Context, p *rabbitmq.Publisher) {
				start := time.Now()
				err = p.Publish(ctx, accepted)
				elapsed = time.Since(start)
			})

			require.Equal(t, 2*time.Second, elapsed)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotErrorIs(t, err, fit.ErrInjected)
			var injected *fault.InjectedError
			require.ErrorAs(t, err, &injected)
			require.Equal(t, acceptedPoint, injected.Point)
			require.Empty(t, s.published)

			span := onlySpanNamed(t, tel, sendSpan)
			require.Len(t, spanEvents(span, evaluationEvent), 1)
			require.Equal(t, injectedErrorType, spanAttrs(span)["error.type"].AsString())
			require.Equal(t, map[string]int64{"failure": 1}, countsBy(t, tel, "humus.circuit.calls", "humus.circuit.outcome"))
			require.Equal(t, map[string]int64{"expired": 1}, countsBy(t, tel, "humus.retry.decisions", "humus.retry.decision"))
		})
	})

	t.Run("the caller cancels it: Ignored", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			s := autoAcking()
			tel := humustest.NewTelemetry()
			var err error
			var elapsed time.Duration
			publishesUnder(t, tel, true, scenarioBaggage(t, delayAccepted(5*time.Second)), publisherSettings(), s, func(ctx context.Context, p *rabbitmq.Publisher) {
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				time.AfterFunc(time.Second, cancel)
				start := time.Now()
				err = p.Publish(ctx, accepted)
				elapsed = time.Since(start)
			})

			require.Equal(t, time.Second, elapsed)
			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, fit.ErrInjected)
			var injected *fault.InjectedError
			require.ErrorAs(t, err, &injected)
			require.Empty(t, s.published)
			require.Equal(t, map[string]int64{"ignored": 1}, countsBy(t, tel, "humus.circuit.calls", "humus.circuit.outcome"))
			require.Equal(t, map[string]int64{"expired": 1}, countsBy(t, tel, "humus.retry.decisions", "humus.retry.decision"))
		})
	})
}

func TestPublisher_strips_the_scenario_unless_ForwardScenario_is_set(t *testing.T) {
	t.Parallel()

	for _, forward := range []bool{false, true} {
		name := "ForwardScenario false"
		if forward {
			name = "ForwardScenario true"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				s := autoAcking()
				ps := publisherSettings()
				ps.ForwardScenario = forward
				tel := humustest.NewTelemetry()
				var err error
				publishesUnder(t, tel, true, scenarioBaggage(t, sparesAccepted), ps, s, func(ctx context.Context, p *rabbitmq.Publisher) {
					err = p.Publish(ctx, accepted)
				})
				require.NoError(t, err)

				require.Len(t, s.published, 1)
				b := publishedBaggage(t, <-s.published)
				require.Equal(t, "acme", b.Member("tenant").Value(), "every other member goes either way")
				require.Equal(t, forward, b.Member(scenarioKey).Key() != "", "the scenario member")

				span := onlySpanNamed(t, tel, sendSpan)
				require.Len(t, spanEvents(span, evaluationEvent), 1, "the Publish's own context keeps the scenario")
				strips := spanEvents(span, stripEvent)
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

// consumeUnder runs an App whose only Service consumes settings with p, on
// ch, until every delivery ch was given is processed, then stops it with
// SIGTERM and requires it to exit 0. faults turns failure injection on.
func consumeUnder(t *testing.T, tel *humustest.Telemetry, faults bool, settings rabbitmq.Settings, ch *standIn, p queue.ProcessorFunc[rabbitmq.Message]) {
	t.Helper()
	init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := rabbitmq.NewService(s, settings, p)
		if err != nil {
			return nil, err
		}
		rabbitmq.SetOpen(svc, ch.open)
		return []humus.Service{svc}, nil
	}
	proc, status := humustest.Start(t, init, humustest.WithApp(appOptions(t, tel, faults)...))
	synctest.Wait()
	proc.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

// deliveryOf returns a delivery from queueName with id and a baggage
// header of bag.
func deliveryOf(id, userID, bag string) amqp.Delivery {
	return amqp.Delivery{
		MessageId:  id,
		RoutingKey: queueName,
		UserId:     userID,
		Headers:    amqp.Table{"baggage": bag},
	}
}

// processed is what Process saw of one delivery.
type processed struct {
	id       string
	scenario bool
	tenant   string
}

// processedBy returns a Processor that records what it sees in out.
func processedBy(t *testing.T, out *[]processed) queue.ProcessorFunc[rabbitmq.Message] {
	codec := faultsCodec(t)
	return func(ctx context.Context, m rabbitmq.Message) error {
		_, carried, _ := fit.ScenarioFromContext(ctx, codec)
		*out = append(*out, processed{
			id:       m.MessageID,
			scenario: carried,
			tenant:   baggage.FromContext(ctx).Member("tenant").Value(),
		})
		return nil
	}
}

// tenantOnly returns the baggage tenant=acme, as a W3C baggage header's
// value.
func tenantOnly(t *testing.T) string {
	t.Helper()
	m, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(m)
	require.NoError(t, err)
	return b.String()
}

func TestService_strips_the_scenario_of_a_delivery_it_does_not_trust(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		trust    func(m rabbitmq.Message) bool
		scenario bool
	}{
		{name: "TrustScenario nil", trust: nil, scenario: false},
		{name: "TrustScenario trusts the UserID", trust: func(m rabbitmq.Message) bool { return m.UserID == "orders-api" }, scenario: true},
		{name: "TrustScenario trusts another UserID", trust: func(m rabbitmq.Message) bool { return m.UserID == "billing" }, scenario: false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				ch := newStandIn()
				// A delivery with a scenario, then one with none.
				ch.send(
					deliveryOf("order-1", "orders-api", scenarioBaggage(t, abortAccepted)),
					deliveryOf("order-2", "orders-api", tenantOnly(t)),
				)

				s := settings()
				var asked []string
				if tc.trust != nil {
					s.TrustScenario = func(m rabbitmq.Message) bool {
						asked = append(asked, m.MessageID)
						return tc.trust(m)
					}
				}
				var seen []processed
				tel := humustest.NewTelemetry()
				consumeUnder(t, tel, true, s, ch, processedBy(t, &seen))

				require.Equal(t, []processed{
					{id: "order-1", scenario: tc.scenario, tenant: "acme"},
					{id: "order-2", scenario: false, tenant: "acme"},
				}, seen)
				if tc.trust != nil {
					require.Equal(t, []string{"order-1"}, asked, "asked only about the delivery that carries a scenario")
				}

				strips := map[string]int{}
				for _, span := range spansNamed(tel, processSpan) {
					strips[spanAttrs(span)["messaging.message.id"].AsString()] = len(spanEvents(span, stripEvent))
					for _, e := range spanEvents(span, stripEvent) {
						require.Empty(t, e.Attributes)
					}
				}
				want := map[string]int{"order-1": 1, "order-2": 0}
				if tc.scenario {
					want["order-1"] = 0
				}
				require.Equal(t, want, strips)
			})
		})
	}
}

func TestService_does_not_recover_a_panic_in_TrustScenario(t *testing.T) {
	t.Parallel()

	// A panic in TrustScenario crashes the process, as one in a Processor
	// does, which a whole App can't show without crashing the test: so the
	// strip is called on its own, as the delivery's goroutine calls it.
	d := fault.DefaultSettings()
	d.Decoder = faultsCodec(t)
	faults, err := fault.New(d)
	require.NoError(t, err)
	s := settings()
	s.TrustScenario = func(rabbitmq.Message) bool { panic("trust") }
	svc, err := rabbitmq.NewService(&humus.Setup{Host: humustest.NewHost(t).Host(), Telemetry: telemetry.Noop(), Faults: faults}, s, nop)
	require.NoError(t, err)

	b, err := baggage.Parse(scenarioBaggage(t, abortAccepted))
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), b)
	require.PanicsWithValue(t, "trust", func() { rabbitmq.StripDelivery(svc, ctx, rabbitmq.Message{Queue: queueName}) })
}

func TestFaults_off_evaluates_and_strips_nothing(t *testing.T) {
	t.Parallel()

	t.Run("the Publisher", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			s := autoAcking()
			tel := humustest.NewTelemetry()
			var err error
			publishesUnder(t, tel, false, scenarioBaggage(t, abortAccepted), publisherSettings(), s, func(ctx context.Context, p *rabbitmq.Publisher) {
				err = p.Publish(ctx, accepted)
			})
			require.NoError(t, err)

			require.Len(t, s.published, 1)
			b := publishedBaggage(t, <-s.published)
			require.NotEmpty(t, b.Member(scenarioKey).Key(), "the scenario goes as any baggage does")
			span := onlySpanNamed(t, tel, sendSpan)
			require.Empty(t, spanEvents(span, evaluationEvent))
			require.Empty(t, spanEvents(span, stripEvent))
		})
	})

	t.Run("the Service", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			ch := newStandIn()
			ch.send(deliveryOf("order-1", "", scenarioBaggage(t, abortAccepted)))

			s := settings()
			asked := false
			s.TrustScenario = func(rabbitmq.Message) bool {
				asked = true
				return false
			}
			var seen []processed
			tel := humustest.NewTelemetry()
			consumeUnder(t, tel, false, s, ch, processedBy(t, &seen))

			require.Equal(t, []processed{{id: "order-1", scenario: true, tenant: "acme"}}, seen)
			require.False(t, asked, "TrustScenario is never asked")
			require.Empty(t, spanEvents(onlySpanNamed(t, tel, processSpan), stripEvent))
		})
	})
}
