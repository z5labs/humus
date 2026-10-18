// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
)

// The tests in this file run the end user's App as client_test.go's do,
// recording its telemetry in a humustest.Telemetry, and read humus's own
// instruments of the Client with its Metrics.

// collect returns what tel's ManualReader collects now, or once the App has
// exited, what its Shutdown collected.
func collect(t *testing.T, ctx context.Context, tel *humustest.Telemetry) metricdata.ResourceMetrics {
	t.Helper()
	rm, err := tel.Metrics(ctx)
	require.NoError(t, err)
	return rm
}

// ownMetrics returns the metrics of rm in the Client's scope.
func ownMetrics(rm metricdata.ResourceMetrics) []metricdata.Metrics {
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name == clientScope {
			out = append(out, sm.Metrics...)
		}
	}
	return out
}

// points returns the data points of the int64 sum named name in the
// Client's scope of rm, keyed by their attributes other than the
// dependency's, sorted and joined, such as humus.circuit.state=open. It
// fails t unless every data point carries the dependency's server.address,
// backend, and server.port, 9090.
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
					require.Equal(t, "backend", kv.Value.AsString())
					address = true
				case "server.port":
					require.EqualValues(t, 9090, kv.Value.AsInt64())
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
		tel := humustest.NewTelemetry()
		var failing atomic.Bool
		failing.Store(true)
		b := &backend{answer: failingUntil(&failing)}

		a := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
			require.Equal(t, breakerIs(1, 0, 0), state(t, collect(t, ctx, tel)))

			for range 5 {
				require.Equal(t, codes.Unavailable, status.Code(emptyCall(ctx, c)))
			}
			require.ErrorIs(t, emptyCall(ctx, c), circuit.ErrRejected)
			require.Equal(t, breakerIs(0, 1, 0), state(t, collect(t, ctx, tel)))

			time.Sleep(10 * time.Second) // OpenFor, and no call
			require.Equal(t, breakerIs(0, 0, 1), state(t, collect(t, ctx, tel)), "half-open as soon as a probe would be admitted")

			failing.Store(false)
			for range 3 {
				require.NoError(t, emptyCall(ctx, c))
			}
			return nil
		})
		a.settings.MaxAttempts = 1
		_, err := a.run(t)
		require.NoError(t, err)
		require.EqualValues(t, 8, b.calls.Load())

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
			// The default Classify retries Unavailable, so with MaxAttempts
			// 1 retry decides each of the five that it was the last attempt
			// allowed. The rejection is stopped.
			"humus.retry.decision=exhausted": 5,
			"humus.retry.decision=stopped":   1,
			"humus.retry.decision=succeeded": 3,
		}, points(t, rm, "humus.retry.decisions"))
	})
}

func TestClient_metrics_count_a_throttled_retry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		b := &backend{answer: unavailable}

		a := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
			// The first call's first Unavailable takes a token and is
			// retried, its second takes another and is throttled, and the
			// second call's first takes the last.
			for range 2 {
				require.Equal(t, codes.Unavailable, status.Code(emptyCall(ctx, c)))
			}
			return nil
		})
		a.settings.RetryBudget.MaxTokens = 3
		_, err := a.run(t)
		require.NoError(t, err)
		require.EqualValues(t, 3, b.calls.Load())

		rm := collect(t, context.Background(), tel)
		require.Equal(t, map[string]int64{
			"humus.retry.decision=again":     1,
			"humus.retry.decision=throttled": 2,
		}, points(t, rm, "humus.retry.decisions"))
		require.Equal(t, map[string]int64{"humus.circuit.outcome=failure": 3}, points(t, rm, "humus.circuit.calls"))
	})
}

func TestClient_metrics_count_streams(t *testing.T) {
	t.Run("a server-streaming RPC that ends with Unavailable after a message counts one failure, once it has ended", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			b := &backend{streamErr: status.Error(codes.Unavailable, "going away")}
			_, err := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
				stream, err := c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{
					ResponseParameters: []*testpb.ResponseParameters{{}},
				})
				if err != nil {
					return err
				}
				if _, err := stream.Recv(); err != nil {
					return err
				}
				require.Empty(t, points(t, collect(t, ctx, tel), "humus.circuit.calls"), "a stream still open counts nothing")
				_, err = stream.Recv()
				return err
			}).run(t)
			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)

			rm := collect(t, context.Background(), tel)
			require.Equal(t, map[string]int64{"humus.circuit.outcome=failure": 1}, points(t, rm, "humus.circuit.calls"))
			require.Equal(t, map[string]int64{"humus.retry.decision=succeeded": 1}, points(t, rm, "humus.retry.decisions"), "the stream's open is one decision")
		})
	})

	t.Run("a server-streaming RPC read to io.EOF counts one success", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			var got int
			_, err := traced(t, tel, &backend{}, streamOne(&got)).run(t)
			require.ErrorIs(t, err, io.EOF)
			require.Equal(t, 1, got)

			rm := collect(t, context.Background(), tel)
			require.Equal(t, map[string]int64{"humus.circuit.outcome=success": 1}, points(t, rm, "humus.circuit.calls"))
			require.Equal(t, map[string]int64{"humus.retry.decision=succeeded": 1}, points(t, rm, "humus.retry.decisions"), "the stream's open is one decision")
		})
	})

	t.Run("a stream whose caller cancels it counts nothing in humus.circuit.calls", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			_, err := traced(t, tel, &backend{}, func(ctx context.Context, c testpb.TestServiceClient) error {
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				// The backend's HalfDuplexCall stays open after its one
				// message, until its caller cancels it.
				stream, err := c.HalfDuplexCall(ctx)
				if err != nil {
					return err
				}
				if _, err := stream.Recv(); err != nil {
					return err
				}
				cancel()
				_, err = stream.Recv()
				return err
			}).run(t)
			require.Equal(t, codes.Canceled, status.Code(err), "err = %v", err)

			rm := collect(t, context.Background(), tel)
			require.Empty(t, points(t, rm, "humus.circuit.calls"))
			require.Equal(t, map[string]int64{"humus.retry.decision=succeeded": 1}, points(t, rm, "humus.retry.decisions"), "the stream's open is one decision")
		})
	})

	t.Run("while the breaker is open, a stream's open counts rejected and decides stopped", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			b := &backend{answer: unavailable}
			var before metricdata.ResourceMetrics
			a := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
				// One failure opens the breaker.
				require.Equal(t, codes.Unavailable, status.Code(emptyCall(ctx, c)))
				before = collect(t, ctx, tel)
				require.Equal(t, breakerIs(0, 1, 0), state(t, before))

				_, err := c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{})
				return err
			})
			a.settings.MaxAttempts = 1
			a.settings.BreakerPolicy.Trip = func(c circuit.Counts) bool { return c.ConsecutiveFailures >= 1 }
			_, err := a.run(t)
			require.ErrorIs(t, err, circuit.ErrRejected)
			require.EqualValues(t, 1, b.calls.Load())

			require.Equal(t, map[string]int64{"humus.circuit.outcome=failure": 1}, points(t, before, "humus.circuit.calls"))
			require.Equal(t, map[string]int64{"humus.retry.decision=exhausted": 1}, points(t, before, "humus.retry.decisions"))

			rm := collect(t, context.Background(), tel)
			require.Equal(t, map[string]int64{
				"humus.circuit.outcome=failure":  1,
				"humus.circuit.outcome=rejected": 1,
			}, points(t, rm, "humus.circuit.calls"))
			require.Equal(t, map[string]int64{
				"humus.retry.decision=exhausted": 1,
				"humus.retry.decision=stopped":   1,
			}, points(t, rm, "humus.retry.decisions"))
		})
	})
}

func TestClient_metrics_of_two_Clients_of_one_address_add_up(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		b := &backend{answer: unavailable}
		settings := settingsFor(ca, "backend:9090")
		settings.MaxAttempts = 1

		tel := humustest.NewTelemetry()
		var during map[string]int64
		_, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			srv, err := grpcserver.NewService(s, grpcserver.Settings{Addr: "backend:9090", TLS: serving(t, ca, "backend")})
			if err != nil {
				return nil, err
			}
			testpb.RegisterTestServiceServer(srv, b)

			first, err := grpcclient.NewClient(s, settings)
			if err != nil {
				return nil, err
			}
			second, err := grpcclient.NewClient(s, settings)
			if err != nil {
				return nil, err
			}
			j := job.NewService(s, func(ctx context.Context) error {
				c := testpb.NewTestServiceClient(first)
				for range 5 { // opens the first's breaker
					require.Equal(t, codes.Unavailable, status.Code(emptyCall(ctx, c)))
				}
				during = state(t, collect(t, ctx, tel))
				require.Equal(t, codes.Unavailable, status.Code(emptyCall(ctx, testpb.NewTestServiceClient(second))))
				return nil
			})
			return []humus.Service{srv, j}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.Equal(t, breakerIs(1, 1, 0), during)
		rm := collect(t, context.Background(), tel)
		require.Equal(t, breakerIs(1, 1, 0), state(t, rm))
		require.Equal(t, map[string]int64{"humus.circuit.outcome=failure": 6}, points(t, rm, "humus.circuit.calls"))
	})
}

func TestClient_metrics_callback_is_registered_last(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		settings := settingsFor(humustest.NewCA(t), "backend:9090")

		tel := humustest.NewTelemetry()
		_, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			// A default service config that isn't JSON fails grpc-go's
			// NewClient, after the instruments and the breaker are built.
			_, err := grpcclient.NewClient(s, settings, grpc.WithDefaultServiceConfig("not json"))
			if err == nil {
				require.FailNow(t, "NewClient accepted a service config that isn't JSON")
			}
			return nil, err
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		st := <-exit
		require.NotZero(t, st.Code())
		require.ErrorContains(t, st.Cause(), "grpcclient: NewClient:")

		require.Empty(t, state(t, collect(t, context.Background(), tel)), "a NewClient that fails leaves no callback behind")
	})
}

func TestClient_breaker_records_take_the_humus_prefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		var failing atomic.Bool
		failing.Store(true)
		b := &backend{answer: failingUntil(&failing)}

		a := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
			for range 5 {
				_ = emptyCall(ctx, c)
			}
			failing.Store(false)
			time.Sleep(10 * time.Second) // OpenFor
			for range 3 {
				require.NoError(t, emptyCall(ctx, c))
			}
			return nil
		})
		a.settings.MaxAttempts = 1
		_, err := a.run(t)
		require.NoError(t, err)

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
