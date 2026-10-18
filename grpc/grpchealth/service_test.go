// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpchealth_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts.
// The probes Service listens on localhost:8081, in plain text, and is asked
// with clients from grpcservertest.NewClient given insecure credentials, as
// the kubelet asks. Whole Apps run with grpcservertest.Start, so they shut
// down as a grpcserver.Main's App does, after its 5s drain.

// probesAddr is the address the tests' probes Service is reached on.
const probesAddr = "localhost:8081"

// probeSettings are the Settings of the tests' probes Service.
var probeSettings = grpchealth.Settings{Addr: ":8081"}

// servesProbes returns an Init whose only Service is a probes Service on
// settings, with opts.
func servesProbes(settings grpchealth.Settings, opts ...health.Option) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpchealth.NewService(s, settings, opts...)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
}

// plaintext returns a client of the health server on address in h that
// speaks plain text, as the kubelet's grpc probes do.
func plaintext(t *testing.T, h *humustest.Host, address string) healthpb.HealthClient {
	t.Helper()
	return healthpb.NewHealthClient(grpcservertest.NewClient(t, h, address, nil,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	))
}

// check asks client for name's status, and fails t if the RPC fails.
func check(t *testing.T, client healthpb.HealthClient, name string) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: name})
	require.NoError(t, err, "service %q", name)
	return resp.GetStatus()
}

// hostSetup returns a Setup on h with telemetry off, for a test that drives
// a Service directly rather than through an App.
func hostSetup(h *humustest.Host) *humus.Setup {
	return &humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}
}

// requireRefused fails t unless a Dial to address is refused.
func requireRefused(t *testing.T, h *humustest.Host, address string) {
	t.Helper()
	conn, err := h.Dial(t.Context(), "tcp", address)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("Dial(%q) connected, want ECONNREFUSED", address)
	}
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
}

// records returns the records of tel emitted under the package's scope.
func records(tel *humustest.Telemetry) []sdklog.Record {
	var rs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == scope {
			rs = append(rs, r)
		}
	}
	return rs
}

const (
	serving    = healthpb.HealthCheckResponse_SERVING
	notServing = healthpb.HealthCheckResponse_NOT_SERVING
)

func TestService(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		svc, err := grpchealth.NewService(s, probeSettings)
		require.NoError(t, err)
		return svc
	})
}

func TestService_answers_the_kubelets_probe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		p, exit := grpcservertest.Start(t, servesProbes(probeSettings),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening(probesAddr)
		synctest.Wait()

		client := plaintext(t, h, probesAddr)
		for _, name := range []string{"liveness", "readiness", ""} {
			require.Equal(t, serving, check(t, client, name), "service %q", name)
		}
		_, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "cart.v1.CartService"})
		require.Equal(t, codes.NotFound, status.Code(err), err)

		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())
	})
}

func TestProbesInit_answers_the_probes_on_a_plaintext_port(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"tls.pem": pemFile(t, ca.Issue(t, "carts")),
		}))
		p, exit := grpcservertest.Start(t, ProbesInit,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{
				"GRPC_ADDR":         ":9090",
				"GRPC_TLS_PEM_FILE": "tls.pem",
				"PROBES_ADDR":       ":8081",
			}),
		)
		<-h.Listening("carts:9090")
		<-h.Listening(probesAddr)
		synctest.Wait()

		client := plaintext(t, h, probesAddr)
		require.Equal(t, serving, check(t, client, "readiness"))
		require.Equal(t, serving, check(t, client, "liveness"))
		stream, _ := watch(t, client, "readiness")
		require.Equal(t, serving, recv(t, stream))

		// The end user's service is served over TLS on its own port, and is
		// not reachable on the probes port.
		_, err := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool())).
			UnaryCall(t.Context(), &testpb.SimpleRequest{})
		require.NoError(t, err)
		_, err = testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, probesAddr, nil,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)).UnaryCall(t.Context(), &testpb.SimpleRequest{})
		require.Equal(t, codes.Unimplemented, status.Code(err), "the probes port serves the end user's service: %v", err)

		<-p.Notified(syscall.SIGTERM)
		begin := time.Now()
		p.Signal(syscall.SIGTERM)
		time.Sleep(time.Second) // inside grpcserver's 5s drain
		require.Equal(t, notServing, check(t, client, "readiness"), "ready a second after SIGTERM")
		require.Equal(t, notServing, check(t, client, ""), "ready a second after SIGTERM")
		require.Equal(t, serving, check(t, client, "liveness"), "not live during the drain")

		require.Equal(t, notServing, recv(t, stream))
		_, err = stream.Recv()
		require.Equal(t, codes.Unavailable, status.Code(err), err)
		require.Equal(t, 5*time.Second, time.Since(begin), "the Watch ended before Shutdown") // the drain, then Shutdown

		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())
	})
}

func TestService_readiness(t *testing.T) {
	t.Run("with no Options, SERVING while it serves and NOT_SERVING from the moment Serve's context is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			p, exit := grpcservertest.Start(t, servesProbes(probeSettings),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			<-h.Listening(probesAddr)
			synctest.Wait()

			client := plaintext(t, h, probesAddr)
			require.Equal(t, serving, check(t, client, "readiness"))
			require.Equal(t, serving, check(t, client, "liveness"))

			<-p.Notified(syscall.SIGTERM)
			sigterm := time.Now()
			p.Signal(syscall.SIGTERM)
			synctest.Wait()
			require.Equal(t, notServing, check(t, client, "readiness"))
			require.Zero(t, time.Since(sigterm), "readiness turned NOT_SERVING the moment shutdown started")
			require.Equal(t, serving, check(t, client, "liveness"))

			time.Sleep(4 * time.Second) // still inside grpcserver's 5s drain
			require.Equal(t, notServing, check(t, client, "readiness"))
			require.Equal(t, serving, check(t, client, "liveness"))

			st := <-exit
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
			requireRefused(t, h, probesAddr)
		})
	})

	t.Run("an unhealthy monitor added with health.Readiness answers readiness NOT_SERVING", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			p, exit := grpcservertest.Start(t, servesProbes(probeSettings, health.Readiness(result(false, nil))),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			<-h.Listening(probesAddr)
			synctest.Wait()

			client := plaintext(t, h, probesAddr)
			require.Equal(t, notServing, check(t, client, "readiness"))
			require.Equal(t, notServing, check(t, client, ""))
			require.Equal(t, serving, check(t, client, "liveness"))

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-exit).Code())
		})
	})

	t.Run("an unhealthy monitor added with health.Liveness answers liveness NOT_SERVING", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			p, exit := grpcservertest.Start(t, servesProbes(probeSettings, health.Liveness(result(false, nil))),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			<-h.Listening(probesAddr)
			synctest.Wait()

			client := plaintext(t, h, probesAddr)
			require.Equal(t, notServing, check(t, client, "liveness"))
			require.Equal(t, serving, check(t, client, "readiness"))

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-exit).Code())
		})
	})
}

func TestService_serves_plain_text_only(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, exit := grpcservertest.Start(t, servesProbes(probeSettings),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening(probesAddr)
		synctest.Wait()

		conn := grpcservertest.NewClient(t, h, probesAddr, ca.Pool())
		_, err := healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{Service: "liveness"})
		require.Equal(t, codes.Unavailable, status.Code(err), "a client over TLS connected: %v", err)

		// The client goes on reconnecting until it is closed. The Host's
		// connections are unbuffered net.Pipes, on which a TLS client and a
		// plaintext server both write first, so a reconnect in flight would
		// hold GracefulStop past the grace period; over TCP, the kernel's
		// buffers let each side's first write through.
		require.NoError(t, conn.Close())
		synctest.Wait()

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v; stderr: %s", st.Cause(), p.Stderr())
	})
}

// panicking is a monitor that panics.
var panicking = health.MonitorFunc(func(context.Context) (bool, error) {
	panic("the connection pool is nil")
})

func TestService_recovers_a_panic_of_a_monitor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, exit := grpcservertest.Start(t, servesProbes(probeSettings, health.Liveness(panicking)),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening(probesAddr)
		synctest.Wait()

		client := plaintext(t, h, probesAddr)
		_, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "liveness"})
		require.Equal(t, codes.Internal, status.Code(err), err)
		require.Equal(t, serving, check(t, client, "readiness"), "the server goes on answering")

		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())

		var errs []sdklog.Record
		for _, r := range tel.Logs() {
			if r.Severity() == log.SeverityError {
				errs = append(errs, r)
			}
		}
		require.Len(t, errs, 1)
		require.Equal(t, scope, errs[0].InstrumentationScope().Name)
		method, ok := attr(errs[0], "rpc.method")
		require.True(t, ok, "the record has no rpc.method")
		require.Equal(t, "grpc.health.v1.Health/Check", method.AsString())
	})
}

func TestNewServer_on_a_grpcserver_Service_logs_a_panic_under_grpcservers_scope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		client := serve(t, sdkOf(t, tel), probesWith("liveness", panicking))

		_, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "liveness"})
		require.Equal(t, codes.Internal, status.Code(err), err)

		var errs []sdklog.Record
		for _, r := range tel.Logs() {
			if r.Severity() == log.SeverityError {
				errs = append(errs, r)
			}
		}
		require.Len(t, errs, 1)
		require.Equal(t, "github.com/z5labs/humus/grpc/grpcserver", errs[0].InstrumentationScope().Name)
	})
}

func TestService_logs_listening(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, exit := grpcservertest.Start(t, servesProbes(probeSettings),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening(probesAddr)
		synctest.Wait()

		rs := records(tel)
		require.Len(t, rs, 1)
		require.Equal(t, "listening", rs[0].Body().AsString())
		require.Equal(t, log.SeverityInfo, rs[0].Severity())
		require.Equal(t, "INFO", rs[0].SeverityText())
		addr, ok := attr(rs[0], "server.address")
		require.True(t, ok, "the record has no server.address")
		require.Equal(t, ":8081", addr.AsString())

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-exit).Code())
	})
}

func TestNewService_settings(t *testing.T) {
	t.Run("the zero Settings fails, naming Addr", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			svc, err := grpchealth.NewService(hostSetup(h), grpchealth.Settings{})
			require.Nil(t, svc)
			require.ErrorContains(t, err, "grpchealth: NewService")
			require.ErrorContains(t, err, "Settings.Addr")
		})
	})

	t.Run("a nil Option is rejected, with its index", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			svc, err := grpchealth.NewService(hostSetup(h), probeSettings, health.Readiness(healthyMonitor), nil)
			require.Nil(t, svc)
			require.ErrorContains(t, err, "opts[1] must not be nil")
			require.NotContains(t, err.Error(), "opts[0]")
			requireRefused(t, h, probesAddr)
		})
	})

	t.Run("every problem is in one error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			_, err := grpchealth.NewService(hostSetup(h), grpchealth.Settings{Addr: ":grpc"}, nil, health.Liveness(healthyMonitor), nil)
			require.ErrorContains(t, err, "Settings.Addr")
			require.ErrorContains(t, err, "opts[0] must not be nil")
			require.ErrorContains(t, err, "opts[2] must not be nil")
			require.NotContains(t, err.Error(), "opts[1]")
		})
	})
}

func TestNewService_the_listener(t *testing.T) {
	t.Run("an Init that fails after NewService exits 1 and closes the listener", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			errInit := errors.New("the database is misconfigured")
			init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				if _, err := grpchealth.NewService(s, probeSettings); err != nil {
					return nil, err
				}
				return nil, errInit
			}
			_, exit := grpcservertest.Start(t, init,
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)

			st := <-exit
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorIs(t, st.Cause(), errInit)
			requireRefused(t, h, probesAddr)
		})
	})

	t.Run("a taken address fails NewService, with the Host's error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			l, err := h.Listen("tcp", ":8081")
			require.NoError(t, err)
			defer func() { _ = l.Close() }()

			svc, err := grpchealth.NewService(hostSetup(h), probeSettings)
			require.Nil(t, svc)
			require.ErrorIs(t, err, syscall.EADDRINUSE)
			require.ErrorContains(t, err, "grpchealth: NewService")
		})
	})
}

func TestService_Addr_reports_the_port_the_Host_picked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		addrs := make(chan net.Addr, 1)
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := grpchealth.NewService(s, grpchealth.Settings{Addr: ":0"})
			if err != nil {
				return nil, err
			}
			addrs <- svc.Addr()
			return []humus.Service{svc}, nil
		}
		p, exit := grpcservertest.Start(t, init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)

		_, port, err := net.SplitHostPort((<-addrs).String())
		require.NoError(t, err)
		require.NotEqual(t, "0", port)
		synctest.Wait()

		require.Equal(t, serving, check(t, plaintext(t, h, fmt.Sprintf("localhost:%s", port)), "readiness"))

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-exit).Code())
	})
}

func TestService_Shutdown(t *testing.T) {
	t.Run("before Serve, Serve then returns nil at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc, err := grpchealth.NewService(hostSetup(h), probeSettings)
			require.NoError(t, err)

			require.NoError(t, svc.Shutdown(t.Context()))
			start := time.Now()
			require.NoError(t, svc.Serve(t.Context()))
			require.Zero(t, time.Since(start))
			requireRefused(t, h, probesAddr)
		})
	})

	t.Run("with a done context, stops the server and wraps the context's error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			started := make(chan struct{})
			cancelled := make(chan error, 1)
			slow := health.MonitorFunc(func(ctx context.Context) (bool, error) {
				close(started)
				<-ctx.Done()
				cancelled <- ctx.Err()
				return false, ctx.Err()
			})
			svc, err := grpchealth.NewService(hostSetup(h), probeSettings, health.Liveness(slow))
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()

			client := plaintext(t, h, probesAddr)
			got := make(chan error, 1)
			go func() {
				_, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "liveness"})
				got <- err
			}()
			<-started
			done, cancel := context.WithCancel(t.Context())
			cancel()
			start := time.Now()
			err = svc.Shutdown(done)
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "grpchealth: shutting down")
			require.Zero(t, time.Since(start))

			require.ErrorIs(t, <-cancelled, context.Canceled)
			require.Error(t, <-got)
			require.NoError(t, <-served)
			requireRefused(t, h, probesAddr)
		})
	})

	t.Run("a second call returns nil", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc, err := grpchealth.NewService(hostSetup(h), probeSettings)
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()
			synctest.Wait()

			require.NoError(t, svc.Shutdown(t.Context()))
			require.NoError(t, svc.Shutdown(t.Context()))
			require.NoError(t, <-served)
		})
	})
}

func TestService_records_no_span_and_no_metric(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, exit := grpcservertest.Start(t, servesProbes(probeSettings, health.Liveness(result(false, errSecret))),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening(probesAddr)
		synctest.Wait()

		client := plaintext(t, h, probesAddr)
		require.Equal(t, serving, check(t, client, "readiness"))
		require.Equal(t, notServing, check(t, client, "liveness"))
		_, err := client.List(t.Context(), &healthpb.HealthListRequest{})
		require.NoError(t, err)
		stream, _ := watch(t, client, "readiness")
		require.Equal(t, serving, recv(t, stream))

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-exit).Code())

		require.Empty(t, tel.Spans(), "a probes Service records no span")
		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		require.Empty(t, rm.ScopeMetrics, "a probes Service records no metric")
		require.Len(t, errorRecords(tel), 2, "the monitor's errors, of Check and List, are still logged")
	})
}

// This test sets OpenTelemetry's globals, so it doesn't run in parallel,
// and it restores them when it ends.
func TestService_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}
	logs := &countingExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the probes Service never uses them, as the proposal for exposing health probes, #761, requires.
	{
		prevTP, prevMP, prevProp, prevLP := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator(), otel.GetLoggerProvider()
		t.Cleanup(func() {
			otel.SetTracerProvider(prevTP)
			otel.SetMeterProvider(prevMP)
			otel.SetTextMapPropagator(prevProp)
			otel.SetLoggerProvider(prevLP)
		})
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
		otel.SetLoggerProvider(lp)
	}

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, exit := grpcservertest.Start(t, servesProbes(probeSettings, health.Liveness(result(false, errSecret)), health.Readiness(panicking)),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening(probesAddr)
		synctest.Wait()

		client := plaintext(t, h, probesAddr)
		require.Equal(t, notServing, check(t, client, "liveness"))
		_, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "readiness"})
		require.Equal(t, codes.Internal, status.Code(err), err)

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-exit).Code())
		require.Len(t, errorRecords(tel), 2, "the Setup's LoggerProvider recorded the monitor's error and the panic")
		require.NotEmpty(t, records(tel), "the Setup's LoggerProvider recorded listening")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
	require.Zero(t, logs.records.Load(), "the global LoggerProvider recorded records")
}
