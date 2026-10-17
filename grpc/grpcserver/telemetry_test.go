// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats/opentelemetry"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
)

// The trace context a caller sends, from the W3C Trace Context
// specification's example of a traceparent header.
const (
	callerTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpanID  = "00f067aa0ba902b7"
	traceparent   = "00-" + callerTraceID + "-" + callerSpanID + "-01"
)

// emptyCall is the name of the span of an RPC to EmptyCall: the method,
// without its leading slash.
const emptyCall = "grpc.testing.TestService/EmptyCall"

// failingServer answers EmptyCall with an error of its code.
type failingServer struct {
	testpb.UnimplementedTestServiceServer

	code codes.Code
}

func (fs failingServer) EmptyCall(context.Context, *testpb.Empty) (*testpb.Empty, error) {
	return nil, status.Error(fs.code, "failing on purpose")
}

// callOnce runs an App whose only Service serves ts on ":9090", recording
// its telemetry in tel, makes one RPC to EmptyCall, with a traceparent in
// its metadata unless tp is empty, then stops the App. It returns the
// RPC's error once the App has exited, so every span the RPC started has
// ended.
func callOnce(t *testing.T, tel *humustest.Telemetry, ts testpb.TestServiceServer, tp string) error {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	p, exit := humustest.Start(t, serves(settingsFor(t, ca, ":9090"), ts),
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
	)
	<-h.Listening("carts:9090")

	ctx := t.Context()
	if tp != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "traceparent", tp)
	}
	client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
	_, rpcErr := client.EmptyCall(ctx, &testpb.Empty{})

	p.Signal(syscall.SIGTERM)
	st := <-exit
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.Empty(t, p.Stderr())
	return rpcErr
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

// attributes returns span's attributes by key.
func attributes(span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	attrs := make(map[attribute.Key]attribute.Value)
	for _, kv := range span.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	return attrs
}

func TestService_traces_each_RPC(t *testing.T) {
	t.Run("one server span, named after the method, with the RPC's attributes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			require.NoError(t, callOnce(t, tel, &testServer{}, ""))

			spans := serverSpans(tel)
			require.Len(t, spans, 1)
			span := spans[0]
			require.Equal(t, emptyCall, span.Name())
			require.Equal(t, trace.SpanKindServer, span.SpanKind())
			require.Equal(t, otelcodes.Unset, span.Status().Code)

			attrs := attributes(span)
			require.Equal(t, "grpc", attrs["rpc.system.name"].AsString())
			require.Equal(t, emptyCall, attrs["rpc.method"].AsString())
			require.Equal(t, int64(9090), attrs["server.port"].AsInt64())
			require.Equal(t, "OK", attrs["rpc.response.status_code"].AsString())
		})
	})

	t.Run("an RPC with a traceparent joins its caller's trace", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			require.NoError(t, callOnce(t, tel, &testServer{}, traceparent))

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

func TestService_sets_the_span_status_for_server_errors(t *testing.T) {
	testCases := []struct {
		name string
		code codes.Code
		// canonical is the code's name in rpc.response.status_code.
		canonical string
		want      otelcodes.Code
	}{
		{name: "Internal is an error", code: codes.Internal, canonical: "INTERNAL", want: otelcodes.Error},
		{name: "Unavailable is an error", code: codes.Unavailable, canonical: "UNAVAILABLE", want: otelcodes.Error},
		{name: "NotFound leaves the status unset", code: codes.NotFound, canonical: "NOT_FOUND", want: otelcodes.Unset},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := humustest.NewTelemetry()
				err := callOnce(t, tel, failingServer{code: tc.code}, "")
				require.Equal(t, tc.code, status.Code(err))

				spans := serverSpans(tel)
				require.Len(t, spans, 1)
				require.Equal(t, tc.want, spans[0].Status().Code)
				require.Equal(t, tc.canonical, attributes(spans[0])["rpc.response.status_code"].AsString())
			})
		})
	}
}

func TestService_measures_each_RPC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		require.NoError(t, callOnce(t, tel, &testServer{}, ""))

		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		var found *metricdata.Metrics
		for _, sm := range rm.ScopeMetrics {
			if sm.Scope.Name != otelgrpc.ScopeName {
				continue
			}
			for i := range sm.Metrics {
				if sm.Metrics[i].Name == "rpc.server.call.duration" {
					found = &sm.Metrics[i]
				}
			}
		}
		require.NotNil(t, found, "no rpc.server.call.duration in %+v", rm.ScopeMetrics)
		hist, ok := found.Data.(metricdata.Histogram[float64])
		require.True(t, ok, "rpc.server.call.duration is a %T, want a float64 histogram", found.Data)
		require.Len(t, hist.DataPoints, 1)
		require.Equal(t, uint64(1), hist.DataPoints[0].Count)
	})
}

func TestService_records_gRFC_A66_metrics_with_a_second_stats_handler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		settings := settingsFor(t, ca, ":9090")
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := grpcserver.NewService(s, settings, opentelemetry.ServerOption(opentelemetry.Options{
				MetricsOptions: opentelemetry.MetricsOptions{MeterProvider: s.Telemetry.MeterProvider},
			}))
			if err != nil {
				return nil, err
			}
			testpb.RegisterTestServiceServer(svc, &testServer{})
			return []humus.Service{svc}, nil
		}
		p, exit := humustest.Start(t, init, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		_, err := client.EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)

		// The stats handlers record the RPC's metrics after its status is
		// sent, so they are read once the App has exited and every handler
		// has returned.
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		names := make(map[string][]string)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				names[m.Name] = append(names[m.Name], sm.Scope.Name)
			}
		}
		require.Contains(t, names, "grpc.server.call.duration", "the A66 metric, from the second stats handler")
		require.Equal(t, []string{otelgrpc.ScopeName}, names["rpc.server.call.duration"], "otelgrpc's metric, from humus's stats handler")
		require.Len(t, serverSpans(tel), 1, "the second stats handler adds no span")
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

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the Service never uses them, as the gRPC services proposal, #681, requires.
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
		require.NoError(t, callOnce(t, tel, &testServer{}, traceparent))
		require.Len(t, serverSpans(tel), 1, "the Setup's TracerProvider recorded no span")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
}
