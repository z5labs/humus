// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpchealth_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// pemFile returns c's certificate chain and private key, PEM-encoded in one
// file, as Init's serverTLS reads them.
func pemFile(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var b []byte
	for _, der := range cert.Certificate {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

func TestInit_answers_the_probes_on_the_Service_port(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"tls.pem": pemFile(t, ca.Issue(t, "carts")),
		}))
		p, exit := grpcservertest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{"GRPC_ADDR": ":9090", "GRPC_TLS_PEM_FILE": "tls.pem"}),
		)
		<-h.Listening("carts:9090")
		synctest.Wait()

		conn := grpcservertest.NewClient(t, h, "carts:9090", ca.Pool())
		client := healthpb.NewHealthClient(conn)
		for _, name := range []string{"", "readiness", "liveness"} {
			resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: name})
			require.NoError(t, err)
			require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus(), "service %q", name)
		}
		_, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "grpc.testing.TestService"})
		require.Equal(t, codes.NotFound, status.Code(err), "a service registered next to the health server has no status of its own: %v", err)

		// The end user's own service is served on the same port.
		resp, err := testpb.NewTestServiceClient(conn).UnaryCall(t.Context(), &testpb.SimpleRequest{})
		require.NoError(t, err)
		require.Equal(t, "greeter", resp.GetUsername())

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		synctest.Wait() // the drain has begun
		ready, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "readiness"})
		require.NoError(t, err)
		require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, ready.GetStatus(), "ready during the drain")
		live, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "liveness"})
		require.NoError(t, err)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, live.GetStatus(), "not live during the drain")

		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

// countingPropagator is a propagator that counts its calls.
type countingPropagator struct {
	calls atomic.Int64
}

func (p *countingPropagator) Inject(context.Context, propagation.TextMapCarrier) { p.calls.Add(1) }

func (p *countingPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	p.calls.Add(1)
	return ctx
}

func (p *countingPropagator) Fields() []string { return nil }

// countingExporter is a log exporter that counts the records it is given.
type countingExporter struct {
	records atomic.Int64
}

func (e *countingExporter) Export(_ context.Context, rs []sdklog.Record) error {
	e.records.Add(int64(len(rs)))
	return nil
}

func (e *countingExporter) Shutdown(context.Context) error   { return nil }
func (e *countingExporter) ForceFlush(context.Context) error { return nil }

// This test sets OpenTelemetry's globals, so it doesn't run in parallel,
// and it restores them when it ends.
func TestNewServer_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}
	logs := &countingExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the health server never uses them, as the proposal for exposing health probes, #761, requires.
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
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		failing := health.MonitorFunc(func(context.Context) (bool, error) { return false, errSecret })
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := grpcserver.NewService(s, grpcserver.Settings{Addr: ":9090", TLS: tlsFor(t, ca)})
			if err != nil {
				return nil, err
			}
			hs := health.NewService(health.Liveness(failing))
			healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, hs.Probes()))
			return []humus.Service{svc, hs}, nil
		}
		p, exit := grpcservertest.Start(t, init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening("carts:9090")
		synctest.Wait()

		client := healthpb.NewHealthClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		resp, err := client.Check(t.Context(), &healthpb.HealthCheckRequest{Service: "liveness"})
		require.NoError(t, err)
		require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, resp.GetStatus())
		_, err = client.List(t.Context(), &healthpb.HealthListRequest{})
		require.NoError(t, err)
		stream, _ := watch(t, client, "liveness")
		require.Equal(t, healthpb.HealthCheckResponse_NOT_SERVING, recv(t, stream))

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Len(t, errorRecords(tel), 3, "the Setup's LoggerProvider recorded the errors of Check, List and Watch")
		require.NotEmpty(t, serverSpans(tel), "the Setup's TracerProvider traced the RPCs")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
	require.Zero(t, logs.records.Load(), "the global LoggerProvider recorded records")
}
