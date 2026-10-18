// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue/kafka"
)

// The tests in this file run the end user's App as the Producer's other
// tests do, recording its telemetry in a humustest.Telemetry, and read
// humus's own instruments of the Producer with its Metrics.

// ownNames are the names of humus's own instruments of the Producer.
var ownNames = []string{"humus.circuit.state", "humus.circuit.transitions", "humus.circuit.calls"}

// collect returns what tel's ManualReader collects now, or once the App has
// exited, what its Shutdown collected.
func collect(t *testing.T, ctx context.Context, tel *humustest.Telemetry) metricdata.ResourceMetrics {
	t.Helper()
	rm, err := tel.Metrics(ctx)
	require.NoError(t, err)
	return rm
}

// ownMetrics returns humus's own instruments of the Producer in rm: the
// metrics of the Producer's scope named one of ownNames.
func ownMetrics(rm metricdata.ResourceMetrics) []metricdata.Metrics {
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != serviceScope {
			continue
		}
		for _, m := range sm.Metrics {
			for _, name := range ownNames {
				if m.Name == name {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

// clusterOf returns the cluster's attributes of a Producer whose first
// broker is broker.
func clusterOf(t *testing.T, broker string) map[string]string {
	t.Helper()
	host, port, err := net.SplitHostPort(broker)
	require.NoError(t, err)
	return map[string]string{
		"messaging.system": "kafka",
		"server.address":   host,
		"server.port":      port,
	}
}

// series returns the data points of the int64 sum named name among
// humus's own instruments in rm, keyed by every attribute, sorted and
// joined, such as humus.circuit.state=open,messaging.system=kafka,....
func series(t *testing.T, rm metricdata.ResourceMetrics, name string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, m := range ownMetrics(rm) {
		if m.Name != name {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		require.True(t, ok, "%s is a %T", name, m.Data)
		for _, dp := range sum.DataPoints {
			out[pairs(setAttrs(dp.Attributes))] += dp.Value
		}
	}
	return out
}

// pairs returns kvs as key=value pairs, sorted and joined with commas.
func pairs(kvs map[string]string) string {
	var out []string
	for k, v := range kvs {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// ownPoints returns the data points of the int64 sum named name among
// humus's own instruments in rm, keyed by their attributes other than the
// cluster's, sorted and joined, such as humus.circuit.state=open. It fails
// t unless every data point carries the cluster's attributes of a Producer
// whose first broker is broker, and nothing else per call.
func ownPoints(t *testing.T, rm metricdata.ResourceMetrics, broker, name string) map[string]int64 {
	t.Helper()
	cluster := clusterOf(t, broker)
	out := map[string]int64{}
	for _, m := range ownMetrics(rm) {
		if m.Name != name {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		require.True(t, ok, "%s is a %T", name, m.Data)
		for _, dp := range sum.DataPoints {
			rest := map[string]string{}
			got := map[string]string{}
			for k, v := range setAttrs(dp.Attributes) {
				if _, ok := cluster[k]; ok {
					got[k] = v
					continue
				}
				require.True(t, strings.HasPrefix(k, "humus."), "a data point of %s with %s", name, k)
				rest[k] = v
			}
			require.Equal(t, cluster, got, "a data point of %s without the cluster's attributes", name)
			out[pairs(rest)] += dp.Value
		}
	}
	return out
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

func TestProducer_metrics_follow_the_breaker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		broker := c.ListenAddrs()[0]
		var failing atomic.Bool
		failing.Store(true)
		answerProduce(c, kerr.NotEnoughReplicas.Code, failing.Load)

		tel := humustest.NewTelemetry()
		ps := producerSettings(c.ListenAddrs()...)
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			require.Equal(t, breakerIs(1, 0, 0), ownPoints(t, collect(t, ctx, tel), broker, "humus.circuit.state"))

			for i := range 5 {
				require.ErrorIs(t, p.Produce(ctx, value(i)), context.DeadlineExceeded, "call %d", i)
			}
			require.ErrorIs(t, p.Produce(ctx, value(5)), circuit.ErrRejected)
			require.Equal(t, breakerIs(0, 1, 0), ownPoints(t, collect(t, ctx, tel), broker, "humus.circuit.state"))

			time.Sleep(ps.Breaker.OpenFor) // and no call
			require.Equal(t, breakerIs(0, 0, 1), ownPoints(t, collect(t, ctx, tel), broker, "humus.circuit.state"),
				"half-open as soon as a probe would be admitted")

			failing.Store(false) // the cluster recovers
			for i := range 3 {
				require.NoError(t, p.Produce(ctx, value(i)), "probe %d", i)
			}
			return nil
		})

		rm := collect(t, context.Background(), tel) // what Shutdown collected, after OnStop
		require.Equal(t, breakerIs(1, 0, 0), ownPoints(t, rm, broker, "humus.circuit.state"),
			"the final collection has the state the App stopped in")
		require.Equal(t, map[string]int64{
			"humus.circuit.previous_state=closed,humus.circuit.state=open":      1,
			"humus.circuit.previous_state=open,humus.circuit.state=half-open":   1,
			"humus.circuit.previous_state=half-open,humus.circuit.state=closed": 1,
		}, ownPoints(t, rm, broker, "humus.circuit.transitions"))
		require.Equal(t, map[string]int64{
			"humus.circuit.outcome=failure":  5,
			"humus.circuit.outcome=rejected": 1,
			"humus.circuit.outcome=success":  3,
		}, ownPoints(t, rm, broker, "humus.circuit.calls"))

		names := metricNames(t, tel, serviceScope)
		require.Subset(t, names, ownNames)
		require.NotContains(t, names, "humus.retry.decisions", "franz-go exposes no retry decision")
	})
}

func TestProducer_metrics_count_one_call_per_Produce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			return p.Produce(ctx, value(0), value(1), value(2))
		})

		rm := collect(t, context.Background(), tel)
		require.Equal(t, map[string]int64{"humus.circuit.outcome=success": 1},
			ownPoints(t, rm, c.ListenAddrs()[0], "humus.circuit.calls"))
		require.Equal(t, int64(3), sum(t, tel, serviceScope, "messaging.client.sent.messages"))
	})
}

func TestProducer_metrics_count_a_rejected_call_once(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		answerProduce(c, kerr.NotEnoughReplicas.Code, always)

		var rejected error
		tel := humustest.NewTelemetry()
		ps := producerSettings(c.ListenAddrs()...)
		ps.BreakerPolicy = tripOnFirstFailure
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			_ = p.Produce(ctx, value(0))
			rejected = p.Produce(ctx, value(1), value(2), value(3))
			return nil
		})
		require.ErrorIs(t, rejected, circuit.ErrRejected)

		require.Equal(t, map[string]int64{
			"humus.circuit.outcome=failure":  1,
			"humus.circuit.outcome=rejected": 1,
		}, ownPoints(t, collect(t, context.Background(), tel), c.ListenAddrs()[0], "humus.circuit.calls"),
			"a call of three records the breaker rejected counts once")
	})
}

func TestProducer_metrics_count_a_topics_own_failure_as_ignored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		var err error
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			err = p.Produce(ctx, kafka.Record{Topic: topic, Value: make([]byte, 1_000_013)}) // larger than ProducerBatchMaxBytes
			return nil
		})
		require.ErrorIs(t, err, kerr.MessageTooLarge)

		rm := collect(t, context.Background(), tel)
		require.Equal(t, map[string]int64{"humus.circuit.outcome=ignored": 1},
			ownPoints(t, rm, c.ListenAddrs()[0], "humus.circuit.calls"))
		require.Empty(t, ownPoints(t, rm, c.ListenAddrs()[0], "humus.circuit.transitions"))
	})
}

func TestProducer_metrics_and_records_carry_the_first_broker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1, kfake.NumBrokers(2))
		answerProduce(c, kerr.NotEnoughReplicas.Code, always)
		addrs := c.ListenAddrs()
		require.Len(t, addrs, 2)
		// The seed the end user names first is the second of kfake's, so
		// the attributes can't come from whichever broker answered first.
		first := addrs[1]

		tel := humustest.NewTelemetry()
		ps := producerSettings(first, addrs[0])
		ps.BreakerPolicy = tripOnFirstFailure
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			_ = p.Produce(ctx, value(0))
			_ = p.Produce(ctx, value(1))
			return nil
		})

		rm := collect(t, context.Background(), tel)
		for _, name := range ownNames {
			require.NotEmpty(t, ownPoints(t, rm, first, name), "%s has a data point", name)
		}

		open := records(tel.Logs(), breakerOpen)
		require.Len(t, open, 1)
		a := attrs(open[0])
		for k, v := range clusterOf(t, first) {
			require.Equal(t, v, a[k], "the record's %s", k)
		}
	})
}

func TestProducer_metrics_of_two_clusters_are_two_series(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		failing := cluster(t, h, 1)
		answerProduce(failing, kerr.NotEnoughReplicas.Code, always)
		healthy := cluster(t, h, 1)
		failingAt, healthyAt := failing.ListenAddrs()[0], healthy.ListenAddrs()[0]
		require.NotEqual(t, failingAt, healthyAt)

		failingSettings := producerSettings(failingAt)
		failingSettings.BreakerPolicy = tripOnFirstFailure

		tel := humustest.NewTelemetry()
		var during map[string]int64
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			fp, err := kafka.NewProducer(s, failingSettings)
			if err != nil {
				return nil, err
			}
			hp, err := kafka.NewProducer(s, producerSettings(healthyAt))
			if err != nil {
				return nil, err
			}
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				_ = fp.Produce(ctx, value(0)) // opens the failing cluster's breaker
				require.NoError(t, hp.Produce(ctx, value(0)))
				during = series(t, collect(t, ctx, tel), "humus.circuit.state")
				return nil
			})}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		key := func(broker, state string) string {
			kvs := clusterOf(t, broker)
			kvs["humus.circuit.state"] = state
			return pairs(kvs)
		}
		require.Equal(t, map[string]int64{
			key(failingAt, "closed"):    0,
			key(failingAt, "open"):      1,
			key(failingAt, "half-open"): 0,
			key(healthyAt, "closed"):    1,
			key(healthyAt, "open"):      0,
			key(healthyAt, "half-open"): 0,
		}, during, "each Producer's callback observes its own breaker")
	})
}

func TestProducer_metrics_callback_is_registered_last(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		ps := producerSettings(c.ListenAddrs()...)
		ps.BreakerPolicy.Probe = nil
		tel := humustest.NewTelemetry()
		_, status := humustest.Start(t, produces(ps, func(context.Context, *kafka.Producer) error {
			require.FailNow(t, "the job ran with a Producer NewProducer should have rejected")
			return nil
		}), humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		st := <-status
		require.NotZero(t, st.Code())
		require.ErrorContains(t, st.Cause(), "ProducerSettings.BreakerPolicy.Probe")

		require.Empty(t, series(t, collect(t, context.Background(), tel), "humus.circuit.state"),
			"a NewProducer that fails leaves no callback behind")
	})
}

func TestProducer_breaker_records_take_the_humus_prefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		var failing atomic.Bool
		failing.Store(true)
		answerProduce(c, kerr.NotEnoughReplicas.Code, failing.Load)

		tel := humustest.NewTelemetry()
		ps := producerSettings(c.ListenAddrs()...)
		ps.BreakerPolicy = tripOnFirstFailure
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			_ = p.Produce(ctx, value(0))
			failing.Store(false)
			time.Sleep(ps.Breaker.OpenFor)
			for i := range 3 {
				require.NoError(t, p.Produce(ctx, value(i)), "probe %d", i)
			}
			return nil
		})

		require.Equal(t, []string{breakerOpen, breakerHalfOpen, breakerClosed}, breakerLogs(tel))
		for _, body := range []string{breakerOpen, breakerHalfOpen, breakerClosed} {
			rs := records(tel.Logs(), body)
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
