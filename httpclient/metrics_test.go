// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
)

// scope is the instrumentation scope of the client's own instruments and
// log records.
const scope = "github.com/z5labs/humus/httpclient"

// collect returns what tel's ManualReader collects now, or once the App has
// exited, what its Shutdown collected.
func collect(t *testing.T, ctx context.Context, tel *humustest.Telemetry) metricdata.ResourceMetrics {
	t.Helper()
	rm, err := tel.Metrics(ctx)
	require.NoError(t, err)
	return rm
}

// ownMetrics returns the metrics in the client's scope of rm.
func ownMetrics(rm metricdata.ResourceMetrics) []metricdata.Metrics {
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name == scope {
			out = append(out, sm.Metrics...)
		}
	}
	return out
}

// points returns the data points of the int64 sum named name in the
// client's scope of rm, keyed by their attributes other than the
// dependency's, sorted and joined, such as humus.circuit.state=open. It
// fails t unless every data point carries the dependency's server.address,
// payments, and server.port, 8443.
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
					require.Equal(t, "payments", kv.Value.AsString())
					address = true
				case "server.port":
					require.EqualValues(t, 8443, kv.Value.AsInt64())
					port = true
				default:
					kvs = append(kvs, string(kv.Key)+"="+kv.Value.String())
				}
			}
			require.True(t, address && port, "a data point of %s without the dependency's attributes: %v", name, dp.Attributes.ToSlice())
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

func TestClient_metrics_follow_the_breaker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		dep := &failing{} // 500 until healthy, a breaker Failure that isn't retried
		stop := startPayments(t, h, h.Host(), ca, dep)
		defer stop()

		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			require.Equal(t, breakerIs(1, 0, 0), state(t, collect(t, ctx, tel)))

			getN(t, ctx, c, 5, "https://payments:8443/")
			_, err := get(ctx, c, "https://payments:8443/")
			require.ErrorIs(t, err, circuit.ErrRejected)
			require.Equal(t, breakerIs(0, 1, 0), state(t, collect(t, ctx, tel)))

			time.Sleep(10 * time.Second) // OpenFor, and no call
			require.Equal(t, breakerIs(0, 0, 1), state(t, collect(t, ctx, tel)), "half-open as soon as a probe would be admitted")

			dep.healthy.Store(true)
			getN(t, ctx, c, 3, "https://payments:8443/")
			return nil
		})
		requireExitZero(t, st)
		require.EqualValues(t, 8, dep.calls.Load())

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
			"humus.retry.decision=stopped":   6, // five 500s and the rejection
			"humus.retry.decision=succeeded": 3,
		}, points(t, rm, "humus.retry.decisions"))
	})
}

func TestClient_metrics_count_a_throttled_retry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, unavailable(&c, ""))
		defer stop()

		settings := paymentsSettings(t, ca)
		settings.RetryBudget.MaxTokens = 3
		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), settings, func(ctx context.Context, cl *http.Client) error {
			// The first call's first 503 takes a token and is retried, its
			// second takes another and is throttled, and the second call's
			// first 503 takes the last.
			for range 2 {
				r, err := get(ctx, cl, "https://payments:8443/charges/1")
				require.NoError(t, err)
				require.Equal(t, http.StatusServiceUnavailable, r.code)
			}
			return nil
		})
		requireExitZero(t, st)
		require.Equal(t, 3, c.count())

		rm := collect(t, context.Background(), tel)
		require.Equal(t, map[string]int64{
			"humus.retry.decision=again":     1,
			"humus.retry.decision=throttled": 2,
		}, points(t, rm, "humus.retry.decisions"))
		require.Equal(t, map[string]int64{"humus.circuit.outcome=failure": 3}, points(t, rm, "humus.circuit.calls"))
	})
}

func TestClient_metrics_count_again_with_no_retry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, unavailable(&c, ""))
		defer stop()

		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, cl *http.Client) error {
			// A POST with no idempotency key isn't safe to repeat.
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://payments:8443/charges", strings.NewReader("{}"))
			if err != nil {
				return err
			}
			resp, err := cl.Do(req)
			if err != nil {
				return err
			}
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			return resp.Body.Close()
		})
		requireExitZero(t, st)
		require.Equal(t, 1, c.count(), "the POST reaches the dependency once")

		require.Equal(t, map[string]int64{
			"humus.retry.decision=again": 1,
		}, points(t, collect(t, context.Background(), tel), "humus.retry.decisions"), "again is what retry decided, not what the client did")
	})
}

func TestClient_metrics_of_two_clients_of_one_origin_add_up(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		stop := startPayments(t, h, h.Host(), ca, &failing{})
		defer stop()

		settings := paymentsSettings(t, ca)
		tel := humustest.NewTelemetry()
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			first, err := httpclient.NewClient(s, settings)
			if err != nil {
				return nil, err
			}
			second, err := httpclient.NewClient(s, settings)
			if err != nil {
				return nil, err
			}
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				getN(t, ctx, first, 5, "https://payments:8443/") // opens the first's breaker
				require.Equal(t, breakerIs(1, 1, 0), state(t, collect(t, ctx, tel)))
				_, err := get(ctx, second, "https://payments:8443/")
				return err
			})}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		requireExitZero(t, <-status)

		rm := collect(t, context.Background(), tel)
		require.Equal(t, breakerIs(1, 1, 0), state(t, rm))
		require.Equal(t, map[string]int64{"humus.circuit.outcome=failure": 6}, points(t, rm, "humus.circuit.calls"))
	})
}

func TestClient_metrics_callback_is_registered_last(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)

		settings := paymentsSettings(t, humustest.NewCA(t))
		settings.BreakerPolicy.Trip = nil
		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), settings, func(context.Context, *http.Client) error {
			require.FailNow(t, "the job ran with a client NewClient should have rejected")
			return nil
		})
		require.NotZero(t, st.Code())
		require.ErrorContains(t, st.Cause(), "Settings.BreakerPolicy.Trip")

		require.Empty(t, state(t, collect(t, context.Background(), tel)), "a NewClient that fails leaves no callback behind")
	})
}

func TestClient_breaker_records_take_the_humus_prefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		dep := &failing{}
		stop := startPayments(t, h, h.Host(), ca, dep)
		defer stop()

		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			getN(t, ctx, c, 5, "https://payments:8443/")
			dep.healthy.Store(true)
			time.Sleep(10 * time.Second) // OpenFor
			getN(t, ctx, c, 3, "https://payments:8443/")
			return nil
		})
		requireExitZero(t, st)

		records := breakerRecords(tel)
		require.Equal(t, []string{breakerOpen, breakerHalfOpen, breakerClosed}, bodies(records))
		for _, r := range records {
			a := recordAttrs(r)
			for _, key := range []string{
				"humus.circuit.previous_state",
				"humus.circuit.successes",
				"humus.circuit.failures",
				"humus.circuit.consecutive_successes",
				"humus.circuit.consecutive_failures",
				"humus.circuit.timed_out",
			} {
				require.Contains(t, a, key, "the record %q", r.Body().AsString())
			}
			for key := range a {
				require.False(t, strings.HasPrefix(key, "circuit."), "the record %q has %s", r.Body().AsString(), key)
			}
		}
	})
}
