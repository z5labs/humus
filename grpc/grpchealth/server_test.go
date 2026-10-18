// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpchealth_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on a humustest Host.
// Most register NewServer's server on a grpcserver.Service of their own,
// served over TLS on carts:9090 with a certificate from a humustest CA. The
// tests of Watch through SIGTERM and Shutdown, and of the globals, run
// whole Apps with grpcservertest.Start.

// scope is the instrumentation scope of the server's logger.
const scope = "github.com/z5labs/humus/grpc/grpchealth"

// errSecret is what a failing monitor returns. It names a host and a user,
// which an answer must never hold.
var errSecret = errors.New("dial tcp db.internal:5432: password authentication failed for user orders")

// result returns a monitor that always returns healthy and err.
func result(healthy bool, err error) health.Monitor {
	return health.MonitorFunc(func(context.Context) (bool, error) {
		return healthy, err
	})
}

// healthyMonitor is a monitor that is always healthy.
var healthyMonitor = result(true, nil)

// sdkOf returns the SDK tel records.
func sdkOf(t *testing.T, tel *humustest.Telemetry) telemetry.SDK {
	t.Helper()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	return sdk
}

// cartService is the desc of the end user's own service, which the tests
// register next to the health server. It has no methods: only its name
// matters.
var cartService = &grpc.ServiceDesc{
	ServiceName: "cart.v1.CartService",
	HandlerType: (*any)(nil),
}

// tlsFor returns a server's TLS config with a certificate for carts from
// ca.
func tlsFor(t *testing.T, ca *humustest.CA) *tls.Config {
	t.Helper()
	return &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "carts").TLS()}}
}

// serve registers NewServer's server for p, and cart.v1.CartService, on a
// grpcserver.Service on carts:9090 of a new humustest Host, whose telemetry
// is sdk, serves it until the test ends, and returns a client of it.
func serve(t *testing.T, sdk telemetry.SDK, p health.Probes) healthpb.HealthClient {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	s := &humus.Setup{Host: h.Host(), Telemetry: sdk}
	svc, err := grpcserver.NewService(s, grpcserver.Settings{
		Addr: ":9090",
		TLS:  tlsFor(t, ca),
	})
	require.NoError(t, err)
	svc.RegisterService(cartService, struct{}{})
	healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, p))

	served := make(chan error, 1)
	go func() { served <- svc.Serve(t.Context()) }()
	t.Cleanup(func() {
		require.NoError(t, svc.Shutdown(context.Background()))
		require.NoError(t, <-served)
	})
	return healthpb.NewHealthClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
}

// errorRecords returns the ERROR records of tel emitted by the server's
// logger.
func errorRecords(tel *humustest.Telemetry) []sdklog.Record {
	var rs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == scope && r.Severity() == log.SeverityError {
			rs = append(rs, r)
		}
	}
	return rs
}

// attr returns the value of r's attribute key, and whether it has one.
func attr(r sdklog.Record, key string) (attribute.Value, bool) {
	var v attribute.Value
	found := false
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == key {
			v, found = kv.Value, true
			return false
		}
		return true
	})
	return v, found
}

// requireErrorRecord fails t unless r is the record of errSecret on probe.
func requireErrorRecord(t *testing.T, r sdklog.Record, probe string) {
	t.Helper()
	require.Equal(t, errSecret.Error(), r.Body().AsString())
	require.Equal(t, "ERROR", r.SeverityText())
	v, ok := attr(r, "health.probe")
	require.True(t, ok, "the record has no health.probe")
	require.Equal(t, probe, v.AsString())
}

// probesWith returns Probes whose monitor for name, "", "readiness" or
// "liveness", is m, and whose other monitor is healthy.
func probesWith(name string, m health.Monitor) health.Probes {
	if name == "liveness" {
		return health.Probes{Liveness: m, Readiness: healthyMonitor}
	}
	return health.Probes{Liveness: healthyMonitor, Readiness: m}
}

// probeOf is the probe a name answers.
func probeOf(name string) string {
	if name == "liveness" {
		return "liveness"
	}
	return "readiness"
}

func TestNewServer_Check_answers_each_name(t *testing.T) {
	monitors := []struct {
		name    string
		monitor health.Monitor
		want    healthpb.HealthCheckResponse_ServingStatus
	}{
		{"a healthy monitor", healthyMonitor, healthpb.HealthCheckResponse_SERVING},
		{"an unhealthy monitor", result(false, nil), healthpb.HealthCheckResponse_NOT_SERVING},
		{"a monitor that returns an error", result(false, errSecret), healthpb.HealthCheckResponse_NOT_SERVING},
		{"a healthy monitor that returns an error", result(true, errSecret), healthpb.HealthCheckResponse_NOT_SERVING},
	}

	for _, name := range []string{"", "readiness", "liveness"} {
		for _, m := range monitors {
			t.Run(fmt.Sprintf("%q, %s", name, m.name), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					client := serve(t, telemetry.Noop(), probesWith(name, m.monitor))

					resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: name})
					require.NoError(t, err)
					require.Equal(t, m.want, resp.GetStatus())

					// The other probe's monitor is healthy, so the name
					// answered from the monitor it names.
					other := "liveness"
					if name == "liveness" {
						other = "readiness"
					}
					resp, err = client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: other})
					require.NoError(t, err)
					require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
				})
			})
		}
	}
}

func TestNewServer_Check_fails_any_other_name_with_NotFound(t *testing.T) {
	for _, name := range []string{"cart.v1.CartService", "grpc.health.v1.Health", "startup", "Readiness", "unknown"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var checks atomic.Int64
				counting := health.MonitorFunc(func(context.Context) (bool, error) { checks.Add(1); return true, nil })
				client := serve(t, telemetry.Noop(), health.Probes{Liveness: counting, Readiness: counting})

				_, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: name})
				require.Equal(t, codes.NotFound, status.Code(err), err)
				require.Zero(t, checks.Load(), "an unknown name checked a monitor")
			})
		})
	}
}

func TestNewServer_Check_checks_with_the_RPCs_context(t *testing.T) {
	t.Run("and adds no deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			deadlines := make(chan bool, 1)
			m := health.MonitorFunc(func(ctx context.Context) (bool, error) {
				_, ok := ctx.Deadline()
				deadlines <- ok
				return true, nil
			})
			client := serve(t, telemetry.Noop(), health.Probes{Liveness: m, Readiness: healthyMonitor})

			_, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "liveness"})
			require.NoError(t, err)
			require.False(t, <-deadlines, "the server added a deadline to an RPC without one")
		})
	})

	t.Run("whose deadline is the caller's", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			deadlines := make(chan time.Time, 1)
			m := health.MonitorFunc(func(ctx context.Context) (bool, error) {
				d, _ := ctx.Deadline()
				deadlines <- d
				return true, nil
			})
			client := serve(t, telemetry.Noop(), health.Probes{Liveness: healthyMonitor, Readiness: m})

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			want, _ := ctx.Deadline()
			_, err := client.Check(ctx, &healthpb.HealthCheckRequest{Service: "readiness"})
			require.NoError(t, err)
			got := <-deadlines
			require.WithinDuration(t, want, got, time.Millisecond)
		})
	})

	t.Run("which a blocked monitor waits on until the caller gives up", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			returned := make(chan error, 1)
			blocked := health.MonitorFunc(func(ctx context.Context) (bool, error) {
				<-ctx.Done()
				returned <- ctx.Err()
				return false, ctx.Err()
			})
			client := serve(t, telemetry.Noop(), health.Probes{Liveness: healthyMonitor, Readiness: blocked})

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			begin := time.Now()
			_, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
			require.Equal(t, codes.DeadlineExceeded, status.Code(err), err)
			require.Equal(t, 30*time.Second, time.Since(begin))
			require.Error(t, <-returned)
		})
	})
}

func TestNewServer_Check_logs_a_monitors_error(t *testing.T) {
	for _, name := range []string{"", "readiness", "liveness"} {
		t.Run(fmt.Sprintf("as one ERROR record in the RPC's context, for %q", name), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := humustest.NewTelemetry()
				client := serve(t, sdkOf(t, tel), probesWith(name, result(false, errSecret)))

				resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: name})
				require.NoError(t, err)
				require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, resp.GetStatus())
				require.NotContains(t, resp.String(), "db.internal")

				rs := errorRecords(tel)
				require.Len(t, rs, 1)
				requireErrorRecord(t, rs[0], probeOf(name))
				require.Len(t, tel.Logs(), 1, "the server logged more than the error")

				// The record is in the RPC's context: it carries the trace
				// of the span the Service's instrumentation started for
				// the probe.
				spans := serverSpans(tel)
				require.Len(t, spans, 1, "the probe is traced like any RPC of the Service")
				require.Equal(t, "grpc.health.v1.Health/Check", spans[0].Name())
				require.Equal(t, spans[0].SpanContext().TraceID(), rs[0].TraceID())
				require.Equal(t, spans[0].SpanContext().SpanID(), rs[0].SpanID())
			})
		})
	}

	t.Run("and nothing for an unhealthy monitor without one", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			client := serve(t, sdkOf(t, tel), health.Probes{Liveness: result(false, nil), Readiness: result(false, nil)})

			for _, name := range []string{"", "readiness", "liveness"} {
				resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: name})
				require.NoError(t, err)
				require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, resp.GetStatus())
			}
			require.Empty(t, tel.Logs())
		})
	})
}

// serverSpans returns the spans of tel that otelgrpc recorded.
func serverSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range tel.Spans() {
		if s.InstrumentationScope().Name == otelgrpc.ScopeName {
			spans = append(spans, s)
		}
	}
	return spans
}

func TestNewServer_List_answers_the_three_names(t *testing.T) {
	for _, tc := range []struct {
		name            string
		probes          health.Probes
		live, ready     healthpb.HealthCheckResponse_ServingStatus
		errorRecordsFor []string
	}{
		{
			name:   "both healthy",
			probes: health.Probes{Liveness: healthyMonitor, Readiness: healthyMonitor},
			live:   healthpb.HealthCheckResponse_SERVING,
			ready:  healthpb.HealthCheckResponse_SERVING,
		},
		{
			name:   "not ready",
			probes: health.Probes{Liveness: healthyMonitor, Readiness: result(false, nil)},
			live:   healthpb.HealthCheckResponse_SERVING,
			ready:  healthpb.HealthCheckResponse_NOT_SERVING,
		},
		{
			name:            "not live, with an error",
			probes:          health.Probes{Liveness: result(true, errSecret), Readiness: healthyMonitor},
			live:            healthpb.HealthCheckResponse_NOT_SERVING,
			ready:           healthpb.HealthCheckResponse_SERVING,
			errorRecordsFor: []string{"liveness"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := humustest.NewTelemetry()
				client := serve(t, sdkOf(t, tel), tc.probes)

				resp, err := client.List(t.Context(), &healthpb.HealthListRequest{})
				require.NoError(t, err)
				got := make(map[string]healthpb.HealthCheckResponse_ServingStatus)
				for name, st := range resp.GetStatuses() {
					got[name] = st.GetStatus()
				}
				require.Equal(t, map[string]healthpb.HealthCheckResponse_ServingStatus{
					"":          tc.ready,
					"readiness": tc.ready,
					"liveness":  tc.live,
				}, got)

				rs := errorRecords(tel)
				require.Len(t, rs, len(tc.errorRecordsFor))
				for i, probe := range tc.errorRecordsFor {
					requireErrorRecord(t, rs[i], probe)
				}
			})
		})
	}
}

// watch starts a Watch of name with client, and returns the stream and the
// function that ends it from the client's side.
func watch(t *testing.T, client healthpb.HealthClient, name string) (healthpb.Health_WatchClient, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stream, err := client.Watch(ctx, &healthpb.HealthCheckRequest{Service: name})
	require.NoError(t, err)
	return stream, cancel
}

// recv receives the next status from stream.
func recv(t *testing.T, stream healthpb.Health_WatchClient) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	resp, err := stream.Recv()
	require.NoError(t, err)
	return resp.GetStatus()
}

// received starts receiving from stream in a goroutine of its own, and
// returns the channel each status arrives on. The channel is closed when
// the stream ends, after its error is sent on errs.
func received(stream healthpb.Health_WatchClient) (<-chan healthpb.HealthCheckResponse_ServingStatus, <-chan error) {
	statuses := make(chan healthpb.HealthCheckResponse_ServingStatus, 100)
	errs := make(chan error, 1)
	go func() {
		defer close(statuses)
		for {
			resp, err := stream.Recv()
			if err != nil {
				errs <- err
				return
			}
			statuses <- resp.GetStatus()
		}
	}()
	return statuses, errs
}

// drain returns every status on statuses that has arrived.
func drain(statuses <-chan healthpb.HealthCheckResponse_ServingStatus) []healthpb.HealthCheckResponse_ServingStatus {
	var got []healthpb.HealthCheckResponse_ServingStatus
	for {
		select {
		case st, ok := <-statuses:
			if !ok {
				return got
			}
			got = append(got, st)
		default:
			return got
		}
	}
}

// watchesInit is an Init that registers NewServer's server on a
// grpcserver.Service on :9090, with settings' TLS, answering from a health
// Service's Probes.
func watchesInit(settings grpcserver.Settings) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, settings)
		if err != nil {
			return nil, err
		}
		hs := health.NewService()
		healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, hs.Probes()))
		return []humus.Service{svc, hs}, nil
	}
}

func TestNewServer_Watch_follows_a_health_Service_through_shutdown(t *testing.T) {
	for _, name := range []string{"", "readiness"} {
		t.Run(fmt.Sprintf("%q: SERVING at once, NOT_SERVING within a second of SIGTERM, Unavailable at Shutdown", name), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				settings := grpcserver.Settings{
					Addr: ":9090",
					TLS:  tlsFor(t, ca),
				}
				p, exit := grpcservertest.Start(t, watchesInit(settings), humustest.WithApp(humus.WithHost(h.Host())))
				<-h.Listening("carts:9090")
				synctest.Wait() // every Service is serving

				client := healthpb.NewHealthClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
				stream, _ := watch(t, client, name)
				require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, stream))

				<-p.Notified(syscall.SIGTERM)
				begin := time.Now()
				p.Signal(syscall.SIGTERM)
				require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, recv(t, stream))
				require.LessOrEqual(t, time.Since(begin), time.Second, "NOT_SERVING came later than a second after SIGTERM")

				_, err := stream.Recv()
				require.Equal(t, codes.Unavailable, status.Code(err), err)
				require.Equal(t, 5*time.Second, time.Since(begin), "the stream ended before Shutdown") // the drain, then Shutdown

				st := <-exit
				require.Zero(t, st.Code(), "cause: %v", st.Cause())
			})
		})
	}

	t.Run(`"liveness" stays SERVING through the drain`, func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := grpcserver.Settings{
				Addr: ":9090",
				TLS:  tlsFor(t, ca),
			}
			p, exit := grpcservertest.Start(t, watchesInit(settings), humustest.WithApp(humus.WithHost(h.Host())))
			<-h.Listening("carts:9090")
			synctest.Wait()

			client := healthpb.NewHealthClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
			stream, _ := watch(t, client, "liveness")
			statuses, errs := received(stream)

			<-p.Notified(syscall.SIGTERM)
			p.Signal(syscall.SIGTERM)
			st := <-exit
			require.Zero(t, st.Code(), "cause: %v", st.Cause())

			require.Equal(t, codes.Unavailable, status.Code(<-errs))
			require.Equal(t, []healthpb.HealthCheckResponse_ServingStatus{healthpb.HealthCheckResponse_SERVING}, drain(statuses))
		})
	})
}

func TestNewServer_Watch_sends_each_change(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ready health.Binary
		client := serve(t, telemetry.Noop(), health.Probes{Liveness: healthyMonitor, Readiness: &ready})

		stream, _ := watch(t, client, "readiness")
		statuses, _ := received(stream)
		synctest.Wait()
		require.Equal(t, []healthpb.HealthCheckResponse_ServingStatus{healthpb.HealthCheckResponse_NOT_SERVING}, drain(statuses), "the current status, at once")

		time.Sleep(5 * time.Second)
		synctest.Wait()
		require.Empty(t, drain(statuses), "a status that did not change was sent again")

		ready.MarkHealthy()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, []healthpb.HealthCheckResponse_ServingStatus{healthpb.HealthCheckResponse_SERVING}, drain(statuses))

		ready.MarkUnhealthy()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, []healthpb.HealthCheckResponse_ServingStatus{healthpb.HealthCheckResponse_NOT_SERVING}, drain(statuses))
	})
}

// countingMonitor counts its checks, and answers healthy.
type countingMonitor struct {
	checks atomic.Int64
}

func (m *countingMonitor) Healthy(context.Context) (bool, error) {
	m.checks.Add(1)
	return true, nil
}

func TestNewServer_Watch_shares_one_poller_per_name(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var m countingMonitor
		client := serve(t, telemetry.Noop(), health.Probes{Liveness: healthyMonitor, Readiness: &m})

		first, cancelFirst := watch(t, client, "")
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, first))
		second, cancelSecond := watch(t, client, "")
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, second), "a second watcher gets the current status at once")
		synctest.Wait()
		require.Equal(t, int64(1), m.checks.Load(), "the second watcher checked the monitor itself")

		time.Sleep(10*time.Second + 500*time.Millisecond)
		synctest.Wait()
		require.Equal(t, int64(11), m.checks.Load(), "two watchers of one name cause one check a second")

		cancelFirst()
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		require.Equal(t, int64(14), m.checks.Load(), "the poller stopped while it had a watcher")

		cancelSecond()
		synctest.Wait()
		stopped := m.checks.Load()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.Equal(t, stopped, m.checks.Load(), "the poller kept checking once both watchers had gone")

		// A new watcher starts a new poller, which checks at once.
		third, _ := watch(t, client, "")
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, third))
		require.Equal(t, stopped+1, m.checks.Load())
	})

	t.Run(`and "" and "readiness" are two names`, func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var m countingMonitor
			client := serve(t, telemetry.Noop(), health.Probes{Liveness: healthyMonitor, Readiness: &m})

			overall, _ := watch(t, client, "")
			require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, overall))
			ready, _ := watch(t, client, "readiness")
			require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, ready))

			time.Sleep(5*time.Second + 500*time.Millisecond)
			synctest.Wait()
			require.Equal(t, int64(12), m.checks.Load(), "one check a second per name")
		})
	})
}

func TestNewServer_Watch_a_check_that_runs_out_of_time_is_NOT_SERVING(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result func(ctx context.Context) (bool, error)
	}{
		{"a monitor that returns its context's error", func(ctx context.Context) (bool, error) { return false, ctx.Err() }},
		{"a monitor that answers healthy too late", func(context.Context) (bool, error) { return true, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var blocking atomic.Bool
				blocking.Store(true)
				m := health.MonitorFunc(func(ctx context.Context) (bool, error) {
					if !blocking.Load() {
						return true, nil
					}
					<-ctx.Done()
					return tc.result(ctx)
				})
				tel := humustest.NewTelemetry()
				client := serve(t, sdkOf(t, tel), health.Probes{Liveness: m, Readiness: healthyMonitor})

				first, _ := watch(t, client, "liveness")
				second, _ := watch(t, client, "liveness")
				begin := time.Now()
				require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, recv(t, first))
				require.Equal(t, time.Second, time.Since(begin), "the check was not done after a second")
				require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, recv(t, second))

				blocking.Store(false)
				require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, first))
				require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, second))

				rs := errorRecords(tel)
				require.Len(t, rs, 1, "the check that ran out of time is logged once")
				require.Contains(t, rs[0].Body().AsString(), context.DeadlineExceeded.Error())
			})
		})
	}
}

func TestNewServer_Watch_a_monitor_that_ignores_its_context_is_NOT_SERVING_after_a_second(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var checks atomic.Int64
		m := health.MonitorFunc(func(context.Context) (bool, error) {
			if checks.Add(1) == 1 {
				<-release // a blocking dial that ignores its context
			}
			return true, nil
		})
		tel := humustest.NewTelemetry()
		client := serve(t, sdkOf(t, tel), health.Probes{Liveness: m, Readiness: healthyMonitor})

		stream, _ := watch(t, client, "liveness")
		statuses, _ := received(stream)
		begin := time.Now()
		require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, <-statuses)
		require.Equal(t, time.Second, time.Since(begin), "the watcher waited longer than a second for a stuck check")

		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.Equal(t, int64(1), checks.Load(), "the monitor was checked again while its check was stuck")
		require.Empty(t, drain(statuses))
		rs := errorRecords(tel)
		require.Len(t, rs, 1, "the stuck check is logged once")
		require.Contains(t, rs[0].Body().AsString(), context.DeadlineExceeded.Error())

		// The stuck check's late answer is NOT_SERVING, and the next check
		// is a fresh one.
		close(release)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, <-statuses)
		require.Equal(t, int64(2), checks.Load())
	})
}

func TestNewServer_Watch_an_unknown_name_gets_one_SERVICE_UNKNOWN(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var m countingMonitor
		client := serve(t, telemetry.Noop(), health.Probes{Liveness: &m, Readiness: &m})

		stream, cancel := watch(t, client, "cart.v1.CartService")
		require.Equal(t, healthpb.HealthCheckResponse_SERVICE_UNKNOWN, recv(t, stream))
		statuses, errs := received(stream)

		time.Sleep(time.Minute)
		synctest.Wait()
		require.Empty(t, drain(statuses), "an unknown name got more than one status")
		select {
		case err := <-errs:
			t.Fatalf("the stream ended: %v", err)
		default:
		}
		require.Zero(t, m.checks.Load(), "an unknown name checked a monitor")

		cancel()
		require.Equal(t, codes.Canceled, status.Code(<-errs))
	})
}

func TestNewServer_Watch_ends_when_its_context_is_done(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		returned := make(chan struct{})
		var once sync.Once
		svcCtx, stop := context.WithCancel(t.Context())
		defer stop()

		// The handler's own return, seen through an interceptor of the
		// Service, which runs inside humus's.
		var handlerErr atomic.Value
		interceptor := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			err := handler(srv, ss)
			if err != nil {
				handlerErr.Store(err)
			}
			once.Do(func() { close(returned) })
			return err
		}

		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		s := &humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: ":9090",
			TLS:  tlsFor(t, ca),
		}, grpc.ChainStreamInterceptor(interceptor))
		require.NoError(t, err)
		healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, health.Probes{Liveness: healthyMonitor, Readiness: healthyMonitor}))
		served := make(chan error, 1)
		go func() { served <- svc.Serve(svcCtx) }()
		defer func() {
			require.NoError(t, svc.Shutdown(context.Background()))
			require.NoError(t, <-served)
		}()

		client := healthpb.NewHealthClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		stream, cancel := watch(t, client, "readiness")
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, stream))

		cancel()
		<-returned
		require.ErrorIs(t, handlerErr.Load().(error), context.Canceled)
	})
}

func TestNewServer_Watch_logs_a_failing_monitor_when_the_status_changes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var failing atomic.Bool
		failing.Store(true)
		m := health.MonitorFunc(func(context.Context) (bool, error) {
			if failing.Load() {
				return false, errSecret
			}
			return true, nil
		})
		tel := humustest.NewTelemetry()
		client := serve(t, sdkOf(t, tel), health.Probes{Liveness: healthyMonitor, Readiness: m})

		first, _ := watch(t, client, "")
		require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, recv(t, first))
		second, _ := watch(t, client, "")
		require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, recv(t, second))

		time.Sleep(10 * time.Second)
		synctest.Wait()
		rs := errorRecords(tel)
		require.Len(t, rs, 1, "a monitor that keeps failing is logged once, not on every poll")
		requireErrorRecord(t, rs[0], "readiness")

		failing.Store(false)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, first))
		failing.Store(true)
		require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, recv(t, first))
		time.Sleep(10 * time.Second)
		synctest.Wait()
		rs = errorRecords(tel)
		require.Len(t, rs, 2, "the second change to NOT_SERVING is logged, once")
		requireErrorRecord(t, rs[1], "readiness")
		require.Len(t, tel.Logs(), 2, "the server logged more than the errors")
	})

	t.Run("and nothing for an unhealthy monitor without an error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			client := serve(t, sdkOf(t, tel), health.Probes{Liveness: result(false, nil), Readiness: healthyMonitor})

			stream, _ := watch(t, client, "liveness")
			require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, recv(t, stream))
			time.Sleep(5 * time.Second)
			synctest.Wait()
			require.Empty(t, tel.Logs())
		})
	})
}

func TestNewServer_Watch_recovers_a_monitors_panic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var panicking atomic.Bool
		panicking.Store(true)
		m := health.MonitorFunc(func(context.Context) (bool, error) {
			if panicking.Load() {
				panic(errSecret)
			}
			return true, nil
		})
		tel := humustest.NewTelemetry()
		client := serve(t, sdkOf(t, tel), health.Probes{Liveness: m, Readiness: healthyMonitor})

		stream, _ := watch(t, client, "liveness")
		require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, recv(t, stream))
		panicking.Store(false)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, recv(t, stream))

		rs := errorRecords(tel)
		require.Len(t, rs, 1)
		require.Contains(t, rs[0].Body().AsString(), errSecret.Error())
	})
}

func TestNewServer_panics_on_a_nil_monitor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &humus.Setup{Telemetry: telemetry.Noop()}

		require.PanicsWithValue(t, "grpchealth: NewServer called with a nil Probes.Liveness", func() {
			grpchealth.NewServer(s, health.Probes{Readiness: healthyMonitor})
		})
		require.PanicsWithValue(t, "grpchealth: NewServer called with a nil Probes.Readiness", func() {
			grpchealth.NewServer(s, health.Probes{Liveness: healthyMonitor})
		})
		require.PanicsWithValue(t, "grpchealth: NewServer called with a nil Probes.Liveness", func() {
			grpchealth.NewServer(s, health.Probes{})
		})
	})
}
