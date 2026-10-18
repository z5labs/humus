// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

// attached is a run of an App whose Init mounts the handler at "/health/"
// of the mux its API is served on, next to a health Service.
type attached struct {
	host   *humustest.Host
	client *http.Client
	stop   func(t *testing.T)

	// failing makes the liveness monitor return errSecret.
	failing *atomic.Bool
}

// startAttached starts an App on a humustest Host, recording its telemetry
// in tel. Its Init serves "hello" on "/" of a mux, mounts the handler at
// "/health/" of the same mux, and serves the mux with httpserver on :8443,
// with a certificate from a humustest CA. It returns once the App's Services
// are serving.
func startAttached(t *testing.T, tel *humustest.Telemetry) attached {
	t.Helper()
	ca := humustest.NewCA(t)
	host := humustest.NewHost(t)
	failing := new(atomic.Bool)

	init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		hs := health.NewService(health.Liveness(health.MonitorFunc(func(context.Context) (bool, error) {
			if failing.Load() {
				return false, errSecret
			}
			return true, nil
		})))

		api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello")
		})
		mux := http.NewServeMux()
		mux.Handle("/", api)
		mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))

		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}
		svc, err := httpserver.NewService(s, settings, mux)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc, hs}, nil
	}

	p, status := humustest.Start(t, init,
		humustest.WithApp(humus.WithHost(host.Host()), humus.WithTelemetry(tel.Builder())),
	)
	<-host.Listening("localhost:8443")
	synctest.Wait() // until every Service is serving

	return attached{
		host:    host,
		client:  httpservertest.NewClient(host, ca.Pool()),
		failing: failing,
		stop: func(t *testing.T) {
			t.Helper()
			p.Signal(syscall.SIGTERM)
			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
			require.Empty(t, p.Stderr())
		},
	}
}

// fetch GETs url with c, and returns the status code and the body.
func fetch(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

// serverSpans returns the spans of tel that otelhttp recorded.
func serverSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range tel.Spans() {
		if s.InstrumentationScope().Name == otelhttp.ScopeName {
			spans = append(spans, s)
		}
	}
	return spans
}

// statusCode returns span's http.response.status_code, or 0.
func statusCode(span sdktrace.ReadOnlySpan) int64 {
	for _, kv := range span.Attributes() {
		if kv.Key == "http.response.status_code" {
			return kv.Value.AsInt64()
		}
	}
	return 0
}

func TestNewHandler_attached_to_an_httpserver_Service(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		app := startAttached(t, tel)

		code, body := fetch(t, app.client, "https://localhost:8443/health/liveness")
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "OK\n", body)
		code, body = fetch(t, app.client, "https://localhost:8443/health/readiness")
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "OK\n", body)

		// The end user's handler answers every other path, and the handler
		// every path under /health/.
		code, body = fetch(t, app.client, "https://localhost:8443/orders/42")
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "hello", body)
		code, _ = fetch(t, app.client, "https://localhost:8443/health/startup")
		require.Equal(t, http.StatusNotFound, code)

		app.failing.Store(true)
		code, body = fetch(t, app.client, "https://localhost:8443/health/liveness")
		require.Equal(t, http.StatusServiceUnavailable, code)
		require.Equal(t, "Service Unavailable\n", body)

		app.stop(t)

		rs := errorRecords(tel)
		require.Len(t, rs, 1)
		require.Equal(t, errSecret.Error(), rs[0].Body().AsString())

		var failed []sdktrace.ReadOnlySpan
		for _, s := range serverSpans(tel) {
			if statusCode(s) == http.StatusServiceUnavailable {
				failed = append(failed, s)
			}
		}
		require.Len(t, failed, 1, "want one span for the failed probe")
		sc := failed[0].SpanContext()
		require.True(t, rs[0].TraceID().IsValid(), "the record carries no trace ID")
		require.Equal(t, sc.TraceID(), rs[0].TraceID())
		require.Equal(t, sc.SpanID(), rs[0].SpanID())
	})
}

// countingPropagator is a TextMapPropagator that counts the calls to its
// Extract and Inject, and propagates nothing.
type countingPropagator struct {
	calls atomic.Int64
}

func (p *countingPropagator) Inject(context.Context, propagation.TextMapCarrier) {
	p.calls.Add(1)
}

func (p *countingPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	p.calls.Add(1)
	return ctx
}

func (p *countingPropagator) Fields() []string { return []string{"traceparent"} }

// countingExporter is an sdklog.Exporter that counts the records it is
// given.
type countingExporter struct {
	records atomic.Int64
}

func (e *countingExporter) Export(_ context.Context, rs []sdklog.Record) error {
	e.records.Add(int64(len(rs)))
	return nil
}

func (e *countingExporter) Shutdown(context.Context) error { return nil }

func (e *countingExporter) ForceFlush(context.Context) error { return nil }

// This test sets OpenTelemetry's globals, so it doesn't run in parallel,
// and it restores them when it ends.
func TestNewHandler_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}
	logs := &countingExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the handler never uses them, as the proposal for exposing health probes, #761, requires.
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
		tel := humustest.NewTelemetry()
		app := startAttached(t, tel)
		app.failing.Store(true)
		code, _ := fetch(t, app.client, "https://localhost:8443/health/liveness")
		require.Equal(t, http.StatusServiceUnavailable, code)
		code, _ = fetch(t, app.client, "https://localhost:8443/health/readiness")
		require.Equal(t, http.StatusOK, code)
		app.stop(t)

		require.Len(t, errorRecords(tel), 1, "the Setup's LoggerProvider recorded no error")
		require.NotEmpty(t, serverSpans(tel), "the Setup's TracerProvider recorded no span")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
	require.Zero(t, logs.records.Load(), "the global LoggerProvider recorded records")
}
