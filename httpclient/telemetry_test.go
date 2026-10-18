// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/humustest"
)

// otelhttpSpans returns the spans of tel that otelhttp recorded: on the
// caller's side, the client's attempts, and on the dependency's, the
// server's requests.
func otelhttpSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range tel.Spans() {
		if s.InstrumentationScope().Name == otelhttp.ScopeName {
			spans = append(spans, s)
		}
	}
	return spans
}

// jobSpan returns the span of the job's run in tel.
func jobSpan(t *testing.T, tel *humustest.Telemetry) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range tel.Spans() {
		if s.Name() == "job" && s.InstrumentationScope().Name != otelhttp.ScopeName {
			return s
		}
	}
	require.FailNow(t, "no span of the job's run")
	return nil
}

// attrs returns s's attributes by key.
func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value)
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value
	}
	return m
}

// callOnce runs the end user's App, recording its telemetry in tel, with a
// job whose run sends one GET of rawURL. It returns the call's error once
// the App has exited, so every attempt's span has ended.
func callOnce(t *testing.T, h *humustest.Host, tel *humustest.Telemetry, ca *humustest.CA, rawURL string) error {
	t.Helper()
	var callErr error
	st := runJobWith(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
		_, callErr = get(ctx, c, rawURL)
		return nil
	})
	requireExitZero(t, st)
	return callErr
}

func TestClient_traces_each_attempt(t *testing.T) {
	t.Run("a GET is one CLIENT span named GET, a child of the job's span", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, &ok{})
			defer stop()

			// User info in a request's URL never reaches the span.
			password := rand.Text()
			tel := humustest.NewTelemetry()
			require.NoError(t, callOnce(t, h, tel, ca, "https://gopher:"+password+"@payments:8443/charges/1?expand=card"))

			spans := otelhttpSpans(tel)
			require.Len(t, spans, 1)
			span := spans[0]
			require.Equal(t, "GET", span.Name())
			require.Equal(t, trace.SpanKindClient, span.SpanKind())
			require.Equal(t, codes.Unset, span.Status().Code)

			parent := jobSpan(t, tel)
			require.Equal(t, parent.SpanContext().TraceID(), span.SpanContext().TraceID())
			require.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID())

			a := attrs(span)
			require.Equal(t, "https://payments:8443/charges/1?expand=card", a["url.full"].AsString())
			require.NotContains(t, a["url.full"].AsString(), password)
			require.Equal(t, "GET", a["http.request.method"].AsString())
			require.Equal(t, "payments", a["server.address"].AsString())
			require.EqualValues(t, 8443, a["server.port"].AsInt64())
			require.EqualValues(t, http.StatusOK, a["http.response.status_code"].AsInt64())
			require.Equal(t, "2.0", a["network.protocol.version"].AsString())
			require.NotContains(t, a, attribute.Key("http.request.resend_count"), "the first attempt is not a resend")
		})
	})

	t.Run("a 404 is an Error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, http.NotFoundHandler())
			defer stop()

			tel := humustest.NewTelemetry()
			require.NoError(t, callOnce(t, h, tel, ca, "https://payments:8443/charges/404"))

			spans := otelhttpSpans(tel)
			require.Len(t, spans, 1)
			require.Equal(t, codes.Error, spans[0].Status().Code)
			a := attrs(spans[0])
			require.EqualValues(t, http.StatusNotFound, a["http.response.status_code"].AsInt64())
			require.Equal(t, "404", a["error.type"].AsString())
		})
	})

	t.Run("a refused connection is an Error, on each of its attempts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t) // nothing listens at payments:8443

			tel := humustest.NewTelemetry()
			callErr := callOnce(t, h, tel, humustest.NewCA(t), "https://payments:8443/charges/1")
			require.Error(t, callErr)

			spans := otelhttpSpans(tel)
			require.Len(t, spans, 3, "a refused GET is retried, up to MaxAttempts")
			var counts []int64
			for _, s := range spans {
				require.Equal(t, trace.SpanKindClient, s.SpanKind())
				require.Equal(t, codes.Error, s.Status().Code)
				require.NotEmpty(t, attrs(s)["error.type"].AsString())
				counts = append(counts, attrs(s)["http.request.resend_count"].AsInt64())
			}
			require.ElementsMatch(t, []int64{0, 1, 2}, counts)
		})
	})

	t.Run("a redirect within the origin is a second span, with a resend count of 1", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /old", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/new", http.StatusFound)
			})
			mux.HandleFunc("GET /new", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "new")
			})
			stop := startPayments(t, h, h.Host(), ca, mux)
			defer stop()

			tel := humustest.NewTelemetry()
			require.NoError(t, callOnce(t, h, tel, ca, "https://payments:8443/old"))

			spans := otelhttpSpans(tel)
			require.Len(t, spans, 2)
			byURL := make(map[string]sdktrace.ReadOnlySpan)
			for _, s := range spans {
				byURL[attrs(s)["url.full"].AsString()] = s
			}
			first, second := byURL["https://payments:8443/old"], byURL["https://payments:8443/new"]
			require.NotNil(t, first)
			require.NotNil(t, second)

			require.EqualValues(t, http.StatusFound, attrs(first)["http.response.status_code"].AsInt64())
			require.NotContains(t, attrs(first), attribute.Key("http.request.resend_count"))
			require.EqualValues(t, 1, attrs(second)["http.request.resend_count"].AsInt64())
			job := jobSpan(t, tel).SpanContext()
			for _, s := range []sdktrace.ReadOnlySpan{first, second} {
				require.Equal(t, trace.SpanKindClient, s.SpanKind())
				require.Equal(t, job.TraceID(), s.SpanContext().TraceID())
				require.Equal(t, job.SpanID(), s.Parent().SpanID(), "each attempt is a child of the job's span")
			}
		})
	})

	t.Run("each redirect counts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/loop", http.StatusFound)
			}))
			defer stop()

			tel := humustest.NewTelemetry()
			require.ErrorContains(t, callOnce(t, h, tel, ca, "https://payments:8443/loop"), "stopped after 10 redirects")

			var counts []int64
			for _, s := range otelhttpSpans(tel) {
				counts = append(counts, attrs(s)["http.request.resend_count"].AsInt64())
			}
			require.ElementsMatch(t, []int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, counts, "the call and its first 9 redirects, until the 10th is refused")
		})
	})
}

func TestClient_propagates_trace_context_and_baggage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		paymentsTel := humustest.NewTelemetry()
		var tenant string
		stop := startPaymentsWith(t, h, h.Host(), paymentsTel.Builder(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant = baggage.FromContext(r.Context()).Member("tenant").Value()
			_, _ = io.WriteString(w, "ok")
		}))

		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			m, err := baggage.NewMember("tenant", "acme")
			if err != nil {
				return err
			}
			b, err := baggage.New(m)
			if err != nil {
				return err
			}
			_, err = get(baggage.ContextWithBaggage(ctx, b), c, "https://payments:8443/charges/1")
			return err
		})
		requireExitZero(t, st)
		stop() // the dependency's span has ended once it exits

		client := otelhttpSpans(tel)
		require.Len(t, client, 1)
		server := otelhttpSpans(paymentsTel)
		require.Len(t, server, 1)
		require.Equal(t, trace.SpanKindServer, server[0].SpanKind())
		require.Equal(t, client[0].SpanContext().TraceID(), server[0].SpanContext().TraceID())
		require.True(t, server[0].Parent().IsRemote())
		require.Equal(t, client[0].SpanContext().SpanID(), server[0].Parent().SpanID())
		require.Equal(t, "acme", tenant)
	})
}

// histogram returns the float64 histogram named name in otelhttp's scope
// of rm, failing t if there is none.
func histogram[N int64 | float64](t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Histogram[N] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != otelhttp.ScopeName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[N])
			require.True(t, ok, "%s is a %T", name, m.Data)
			return hist
		}
	}
	require.FailNow(t, "no "+name+" in otelhttp's scope", "%+v", rm.ScopeMetrics)
	return metricdata.Histogram[N]{}
}

func TestClient_measures_each_attempt(t *testing.T) {
	t.Run("a call has a data point with its status code", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, &ok{})
			defer stop()

			tel := humustest.NewTelemetry()
			require.NoError(t, callOnce(t, h, tel, ca, "https://payments:8443/charges/1"))

			rm, err := tel.Metrics(t.Context())
			require.NoError(t, err)
			duration := histogram[float64](t, rm, "http.client.request.duration")
			require.Len(t, duration.DataPoints, 1)
			dp := duration.DataPoints[0]
			require.EqualValues(t, 1, dp.Count)
			code, found := dp.Attributes.Value("http.response.status_code")
			require.True(t, found)
			require.EqualValues(t, http.StatusOK, code.AsInt64())
			_, found = dp.Attributes.Value("error.type")
			require.False(t, found)

			size := histogram[int64](t, rm, "http.client.request.body.size")
			require.Len(t, size.DataPoints, 1)
			require.EqualValues(t, 1, size.DataPoints[0].Count)

			for _, sm := range rm.ScopeMetrics {
				require.NotContains(t, sm.Scope.Name, "humus", "humus records no instrument of its own")
			}
		})
	})

	t.Run("a refused connection's data point has error.type", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t) // nothing listens at payments:8443

			tel := humustest.NewTelemetry()
			require.Error(t, callOnce(t, h, tel, humustest.NewCA(t), "https://payments:8443/charges/1"))

			rm, err := tel.Metrics(t.Context())
			require.NoError(t, err)
			duration := histogram[float64](t, rm, "http.client.request.duration")
			require.Len(t, duration.DataPoints, 1)
			require.EqualValues(t, 3, duration.DataPoints[0].Count, "each attempt is measured")
			errType, found := duration.DataPoints[0].Attributes.Value("error.type")
			require.True(t, found, "attributes: %v", duration.DataPoints[0].Attributes.ToSlice())
			require.NotEmpty(t, errType.AsString())
			_, found = duration.DataPoints[0].Attributes.Value("http.response.status_code")
			require.False(t, found)
		})
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
func TestClient_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the client never uses them, as the proposal for outbound HTTP clients, #762, requires.
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
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		stop := startPayments(t, h, h.Host(), ca, &ok{})
		defer stop()

		tel := humustest.NewTelemetry()
		require.NoError(t, callOnce(t, h, tel, ca, "https://payments:8443/charges/1"))
		require.Len(t, otelhttpSpans(tel), 1, "the Setup's TracerProvider recorded no span")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
}
