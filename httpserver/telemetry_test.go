// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver_test

import (
	"context"
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
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

// The trace context a caller sends, from the W3C Trace Context
// specification's example of a traceparent header.
const (
	callerTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpanID  = "00f067aa0ba902b7"
	traceparent   = "00-" + callerTraceID + "-" + callerSpanID + "-01"
)

// helloName answers a request to GET /hello/{name} with "hello, <name>".
func helloName() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello/{name}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello, "+r.PathValue("name"))
	})
	return mux
}

// do sends req with c in a new goroutine, and returns the channel its
// response arrives on.
func do(c *http.Client, req *http.Request) <-chan response {
	got := make(chan response, 1)
	go func() {
		resp, err := c.Do(req)
		if err != nil {
			got <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- response{proto: resp.ProtoMajor, code: resp.StatusCode, body: string(b), err: err}
	}()
	return got
}

// serveOne runs an App whose only Service serves h, recording its
// telemetry in tel, sends it req, then stops the App. It returns the
// response once the App has exited, so every span the request started has
// ended.
func serveOne(t *testing.T, tel *humustest.Telemetry, h http.Handler, req func(t *testing.T) *http.Request) response {
	t.Helper()
	ca := humustest.NewCA(t)
	host := humustest.NewHost(t)
	p, status := humustest.Start(t, serves(settingsFor(t, ca), h),
		humustest.WithApp(humus.WithHost(host.Host()), humus.WithTelemetry(tel.Builder())),
	)
	<-host.Listening("localhost:8443")

	r := <-do(client(host, ca, offersBoth), req(t))
	require.NoError(t, r.err)

	p.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.Empty(t, p.Stderr())
	return r
}

// newRequest returns a function that builds a GET of url, with header
// traceparent set to tp unless tp is empty.
func newRequest(url, tp string) func(t *testing.T) *http.Request {
	return func(t *testing.T) *http.Request {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		require.NoError(t, err)
		if tp != "" {
			req.Header.Set("traceparent", tp)
		}
		return req
	}
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

func TestService_traces_each_request(t *testing.T) {
	t.Run("a mux route, named after the method and the pattern", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			r := serveOne(t, tel, helloName(), newRequest("https://localhost:8443/hello/gopher", ""))
			require.Equal(t, http.StatusOK, r.code)
			require.Equal(t, "hello, gopher", r.body)

			spans := serverSpans(tel)
			require.Len(t, spans, 1)
			require.Equal(t, "GET /hello/{name}", spans[0].Name())
			require.Equal(t, trace.SpanKindServer, spans[0].SpanKind())
		})
	})

	t.Run("a handler with no pattern, named after the method", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			r := serveOne(t, tel, hello, newRequest("https://localhost:8443/anything", ""))
			require.Equal(t, "hello", r.body)

			spans := serverSpans(tel)
			require.Len(t, spans, 1)
			require.Equal(t, "GET", spans[0].Name())
			require.Equal(t, trace.SpanKindServer, spans[0].SpanKind())
		})
	})

	t.Run("a request with a traceparent joins its caller's trace", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			r := serveOne(t, tel, helloName(), newRequest("https://localhost:8443/hello/gopher", traceparent))
			require.Equal(t, http.StatusOK, r.code)

			spans := serverSpans(tel)
			require.Len(t, spans, 1)
			require.Equal(t, callerTraceID, spans[0].SpanContext().TraceID().String())
			parent := spans[0].Parent()
			require.True(t, parent.IsRemote(), "the span's parent is not remote")
			require.Equal(t, callerTraceID, parent.TraceID().String())
			require.Equal(t, callerSpanID, parent.SpanID().String())
		})
	})
}

func TestService_measures_each_request(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		r := serveOne(t, tel, helloName(), newRequest("https://localhost:8443/hello/gopher", ""))
		require.Equal(t, http.StatusOK, r.code)

		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		var found *metricdata.Metrics
		for _, sm := range rm.ScopeMetrics {
			if sm.Scope.Name != otelhttp.ScopeName {
				continue
			}
			for i := range sm.Metrics {
				if sm.Metrics[i].Name == "http.server.request.duration" {
					found = &sm.Metrics[i]
				}
			}
		}
		require.NotNil(t, found, "no http.server.request.duration in %+v", rm.ScopeMetrics)
		hist, ok := found.Data.(metricdata.Histogram[float64])
		require.True(t, ok, "http.server.request.duration is a %T, want a float64 histogram", found.Data)
		require.Len(t, hist.DataPoints, 1)
		require.Equal(t, uint64(1), hist.DataPoints[0].Count)
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

// This test sets OpenTelemetry's globals, so it doesn't run in parallel,
// and it restores them when it ends.
func TestService_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the Service never uses them, as the HTTP server proposal, #680, requires.
	{
		prevTP, prevMP, prevProp := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
		t.Cleanup(func() {
			otel.SetTracerProvider(prevTP)
			otel.SetMeterProvider(prevMP)
			otel.SetTextMapPropagator(prevProp)
		})
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
	}

	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		r := serveOne(t, tel, helloName(), newRequest("https://localhost:8443/hello/gopher", traceparent))
		require.Equal(t, http.StatusOK, r.code)
		require.Len(t, serverSpans(tel), 1, "the Setup's TracerProvider recorded no span")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
}
