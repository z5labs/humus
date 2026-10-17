// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
)

// loggerName is the name of the logger the Service logs a panic through.
const loggerName = "github.com/z5labs/humus/grpc/grpcserver"

// errBoom is what the panicking handlers and interceptors in this file
// panic with.
var errBoom = errors.New("boom")

// panickingServer panics with errBoom in UnaryCall, and in
// StreamingOutputCall once it has sent one message. Its EmptyCall answers
// at once.
type panickingServer struct {
	testpb.UnimplementedTestServiceServer
}

func (panickingServer) EmptyCall(context.Context, *testpb.Empty) (*testpb.Empty, error) {
	return &testpb.Empty{}, nil
}

func (panickingServer) UnaryCall(context.Context, *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	panic(errBoom)
}

func (panickingServer) StreamingOutputCall(_ *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	if err := stream.Send(&testpb.StreamingOutputCallResponse{}); err != nil {
		return err
	}
	panic(errBoom)
}

// panicsOnce returns an end user's unary interceptor that panics with
// errBoom on the first RPC, and calls the handler on every other one.
func panicsOnce() grpc.UnaryServerInterceptor {
	var calls atomic.Int64
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if calls.Add(1) == 1 {
			panic(errBoom)
		}
		return handler(ctx, req)
	}
}

// panicRun is what an App whose RPCs panic recorded.
type panicRun struct {
	spans  []sdktrace.ReadOnlySpan
	errors []sdklog.Record
	stderr string
}

// runPanics runs an App whose only Service serves ts on ":9090", built
// with opts, calls rpcs with a client of it, then stops the App, and
// returns what it recorded once it has exited.
func runPanics(t *testing.T, ts testpb.TestServiceServer, rpcs func(testpb.TestServiceClient), opts ...grpc.ServerOption) panicRun {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()
	p, exit := humustest.Start(t, serves(settingsFor(t, ca, ":9090"), ts, opts...),
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
	)
	<-h.Listening("carts:9090")

	rpcs(testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool())))

	p.Signal(syscall.SIGTERM)
	st := <-exit
	require.Zero(t, st.Code(), "cause: %v", st.Cause())

	run := panicRun{spans: serverSpans(tel), stderr: p.Stderr()}
	for _, r := range tel.Logs() {
		if r.Severity() == log.SeverityError {
			run.errors = append(run.errors, r)
		}
	}
	return run
}

// requireInternal fails t unless err is the error of an RPC that
// panicked: codes.Internal, with a message that says nothing of the panic.
func requireInternal(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "%v is not a gRPC status", err)
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "internal error", st.Message())
}

// spanOf returns the one span of run named name.
func spanOf(t *testing.T, run panicRun, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range run.spans {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "spans named %q", name)
	return found[0]
}

// recordAttr returns the value of r's attribute key, and whether it has one.
func recordAttr(r sdklog.Record, key string) (attribute.Value, bool) {
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

// requireRecorded fails t unless run holds exactly one ERROR record, of
// the Service's logger, for the panic of the RPC whose span is span, and
// span has the panic as an exception event and the status Error.
func requireRecorded(t *testing.T, run panicRun, span sdktrace.ReadOnlySpan) {
	t.Helper()
	require.Equal(t, otelcodes.Error, span.Status().Code)

	var exceptions []sdktrace.Event
	for _, ev := range span.Events() {
		if ev.Name == "exception" {
			exceptions = append(exceptions, ev)
		}
	}
	require.Len(t, exceptions, 1)
	evAttrs := make(map[attribute.Key]attribute.Value)
	for _, kv := range exceptions[0].Attributes {
		evAttrs[kv.Key] = kv.Value
	}
	require.Equal(t, "boom", evAttrs["exception.message"].AsString())
	require.Contains(t, evAttrs["exception.stacktrace"].AsString(), "panic_test.go")

	require.Len(t, run.errors, 1)
	rec := run.errors[0]
	require.Equal(t, loggerName, rec.InstrumentationScope().Name)
	require.Equal(t, "ERROR", rec.SeverityText())
	require.Equal(t, span.SpanContext().TraceID(), rec.TraceID())
	require.Equal(t, span.SpanContext().SpanID(), rec.SpanID())

	method, ok := recordAttr(rec, "rpc.method")
	require.True(t, ok, "the record has no rpc.method")
	require.Equal(t, span.Name(), method.AsString())
	msg, ok := recordAttr(rec, "exception.message")
	require.True(t, ok, "the record has no exception.message")
	require.Equal(t, "boom", msg.AsString())
	stack, ok := recordAttr(rec, "exception.stacktrace")
	require.True(t, ok, "the record has no exception.stacktrace")
	require.Contains(t, stack.AsString(), "panic_test.go", "the stack trace does not reach the panic")

	require.Empty(t, run.stderr)
}

func TestService_a_unary_panic(t *testing.T) {
	t.Run("fails the RPC with Internal, and the next RPC succeeds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var panicErr, nextErr error
			runPanics(t, panickingServer{}, func(c testpb.TestServiceClient) {
				_, panicErr = c.UnaryCall(t.Context(), &testpb.SimpleRequest{})
				_, nextErr = c.EmptyCall(t.Context(), &testpb.Empty{})
			})

			requireInternal(t, panicErr)
			require.NotContains(t, panicErr.Error(), "boom", "the panic's value reached the client")
			require.NoError(t, nextErr)
		})
	})

	t.Run("is recorded on the RPC's span and in one ERROR record", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			run := runPanics(t, panickingServer{}, func(c testpb.TestServiceClient) {
				_, err := c.UnaryCall(t.Context(), &testpb.SimpleRequest{})
				requireInternal(t, err)
			})

			span := spanOf(t, run, "grpc.testing.TestService/UnaryCall")
			requireRecorded(t, run, span)
		})
	})
}

func TestService_a_stream_panic_ends_its_stream_with_Internal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var received int
		var streamErr, nextErr error
		run := runPanics(t, panickingServer{}, func(c testpb.TestServiceClient) {
			stream, err := c.StreamingOutputCall(t.Context(), &testpb.StreamingOutputCallRequest{})
			require.NoError(t, err)
			for {
				_, err := stream.Recv()
				if err != nil {
					streamErr = err
					break
				}
				received++
			}
			_, nextErr = c.EmptyCall(t.Context(), &testpb.Empty{})
		})

		require.Equal(t, 1, received, "want the message sent before the panic")
		require.NotErrorIs(t, streamErr, io.EOF, "the stream ended cleanly")
		requireInternal(t, streamErr)
		require.NoError(t, nextErr)

		span := spanOf(t, run, "grpc.testing.TestService/StreamingOutputCall")
		requireRecorded(t, run, span)
	})
}

func TestService_an_interceptors_panic_is_recovered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var panicErr, nextErr error
		run := runPanics(t, &testServer{}, func(c testpb.TestServiceClient) {
			_, panicErr = c.EmptyCall(t.Context(), &testpb.Empty{})
			_, nextErr = c.EmptyCall(t.Context(), &testpb.Empty{})
		}, grpc.ChainUnaryInterceptor(panicsOnce()))

		requireInternal(t, panicErr)
		require.NoError(t, nextErr)

		var failed []sdktrace.ReadOnlySpan
		for _, s := range run.spans {
			if s.Status().Code == otelcodes.Error {
				failed = append(failed, s)
			}
		}
		require.Len(t, run.spans, 2)
		require.Len(t, failed, 1)
		requireRecorded(t, run, failed[0])
	})
}
