// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server_test

import (
	"context"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/grpc/internal/server"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The Server knows nothing of TLS, so these tests serve plain text, as a
// health server built on it would: every client passes insecure
// credentials.

// testServer answers EmptyCall, and leaves every other method
// unimplemented.
type testServer struct {
	testpb.UnimplementedTestServiceServer
}

func (testServer) EmptyCall(context.Context, *testpb.Empty) (*testpb.Empty, error) {
	return &testpb.Empty{}, nil
}

// newSetup returns a Setup on h.
func newSetup(h *humustest.Host) *humus.Setup {
	return &humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}
}

// plaintext returns a client of address in h that speaks plain text.
func plaintext(t *testing.T, h *humustest.Host, address string) testpb.TestServiceClient {
	return testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, address, nil,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	))
}

func TestServer_meets_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		srv, err := server.New(s, ":8081")
		require.NoError(t, err)
		require.NoError(t, srv.RegisterService(&testpb.TestService_ServiceDesc, testServer{}))
		return srv
	})
}

func TestServer_serves_plain_text_through_the_drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			srv, err := server.New(s, ":8081")
			if err != nil {
				return nil, err
			}
			if err := srv.RegisterService(&testpb.TestService_ServiceDesc, testServer{}); err != nil {
				return nil, err
			}
			return []humus.Service{srv}, nil
		}
		p, exit := humustest.Start(t, init, humustest.WithApp(
			humus.WithHost(h.Host()),
			humus.WithTelemetry(telemetry.NoopBuilder),
			humus.WithDrain(humus.DrainDelay(5*time.Second)),
		))
		<-h.Listening("localhost:8081")

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		time.Sleep(time.Second)
		_, err := plaintext(t, h, "localhost:8081").EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)

		require.Zero(t, (<-exit).Code())
	})
}

func TestNew_returns_the_error_of_Listen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		first, err := server.New(newSetup(h), ":8081")
		require.NoError(t, err)
		t.Cleanup(func() { _ = first.Shutdown(context.Background()) })

		second, err := server.New(newSetup(h), ":8081")
		require.Nil(t, second)
		require.ErrorIs(t, err, syscall.EADDRINUSE)
	})
}

func TestServer_RegisterService_refuses_what_grpc_go_exits_on(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		srv, err := server.New(newSetup(h), ":8081")
		require.NoError(t, err)

		err = srv.RegisterService(&testpb.TestService_ServiceDesc, struct{}{})
		require.ErrorContains(t, err, `service "grpc.testing.TestService": the implementation, of type struct {}, does not implement grpc_testing.TestServiceServer`)

		require.NoError(t, srv.RegisterService(&testpb.TestService_ServiceDesc, testServer{}))
		err = srv.RegisterService(&testpb.TestService_ServiceDesc, testServer{})
		require.ErrorContains(t, err, `service "grpc.testing.TestService": already registered`)

		served := make(chan error, 1)
		go func() { served <- srv.Serve(t.Context()) }()
		synctest.Wait()
		err = srv.RegisterService(&testpb.UnimplementedService_ServiceDesc, testpb.UnimplementedUnimplementedServiceServer{})
		require.ErrorContains(t, err, `service "grpc.testing.UnimplementedService": registered after Serve`)

		require.Len(t, srv.GetServiceInfo(), 1)
		require.NoError(t, srv.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestServer_Shutdown_closes_the_listener_before_Serve(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		srv, err := server.New(newSetup(h), "localhost:0")
		require.NoError(t, err)
		addr := srv.Addr().String()

		require.NoError(t, srv.Shutdown(t.Context()))
		_, err = h.Dial(t.Context(), "tcp", addr)
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
		require.NoError(t, srv.Serve(t.Context()))
	})
}
