// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server_test

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/internal/server"
	"github.com/z5labs/humus/humustest"
)

// panickingServer panics in UnaryCall, and in StreamingOutputCall once it
// has sent one message. Its EmptyCall answers at once.
type panickingServer struct {
	testpb.UnimplementedTestServiceServer
}

func (panickingServer) EmptyCall(context.Context, *testpb.Empty) (*testpb.Empty, error) {
	return &testpb.Empty{}, nil
}

func (panickingServer) UnaryCall(context.Context, *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	panic(errors.New("boom"))
}

func (panickingServer) StreamingOutputCall(_ *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	if err := stream.Send(&testpb.StreamingOutputCallResponse{}); err != nil {
		return err
	}
	panic("boom")
}

// requireInternal fails t unless err is the error of an RPC that panicked.
func requireInternal(t *testing.T, err error) {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "%v is not a gRPC status", err)
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "internal error", st.Message())
}

// The panic is logged under the scope New was given, callerScope, and not
// under grpcserver's: each package that builds a Server logs its panics as
// its own.
func TestServer_recovers_a_panic_of_an_RPC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			srv, err := server.New(s, callerScope, ":8081")
			if err != nil {
				return nil, err
			}
			if err := srv.RegisterService(&testpb.TestService_ServiceDesc, panickingServer{}); err != nil {
				return nil, err
			}
			return []humus.Service{srv}, nil
		}
		p, exit := humustest.Start(t, init, humustest.WithApp(
			humus.WithHost(h.Host()),
			humus.WithTelemetry(tel.Builder()),
		))
		<-h.Listening("localhost:8081")
		client := plaintext(t, h, "localhost:8081")

		_, err := client.UnaryCall(t.Context(), &testpb.SimpleRequest{})
		requireInternal(t, err)

		stream, err := client.StreamingOutputCall(t.Context(), &testpb.StreamingOutputCallRequest{})
		require.NoError(t, err)
		_, err = stream.Recv()
		require.NoError(t, err)
		_, err = stream.Recv()
		requireInternal(t, err)

		_, err = client.EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)

		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())

		var panics []string
		for _, r := range tel.Logs() {
			if r.Severity() == log.SeverityError {
				require.Equal(t, callerScope, r.InstrumentationScope().Name)
				r.WalkAttributes(func(kv attribute.KeyValue) bool {
					if kv.Key == "rpc.method" {
						panics = append(panics, kv.Value.AsString())
					}
					return true
				})
			}
		}
		require.Equal(t, []string{
			"grpc.testing.TestService/UnaryCall",
			"grpc.testing.TestService/StreamingOutputCall",
		}, panics)
	})
}
