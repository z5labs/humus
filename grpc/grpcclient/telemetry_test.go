// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/humustest"
)

// emptyCallSpan is the name of the span of an attempt at EmptyCall: the
// full method, without its leading slash.
const emptyCallSpan = "grpc.testing.TestService/EmptyCall"

// otelgrpcSpans returns the spans of kind in tel that otelgrpc recorded.
// The backend runs in the same App as the Client, so tel holds the
// backend's server spans beside the Client's client spans.
func otelgrpcSpans(tel *humustest.Telemetry, kind trace.SpanKind) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range tel.Spans() {
		if s.InstrumentationScope().Name == otelgrpc.ScopeName && s.SpanKind() == kind {
			spans = append(spans, s)
		}
	}
	return spans
}

// jobSpan returns the span of the job's run in tel.
func jobSpan(t *testing.T, tel *humustest.Telemetry) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range tel.Spans() {
		if s.Name() == "job" && s.InstrumentationScope().Name != otelgrpc.ScopeName {
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

// traced returns the App of the tests in this file: it records its
// telemetry in tel, serves b at backend:9090 with a certificate from a new
// CA, and builds a Client of it, which work calls.
func traced(t *testing.T, tel *humustest.Telemetry, b *backend, work func(ctx context.Context, c testpb.TestServiceClient) error) app {
	t.Helper()
	ca := humustest.NewCA(t)
	return app{
		host:      humustest.NewHost(t).Host(),
		tel:       tel.Builder(),
		backend:   b,
		serverTLS: serving(t, ca, "backend"),
		settings:  settingsFor(ca, "backend:9090"),
		work:      work,
	}
}

func TestClient_traces_each_attempt(t *testing.T) {
	t.Run("a call is one CLIENT span named after the method, a child of the job's span", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			_, err := traced(t, tel, &backend{}, emptyCall).run(t)
			require.NoError(t, err)

			spans := otelgrpcSpans(tel, trace.SpanKindClient)
			require.Len(t, spans, 1)
			span := spans[0]
			require.Equal(t, emptyCallSpan, span.Name())
			require.Equal(t, otelcodes.Unset, span.Status().Code)

			job := jobSpan(t, tel).SpanContext()
			require.Equal(t, job.TraceID(), span.SpanContext().TraceID())
			require.Equal(t, job.SpanID(), span.Parent().SpanID(), "the attempt is a child of the job's span")

			a := attrs(span)
			require.Equal(t, "grpc", a["rpc.system.name"].AsString())
			require.Equal(t, emptyCallSpan, a["rpc.method"].AsString())
			require.Equal(t, "OK", a["rpc.response.status_code"].AsString())
		})
	})

	t.Run("a server-streaming RPC read to io.EOF is one CLIENT span", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			_, err := traced(t, tel, &backend{}, func(ctx context.Context, c testpb.TestServiceClient) error {
				stream, err := c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{
					ResponseParameters: []*testpb.ResponseParameters{{}, {}, {}},
				})
				if err != nil {
					return err
				}
				for {
					if _, err := stream.Recv(); err != nil {
						return err
					}
				}
			}).run(t)
			require.ErrorIs(t, err, io.EOF)

			spans := otelgrpcSpans(tel, trace.SpanKindClient)
			require.Len(t, spans, 1)
			require.Equal(t, "grpc.testing.TestService/StreamingOutputCall", spans[0].Name())
			require.Equal(t, "OK", attrs(spans[0])["rpc.response.status_code"].AsString())
		})
	})
}

func TestClient_sets_the_span_status_Error_for_every_code_but_OK(t *testing.T) {
	t.Run("a call answered NotFound", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			b := &backend{answer: func(context.Context) error {
				return status.Error(codes.NotFound, "no such cart")
			}}
			_, err := traced(t, tel, b, emptyCall).run(t)
			require.Equal(t, codes.NotFound, status.Code(err), "err = %v", err)

			spans := otelgrpcSpans(tel, trace.SpanKindClient)
			require.Len(t, spans, 1)
			require.Equal(t, otelcodes.Error, spans[0].Status().Code)
			require.Equal(t, "NOT_FOUND", attrs(spans[0])["rpc.response.status_code"].AsString())
		})
	})

	t.Run("a call to an address nothing listens on", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			settings := grpcclient.DefaultSettings()
			settings.Addr = "nowhere:9090"
			_, err := app{host: humustest.NewHost(t).Host(), tel: tel.Builder(), settings: settings, work: emptyCall}.run(t)
			require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)

			spans := otelgrpcSpans(tel, trace.SpanKindClient)
			require.Len(t, spans, 3, "one span for each of the three attempts")
			for _, span := range spans {
				require.Equal(t, emptyCallSpan, span.Name())
				require.Equal(t, otelcodes.Error, span.Status().Code)
				require.Equal(t, "UNAVAILABLE", attrs(span)["rpc.response.status_code"].AsString())
			}
		})
	})
}

func TestClient_propagates_trace_context_and_baggage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		var tenant string
		b := &backend{answer: func(ctx context.Context) error {
			tenant = baggage.FromContext(ctx).Member("tenant").Value()
			return nil
		}}
		_, err := traced(t, tel, b, func(ctx context.Context, c testpb.TestServiceClient) error {
			m, err := baggage.NewMember("tenant", "acme")
			if err != nil {
				return err
			}
			bag, err := baggage.New(m)
			if err != nil {
				return err
			}
			return emptyCall(baggage.ContextWithBaggage(ctx, bag), c)
		}).run(t)
		require.NoError(t, err)

		client := otelgrpcSpans(tel, trace.SpanKindClient)
		require.Len(t, client, 1)
		server := otelgrpcSpans(tel, trace.SpanKindServer)
		require.Len(t, server, 1)
		require.Equal(t, client[0].SpanContext().TraceID(), server[0].SpanContext().TraceID(), "the backend joins the caller's trace")
		require.True(t, server[0].Parent().IsRemote(), "the server span's parent came in the call's metadata")
		require.Equal(t, client[0].SpanContext().SpanID(), server[0].Parent().SpanID(), "the client span is the server span's parent")
		require.Equal(t, "acme", tenant, "the caller's baggage reaches the handler")
	})
}

func TestClient_measures_each_attempt(t *testing.T) {
	t.Run("each attempt records rpc.client.call.duration in otelgrpc's scope", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			_, err := traced(t, tel, &backend{}, emptyCall).run(t)
			require.NoError(t, err)

			rm, err := tel.Metrics(t.Context())
			require.NoError(t, err)
			var found *metricdata.Metrics
			for _, sm := range rm.ScopeMetrics {
				// The backend runs in the same App, so its metrics are in
				// rm too: otelgrpc's, in the same scope.
				require.Contains(t, []string{otelgrpc.ScopeName, clientScope}, sm.Scope.Name, "no other scope records an instrument")
				if sm.Scope.Name != otelgrpc.ScopeName {
					continue
				}
				for i := range sm.Metrics {
					if sm.Metrics[i].Name == "rpc.client.call.duration" {
						found = &sm.Metrics[i]
					}
				}
			}
			require.NotNil(t, found, "no rpc.client.call.duration in otelgrpc's scope: %+v", rm.ScopeMetrics)
			hist, ok := found.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "rpc.client.call.duration is a %T, want a float64 histogram", found.Data)
			require.Len(t, hist.DataPoints, 1)
			require.EqualValues(t, 1, hist.DataPoints[0].Count)
		})
	})

	t.Run("the package's scope holds humus's four instruments and nothing else, beside otelgrpc's", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Five failures open the breaker, so every instrument has a
			// data point.
			tel := humustest.NewTelemetry()
			a := traced(t, tel, &backend{answer: unavailable}, func(ctx context.Context, c testpb.TestServiceClient) error {
				for range 5 {
					_ = emptyCall(ctx, c)
				}
				return nil
			})
			a.settings.MaxAttempts = 1
			_, err := a.run(t)
			require.NoError(t, err)

			rm, err := tel.Metrics(t.Context())
			require.NoError(t, err)
			for _, sm := range rm.ScopeMetrics {
				require.Contains(t, []string{otelgrpc.ScopeName, clientScope}, sm.Scope.Name, "no other scope records an instrument")
			}
			type instrument struct {
				unit, description string
				monotonic         bool
			}
			got := map[string]instrument{}
			for _, m := range ownMetrics(rm) {
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "%s is a %T", m.Name, m.Data)
				require.Equal(t, metricdata.CumulativeTemporality, sum.Temporality)
				got[m.Name] = instrument{unit: m.Unit, description: m.Description, monotonic: sum.IsMonotonic}
				require.NotEmpty(t, points(t, rm, m.Name)) // every data point carries the dependency's attributes
			}
			require.Equal(t, map[string]instrument{
				"humus.circuit.state":       {unit: "{breaker}", description: "Circuit breakers in each state. Only the current state's value is non-zero.", monotonic: false},
				"humus.circuit.transitions": {unit: "{transition}", description: "Changes of state of a circuit breaker.", monotonic: true},
				"humus.circuit.calls":       {unit: "{call}", description: "Calls through a circuit breaker, by how each counted.", monotonic: true},
				"humus.retry.decisions":     {unit: "{decision}", description: "What retry decided after each attempt.", monotonic: true},
			}, got)
		})
	})
}

// countingHandler is the end user's stats.Handler. It counts the RPCs that
// ended.
type countingHandler struct {
	ended atomic.Int64
}

func (h *countingHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (h *countingHandler) HandleRPC(_ context.Context, s stats.RPCStats) {
	if _, ok := s.(*stats.End); ok {
		h.ended.Add(1)
	}
}

func (h *countingHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (h *countingHandler) HandleConn(context.Context, stats.ConnStats) {}

func TestNewClient_runs_a_stats_handler_in_opts_as_well(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		h := &countingHandler{}
		a := traced(t, tel, &backend{}, emptyCall)
		a.opts = []grpc.DialOption{grpc.WithStatsHandler(h)}
		_, err := a.run(t)
		require.NoError(t, err)

		require.EqualValues(t, 1, h.ended.Load(), "the end user's stats handler saw the call")
		require.Len(t, otelgrpcSpans(tel, trace.SpanKindClient), 1, "otelgrpc's stats handler still traced it")
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
func TestClient_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}
	logs := &countingExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the Client never uses them, as the proposal for outbound gRPC clients, #763, requires.
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
		_, err := traced(t, tel, &backend{}, emptyCall).run(t)
		require.NoError(t, err)
		require.Len(t, otelgrpcSpans(tel, trace.SpanKindClient), 1, "the Setup's TracerProvider recorded no client span")
		require.Equal(t, map[string]int64{"humus.circuit.outcome=success": 1},
			points(t, collect(t, context.Background(), tel), "humus.circuit.calls"), "the breaker counted the call in the Setup's MeterProvider")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
	require.Zero(t, logs.records.Load(), "the global LoggerProvider recorded records")
}
