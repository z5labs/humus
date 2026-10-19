// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// The tests in this file run the Publisher on the stand-in of
// publisher_standin_test.go, inside testing/synctest, recording its
// telemetry in a humustest.Telemetry, and read humus's own instruments of
// the Publisher with its Metrics.

// collect returns what tel's ManualReader collects now, or once the App has
// exited, what its Shutdown collected.
func collect(t *testing.T, ctx context.Context, tel *humustest.Telemetry) metricdata.ResourceMetrics {
	t.Helper()
	rm, err := tel.Metrics(ctx)
	require.NoError(t, err)
	return rm
}

// ownMetrics returns the metrics of rm in the package's scope.
func ownMetrics(rm metricdata.ResourceMetrics) []metricdata.Metrics {
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name == serviceScope {
			out = append(out, sm.Metrics...)
		}
	}
	return out
}

// ownNames returns the names of the metrics of rm in the package's scope
// that start with humus., sorted.
func ownNames(rm metricdata.ResourceMetrics) []string {
	var out []string
	for _, m := range ownMetrics(rm) {
		if strings.HasPrefix(m.Name, "humus.") {
			out = append(out, m.Name)
		}
	}
	sort.Strings(out)
	return out
}

// points returns the data points of the int64 sum named name in the
// package's scope of rm, keyed by their attributes other than the broker's,
// sorted and joined, such as humus.circuit.state=open. It fails t unless
// every data point carries Connection.Addr's server.address, rabbit, and
// server.port, 5672, from publisherSettings(), and only attributes of
// humus's own besides.
func points(t *testing.T, rm metricdata.ResourceMetrics, name string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, m := range ownMetrics(rm) {
		if m.Name != name {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		require.True(t, ok, "%s is a %T", name, m.Data)
		for _, dp := range sum.DataPoints {
			var kvs []string
			var address, port bool
			for _, kv := range dp.Attributes.ToSlice() {
				switch kv.Key {
				case "server.address":
					require.Equal(t, "rabbit", kv.Value.AsString())
					address = true
				case "server.port":
					require.EqualValues(t, 5672, kv.Value.AsInt64())
					port = true
				default:
					require.True(t, strings.HasPrefix(string(kv.Key), "humus."), "a data point of %s with %s", name, kv.Key)
					kvs = append(kvs, string(kv.Key)+"="+kv.Value.String())
				}
			}
			require.True(t, address && port, "a data point of %s without the broker's attributes: %v", name, dp.Attributes.ToSlice())
			sort.Strings(kvs)
			out[strings.Join(kvs, ",")] += dp.Value
		}
	}
	return out
}

// state returns humus.circuit.state's data points in rm.
func state(t *testing.T, rm metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	return points(t, rm, "humus.circuit.state")
}

// breakerIs is humus.circuit.state's data points for breakers, of which
// the given number are in each state.
func breakerIs(closed, open, halfOpen int64) map[string]int64 {
	return map[string]int64{
		"humus.circuit.state=closed":    closed,
		"humus.circuit.state=open":      open,
		"humus.circuit.state=half-open": halfOpen,
	}
}

// publishesWith runs an App whose telemetry is tel's, whose Init builds a
// Publisher of settings on s, and whose one Service, a job, calls run with
// it. It fails t unless the App exits 0.
func publishesWith(t *testing.T, tel *humustest.Telemetry, s *pubStandIn, settings rabbitmq.PublisherSettings, run func(ctx context.Context, pub *rabbitmq.Publisher) error) {
	t.Helper()
	_, status := humustest.Start(t, func(_ context.Context, setup *humus.Setup) ([]humus.Service, error) {
		p, err := rabbitmq.NewPublisher(setup, settings)
		if err != nil {
			return nil, err
		}
		rabbitmq.SetDial(p, s.dial)
		return []humus.Service{job.NewService(setup, func(ctx context.Context) error { return run(ctx, p) })}, nil
	}, humustest.WithApp(humus.WithTelemetry(tel.Builder())))
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

func TestPublisher_metrics_follow_the_breaker(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		s := newPubStandIn()
		s.autoNack = true
		settings := publisherSettings()
		settings.MaxAttempts = 1

		publishesWith(t, tel, s, settings, func(ctx context.Context, pub *rabbitmq.Publisher) error {
			require.Equal(t, breakerIs(1, 0, 0), state(t, collect(t, ctx, tel)))

			for i := range 5 {
				require.ErrorContains(t, pub.Publish(ctx, accepted), "the broker nacked the message", "publish %d", i)
			}
			require.ErrorIs(t, pub.Publish(ctx, accepted), circuit.ErrRejected)
			require.Equal(t, breakerIs(0, 1, 0), state(t, collect(t, ctx, tel)))

			time.Sleep(settings.Breaker.OpenFor) // and no publish
			rm := collect(t, ctx, tel)
			require.Equal(t, breakerIs(0, 0, 1), state(t, rm), "half-open as soon as a probe would be admitted")
			// Reading the state changes nothing: the breaker turns half-open
			// at the next attempt, not in the collection.
			require.Equal(t, map[string]int64{
				"humus.circuit.previous_state=closed,humus.circuit.state=open": 1,
			}, points(t, rm, "humus.circuit.transitions"))
			require.Equal(t, []string{breakerOpen}, breakerLogs(tel))

			// The broker recovers.
			s.autoNack = false
			s.autoAck = true
			for i := range 3 {
				require.NoError(t, pub.Publish(ctx, accepted), "probe %d", i)
			}
			return nil
		})
		require.Len(t, s.published, 8, "the rejected publish sent nothing")

		rm := collect(t, context.Background(), tel) // what Shutdown collected, after OnStop
		require.Equal(t, breakerIs(1, 0, 0), state(t, rm), "the final collection has the state the App stopped in")
		require.Equal(t, map[string]int64{
			"humus.circuit.previous_state=closed,humus.circuit.state=open":      1,
			"humus.circuit.previous_state=open,humus.circuit.state=half-open":   1,
			"humus.circuit.previous_state=half-open,humus.circuit.state=closed": 1,
		}, points(t, rm, "humus.circuit.transitions"))
		require.Equal(t, map[string]int64{
			"humus.circuit.outcome=failure":  5,
			"humus.circuit.outcome=rejected": 1,
			"humus.circuit.outcome=success":  3,
		}, points(t, rm, "humus.circuit.calls"))
		require.Equal(t, map[string]int64{
			// classify retries a nack, so with MaxAttempts 1 retry decides
			// each of the five that it was the last attempt allowed. The
			// rejection is stopped.
			"humus.retry.decision=exhausted": 5,
			"humus.retry.decision=stopped":   1,
			"humus.retry.decision=succeeded": 3,
		}, points(t, rm, "humus.retry.decisions"))
		require.Equal(t, []string{
			"humus.circuit.calls",
			"humus.circuit.state",
			"humus.circuit.transitions",
			"humus.retry.decisions",
		}, ownNames(rm))
	})
}

func TestPublisher_metrics_count_a_throttled_retry(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.RetryBudget.MaxTokens = 3
		tp := newTracedPublisherOf(t, settings)
		tp.s.autoNack = true

		// The first Publish's first nack takes a token and is retried, its
		// second takes another and is throttled, and the second Publish's
		// first takes the last.
		for i := range 2 {
			require.ErrorContains(t, tp.pub.Publish(t.Context(), accepted), "the broker nacked the message", "publish %d", i)
		}
		require.Len(t, tp.s.published, 3)

		rm := collect(t, t.Context(), tp.tel)
		require.Equal(t, map[string]int64{
			"humus.retry.decision=again":     1,
			"humus.retry.decision=throttled": 2,
		}, points(t, rm, "humus.retry.decisions"))
		require.Equal(t, map[string]int64{"humus.circuit.outcome=failure": 3}, points(t, rm, "humus.circuit.calls"))
	})
}

func TestPublisher_metrics_count_a_UserID_the_broker_refused_a_success_and_stopped(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisherOf(t, publisherSettings())
		m := accepted
		m.UserID = "billing"
		done := publishAsync(t.Context(), tp.pub, m)
		p := <-tp.s.published
		require.Equal(t, "billing", p.msg.UserId)
		p.ch.fail(&amqp.Error{Code: amqp.PreconditionFailed, Reason: "PRECONDITION_FAILED - user_id property set to 'billing' but authenticated user was 'orders-api'", Server: true})
		ae, ok := errors.AsType[*amqp.Error](<-done)
		require.True(t, ok)
		require.Equal(t, amqp.PreconditionFailed, ae.Code)
		requireNoMorePublishes(t, tp.s)

		rm := collect(t, t.Context(), tp.tel)
		require.Equal(t, map[string]int64{"humus.circuit.outcome=success": 1}, points(t, rm, "humus.circuit.calls"),
			"the broker answered, so the breaker counts a success")
		require.Equal(t, map[string]int64{"humus.retry.decision=stopped": 1}, points(t, rm, "humus.retry.decisions"),
			"a channel error is never retried")
	})
}

func TestPublisher_metrics_of_two_Publishers_of_one_broker_add_up(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		nacking := newPubStandIn()
		nacking.autoNack = true
		acking := newPubStandIn()
		acking.autoAck = true
		settings := publisherSettings()
		settings.MaxAttempts = 1
		settings.BreakerPolicy = tripOnFirstFailure

		tel := humustest.NewTelemetry()
		var during map[string]int64
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			first, err := rabbitmq.NewPublisher(s, settings)
			if err != nil {
				return nil, err
			}
			rabbitmq.SetDial(first, nacking.dial)
			second, err := rabbitmq.NewPublisher(s, settings)
			if err != nil {
				return nil, err
			}
			rabbitmq.SetDial(second, acking.dial)
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				// opens the first's breaker
				require.ErrorContains(t, first.Publish(ctx, accepted), "the broker nacked the message")
				require.NoError(t, second.Publish(ctx, accepted))
				during = state(t, collect(t, ctx, tel))
				return nil
			})}, nil
		}, humustest.WithApp(humus.WithTelemetry(tel.Builder())))
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.Equal(t, breakerIs(1, 1, 0), during, "each Publisher's callback observes its own breaker")
		rm := collect(t, context.Background(), tel)
		require.Equal(t, breakerIs(1, 1, 0), state(t, rm))
		require.Equal(t, map[string]int64{
			"humus.circuit.outcome=failure": 1,
			"humus.circuit.outcome=success": 1,
		}, points(t, rm, "humus.circuit.calls"))
	})
}

func TestPublisher_metrics_every_data_point_carries_the_broker(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		s := newPubStandIn()
		s.autoNack = true
		settings := publisherSettings()
		settings.MaxAttempts = 1
		settings.BreakerPolicy = tripOnFirstFailure

		publishesWith(t, tel, s, settings, func(ctx context.Context, pub *rabbitmq.Publisher) error {
			_ = pub.Publish(ctx, accepted)
			_ = pub.Publish(ctx, rabbitmq.Publishing{Exchange: "invoices", RoutingKey: "paid"})
			return nil
		})

		rm := collect(t, context.Background(), tel)
		// points fails unless every data point has server.address and
		// server.port, and nothing else but humus's own attributes: no
		// exchange and no routing key.
		for _, name := range []string{"humus.circuit.state", "humus.circuit.transitions", "humus.circuit.calls", "humus.retry.decisions"} {
			require.NotEmpty(t, points(t, rm, name), "%s has a data point", name)
		}
	})
}

func TestPublisher_metrics_callback_is_registered_last(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.BreakerPolicy.Probe = nil

		tel := humustest.NewTelemetry()
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			_, err := rabbitmq.NewPublisher(s, settings)
			if err == nil {
				require.FailNow(t, "NewPublisher accepted a BreakerPolicy with no Probe")
			}
			return nil, err
		}, humustest.WithApp(humus.WithTelemetry(tel.Builder())))
		st := <-status
		require.NotZero(t, st.Code())
		require.ErrorContains(t, st.Cause(), "PublisherSettings.BreakerPolicy.Probe")

		require.Empty(t, state(t, collect(t, context.Background(), tel)), "a NewPublisher that fails leaves no callback behind")
	})
}

func TestPublisher_breaker_records_take_the_humus_prefix(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.MaxAttempts = 1
		settings.BreakerPolicy = tripOnFirstFailure
		tp := newTracedPublisherOf(t, settings)
		tp.s.autoNack = true

		require.ErrorContains(t, tp.pub.Publish(t.Context(), accepted), "the broker nacked the message")
		tp.s.autoNack = false
		tp.s.autoAck = true
		time.Sleep(settings.Breaker.OpenFor)
		require.NoError(t, tp.pub.Publish(t.Context(), accepted))

		require.Equal(t, []string{breakerOpen, breakerHalfOpen, breakerClosed}, breakerLogs(tp.tel))
		for _, body := range []string{breakerOpen, breakerHalfOpen, breakerClosed} {
			rs := records(tp.tel.Logs(), body)
			require.Len(t, rs, 1)
			a := attrs(rs[0])
			for _, key := range []string{
				"humus.circuit.previous_state",
				"humus.circuit.successes",
				"humus.circuit.failures",
				"humus.circuit.consecutive_successes",
				"humus.circuit.consecutive_failures",
				"humus.circuit.timed_out",
			} {
				require.Contains(t, a, key, "the record %q", body)
			}
			for key := range a {
				require.False(t, strings.HasPrefix(key, "circuit."), "the record %q has %s", body, key)
			}
		}
	})
}

func TestService_records_no_retry_decisions(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), nop)
		var (
			mu    sync.Mutex
			opens int
		)
		rabbitmq.SetOpen(u.svc, func(ctx context.Context) (rabbitmq.Channel, io.Closer, error) {
			mu.Lock()
			opens++
			n := opens
			mu.Unlock()
			if n == 1 {
				// The first attempt to connect fails, and is retried.
				return nil, nil, fmt.Errorf("dialing rabbit:5672: %w", syscall.ECONNREFUSED)
			}
			return u.open(ctx)
		})
		served := u.serve(t.Context())
		synctest.Wait()
		time.Sleep(time.Minute) // past the wait after the failed attempt
		synctest.Wait()
		require.Len(t, records(u.tel.Logs(), "connecting failed"), 1)

		u.ch.lose(forced) // and the Service connects again
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		u.channel(t, 1)

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)

		require.Empty(t, ownNames(collect(t, t.Context(), u.tel)), "the Service records none of humus's own instruments")
	})
}

// This test sets OpenTelemetry's global MeterProvider, so it doesn't run in
// parallel. It sets it back when it ends, but the delegates OpenTelemetry
// handed out before the first Set stay bound to this test's recorder for
// the rest of the process: harmless, since the package uses no globals.
func TestPublisher_metrics_use_no_OpenTelemetry_globals(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	//nolint:forbidigo // The test sets OpenTelemetry's global MeterProvider to a recorder of its own, to show the Publisher never uses it, as the RabbitMQ proposal, #765, requires.
	{
		prev := otel.GetMeterProvider()
		t.Cleanup(func() { otel.SetMeterProvider(prev) })
		otel.SetMeterProvider(mp)
	}

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisher(t)
		tp.s.autoAck = true
		require.NoError(t, tp.pub.Publish(t.Context(), accepted))

		require.Equal(t, map[string]int64{"humus.circuit.outcome=success": 1}, points(t, collect(t, t.Context(), tp.tel), "humus.circuit.calls"),
			"the Setup's MeterProvider recorded the attempt the breaker counted")
	})

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
}
