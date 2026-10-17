// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	otelcodes "go.opentelemetry.io/otel/codes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
)

// subscriber's StreamingOutputCall sends one response, then waits for its
// context, as a subscription does.
type subscriber struct {
	testpb.UnimplementedTestServiceServer
}

func (subscriber) StreamingOutputCall(_ *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	if err := stream.Send(&testpb.StreamingOutputCallResponse{}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestMain_shutdown_ends_a_stream_with_Unavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, exit := grpcservertest.Start(t, serves(settingsFor(t, ca, ":9090"), subscriber{}),
			humustest.WithApp(humus.WithHost(h.Host())),
		)
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		stream, err := client.StreamingOutputCall(t.Context(), &testpb.StreamingOutputCallRequest{})
		require.NoError(t, err)
		_, err = stream.Recv()
		require.NoError(t, err)

		<-p.Notified(syscall.SIGTERM)
		begin := time.Now()
		p.Signal(syscall.SIGTERM)
		_, err = stream.Recv()
		require.Equal(t, codes.Unavailable, status.Code(err), err)
		require.Equal(t, "server is shutting down", status.Convert(err).Message())
		require.Equal(t, 5*time.Second, time.Since(begin)) // the drain, then Shutdown ends the stream

		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

// waitingServer's three streaming handlers, a server, a client and a
// bidirectional stream, close entered, wait for their context, and then
// return what end returns for it. None of them receives.
type waitingServer struct {
	testpb.UnimplementedTestServiceServer

	entered chan struct{}
	end     func(ctx context.Context) error
}

func (ws *waitingServer) wait(ctx context.Context) error {
	close(ws.entered)
	<-ctx.Done()
	return ws.end(ctx)
}

func (ws *waitingServer) StreamingOutputCall(_ *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	return ws.wait(stream.Context())
}

func (ws *waitingServer) StreamingInputCall(stream grpc.ClientStreamingServer[testpb.StreamingInputCallRequest, testpb.StreamingInputCallResponse]) error {
	return ws.wait(stream.Context())
}

func (ws *waitingServer) FullDuplexCall(stream grpc.BidiStreamingServer[testpb.StreamingOutputCallRequest, testpb.StreamingOutputCallResponse]) error {
	return ws.wait(stream.Context())
}

// streamKind opens one kind of stream with client, and returns a function
// that waits for the stream's end and returns its status.
type streamKind struct {
	name string
	open func(t *testing.T, client testpb.TestServiceClient) (end func() error)
}

var streamKinds = []streamKind{
	{
		name: "a server stream",
		open: func(t *testing.T, client testpb.TestServiceClient) func() error {
			stream, err := client.StreamingOutputCall(t.Context(), &testpb.StreamingOutputCallRequest{})
			require.NoError(t, err)
			return func() error {
				_, err := stream.Recv()
				return err
			}
		},
	},
	{
		name: "a client stream",
		open: func(t *testing.T, client testpb.TestServiceClient) func() error {
			stream, err := client.StreamingInputCall(t.Context())
			require.NoError(t, err)
			require.NoError(t, stream.Send(&testpb.StreamingInputCallRequest{}))
			return func() error {
				_, err := stream.CloseAndRecv()
				return err
			}
		},
	},
	{
		name: "a bidirectional stream",
		open: func(t *testing.T, client testpb.TestServiceClient) func() error {
			stream, err := client.FullDuplexCall(t.Context())
			require.NoError(t, err)
			require.NoError(t, stream.Send(&testpb.StreamingOutputCallRequest{}))
			return func() error {
				_, err := stream.Recv()
				return err
			}
		},
	},
}

// streamEnd is how a stream ended by Shutdown ended, seen from its client.
type streamEnd struct {
	err   error
	after time.Duration // from SIGTERM
	exit  int
	cause error
}

// endStream runs an App on the gRPC defaults whose only Service serves a
// waitingServer, whose handlers return end(ctx) once their context is
// done. It opens a stream of kind, sends SIGTERM once the handler runs,
// and returns how the stream and the App ended.
func endStream(t *testing.T, kind streamKind, end func(ctx context.Context) error) streamEnd {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	ws := &waitingServer{entered: make(chan struct{}), end: end}
	p, exit := grpcservertest.Start(t, serves(settingsFor(t, ca, ":9090"), ws),
		humustest.WithApp(humus.WithHost(h.Host())),
	)
	<-h.Listening("carts:9090")

	client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
	wait := kind.open(t, client)
	<-ws.entered

	<-p.Notified(syscall.SIGTERM)
	sigterm := time.Now()
	p.Signal(syscall.SIGTERM)
	err := wait()
	after := time.Since(sigterm)
	st := <-exit
	return streamEnd{err: err, after: after, exit: st.Code(), cause: st.Cause()}
}

func TestService_Shutdown_ends_every_kind_of_stream(t *testing.T) {
	endings := []struct {
		name string
		end  func(ctx context.Context) error
		want codes.Code
	}{
		{
			name: "with Unavailable when its handler returns its context's error",
			end:  func(ctx context.Context) error { return ctx.Err() },
			want: codes.Unavailable,
		},
		{
			name: "with Unavailable, not OK, when its handler returns nil",
			end:  func(context.Context) error { return nil },
			want: codes.Unavailable,
		},
		{
			name: "with Unavailable when its handler returns a status with Canceled",
			end:  func(context.Context) error { return status.Error(codes.Canceled, "cancelled") },
			want: codes.Unavailable,
		},
		{
			name: "with Unavailable when its handler returns its context's cause",
			end:  func(ctx context.Context) error { return context.Cause(ctx) },
			want: codes.Unavailable,
		},
		{
			name: "with the handler's own status when it returns Aborted",
			end:  func(context.Context) error { return status.Error(codes.Aborted, "aborted on purpose") },
			want: codes.Aborted,
		},
	}
	for _, kind := range streamKinds {
		t.Run(kind.name, func(t *testing.T) {
			for _, ending := range endings {
				t.Run(ending.name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						got := endStream(t, kind, ending.end)

						require.Equal(t, ending.want, status.Code(got.err), got.err)
						if ending.want == codes.Aborted {
							require.Equal(t, "aborted on purpose", status.Convert(got.err).Message())
						} else {
							require.Equal(t, "server is shutting down", status.Convert(got.err).Message())
						}
						require.Equal(t, 5*time.Second, got.after, "the drain, then Shutdown ends the stream")
						require.Zero(t, got.exit, "cause: %v", got.cause)
					})
				})
			}
		})
	}
}

// unaryAndStream answers UnaryCall as its testServer does, and
// StreamingOutputCall as a subscriber does.
type unaryAndStream struct {
	*testServer
}

func (unaryAndStream) StreamingOutputCall(req *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	return subscriber{}.StreamingOutputCall(req, stream)
}

func TestService_Shutdown_does_not_cancel_a_unary_RPC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		ts := &testServer{entered: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan time.Time, 1)}
		p, exit := grpcservertest.Start(t, serves(settingsFor(t, ca, ":9090"), unaryAndStream{ts}),
			humustest.WithApp(humus.WithHost(h.Host())),
		)
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		got := call(t, client)
		<-ts.entered // the unary RPC is in flight
		stream, err := client.StreamingOutputCall(t.Context(), &testpb.StreamingOutputCallRequest{})
		require.NoError(t, err)
		_, err = stream.Recv()
		require.NoError(t, err)

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)

		// Shutdown has started: it ended the stream.
		_, err = stream.Recv()
		require.Equal(t, codes.Unavailable, status.Code(err), err)
		require.Equal(t, 5*time.Second, time.Since(sigterm))

		time.Sleep(time.Second)
		synctest.Wait()
		require.Empty(t, ts.cancelled, "the unary RPC's context is done")
		close(ts.release)

		require.Equal(t, "done", <-got)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, 6*time.Second, time.Since(sigterm), "Shutdown waited for the unary RPC")
	})
}

// receiver's FullDuplexCall receives until Recv fails, closing entered once
// its first message arrives, and sends the time Recv failed on returned.
type receiver struct {
	testpb.UnimplementedTestServiceServer

	entered  chan struct{}
	returned chan time.Time
}

func (r receiver) FullDuplexCall(stream grpc.BidiStreamingServer[testpb.StreamingOutputCallRequest, testpb.StreamingOutputCallResponse]) error {
	for first := true; ; first = false {
		if _, err := stream.Recv(); err != nil {
			r.returned <- time.Now()
			return err
		}
		if first {
			close(r.entered)
		}
	}
}

func TestService_a_handler_blocked_in_Recv_holds_Shutdown_until_the_grace_period(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		r := receiver{entered: make(chan struct{}), returned: make(chan time.Time, 1)}
		p, exit := grpcservertest.Start(t, serves(settingsFor(t, ca, ":9090"), r),
			humustest.WithApp(humus.WithHost(h.Host())),
			humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"}),
		)
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		stream, err := client.FullDuplexCall(t.Context())
		require.NoError(t, err)
		require.NoError(t, stream.Send(&testpb.StreamingOutputCallRequest{}))
		<-r.entered // the handler is blocked in its second Recv

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)

		require.Equal(t, 7*time.Second, (<-r.returned).Sub(sigterm), "the drain's 5s, then the grace period's 2s")

		st := <-exit
		require.Equal(t, humus.ExitFailure, st.Code())
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
		require.Equal(t, 2*time.Second, gerr.GracePeriod)
	})
}

func TestService_a_stream_ended_by_Shutdown_records_an_Error_span(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, exit := grpcservertest.Start(t, serves(settingsFor(t, ca, ":9090"), subscriber{}),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		stream, err := client.StreamingOutputCall(t.Context(), &testpb.StreamingOutputCallRequest{})
		require.NoError(t, err)
		_, err = stream.Recv()
		require.NoError(t, err)

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		_, err = stream.Recv()
		require.Equal(t, codes.Unavailable, status.Code(err), err)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		spans := serverSpans(tel)
		require.Len(t, spans, 1)
		require.Equal(t, "grpc.testing.TestService/StreamingOutputCall", spans[0].Name())
		require.Equal(t, otelcodes.Error, spans[0].Status().Code)
		require.Equal(t, "UNAVAILABLE", attributes(spans[0])["rpc.response.status_code"].AsString())
	})
}

func TestService_the_end_users_stream_interceptors_see_the_context_Shutdown_cancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		entered := make(chan struct{})
		// waits never calls its handler: it waits for the context it is
		// given, as a handler would.
		waits := func(_ any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
			close(entered)
			<-ss.Context().Done()
			return ss.Context().Err()
		}
		p, exit := grpcservertest.Start(t, serves(settingsFor(t, ca, ":9090"), subscriber{}, grpc.ChainStreamInterceptor(waits)),
			humustest.WithApp(humus.WithHost(h.Host())),
		)
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		stream, err := client.StreamingOutputCall(t.Context(), &testpb.StreamingOutputCallRequest{})
		require.NoError(t, err)
		<-entered

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		_, err = stream.Recv()
		require.Equal(t, codes.Unavailable, status.Code(err), err)
		require.Equal(t, 5*time.Second, time.Since(sigterm), "the interceptor's context was cancelled when Shutdown started")

		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}
