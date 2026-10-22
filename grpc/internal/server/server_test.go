// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server_test

import (
	"context"
	"io"
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

// testServer answers EmptyCall at once. Its UnaryCall closes entered, if
// it is set, and returns its context's error once its context is done.
type testServer struct {
	testpb.UnimplementedTestServiceServer

	entered chan struct{}
}

func (ts testServer) UnaryCall(ctx context.Context, _ *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	if ts.entered != nil {
		close(ts.entered)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (testServer) EmptyCall(context.Context, *testpb.Empty) (*testpb.Empty, error) {
	return &testpb.Empty{}, nil
}

// callerScope is the logger scope the tests build their Servers with, as
// the package building a Server passes its own import path.
const callerScope = "example.com/caller"

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
		srv, err := server.New(s, callerScope, ":8081", nil)
		require.NoError(t, err)
		require.NoError(t, srv.RegisterService(&testpb.TestService_ServiceDesc, testServer{}))
		return srv
	})
}

func TestServer_serves_plain_text_through_the_drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			srv, err := server.New(s, callerScope, ":8081", nil)
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
		first, err := server.New(newSetup(h), callerScope, ":8081", nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = first.Shutdown(context.Background()) })

		second, err := server.New(newSetup(h), callerScope, ":8081", nil)
		require.Nil(t, second)
		require.ErrorIs(t, err, syscall.EADDRINUSE)
	})
}

func TestServer_RegisterService_refuses_what_grpc_go_exits_on(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		srv, err := server.New(newSetup(h), callerScope, ":8081", nil)
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
		srv, err := server.New(newSetup(h), callerScope, "localhost:0", nil)
		require.NoError(t, err)
		addr := srv.Addr().String()

		require.NoError(t, srv.Shutdown(t.Context()))
		_, err = h.Dial(t.Context(), "tcp", addr)
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
		require.NoError(t, srv.Serve(t.Context()))
	})
}

func TestServer_Shutdown_called_during_another_waits_for_it(t *testing.T) {
	t.Run("and returns nil when the first stops", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ts := testServer{entered: make(chan struct{})}
			srv, err := server.New(newSetup(h), callerScope, "localhost:8081", nil)
			require.NoError(t, err)
			require.NoError(t, srv.RegisterService(&testpb.TestService_ServiceDesc, ts))
			served := make(chan error, 1)
			go func() { served <- srv.Serve(t.Context()) }()

			rpcCtx, endRPC := context.WithCancel(t.Context())
			called := make(chan error, 1)
			go func() {
				_, err := plaintext(t, h, "localhost:8081").UnaryCall(rpcCtx, &testpb.SimpleRequest{})
				called <- err
			}()
			<-ts.entered

			// Both wait for the RPC, which ends when its client cancels it.
			first := make(chan error, 1)
			go func() { first <- srv.Shutdown(t.Context()) }()
			synctest.Wait()
			second := make(chan error, 1)
			go func() { second <- srv.Shutdown(t.Context()) }()
			synctest.Wait()
			select {
			case err := <-second:
				t.Fatalf("the second Shutdown returned %v while the first was waiting", err)
			default:
			}

			endRPC()
			require.Error(t, <-called)
			require.NoError(t, <-first)
			require.NoError(t, <-second)
			require.NoError(t, <-served)
		})
	})

	t.Run("and stops the server when its own context is done first", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ts := testServer{entered: make(chan struct{})}
			srv, err := server.New(newSetup(h), callerScope, "localhost:8081", nil)
			require.NoError(t, err)
			require.NoError(t, srv.RegisterService(&testpb.TestService_ServiceDesc, ts))
			served := make(chan error, 1)
			go func() { served <- srv.Serve(t.Context()) }()

			called := make(chan error, 1)
			go func() {
				_, err := plaintext(t, h, "localhost:8081").UnaryCall(t.Context(), &testpb.SimpleRequest{})
				called <- err
			}()
			<-ts.entered

			// The first Shutdown waits for the RPC, which never returns on
			// its own.
			first := make(chan error, 1)
			go func() { first <- srv.Shutdown(context.Background()) }()
			synctest.Wait()

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			start := time.Now()
			err = srv.Shutdown(ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Equal(t, 2*time.Second, time.Since(start))

			require.NoError(t, <-first, "the first Shutdown's own context never ran out")
			require.Error(t, <-called, "the RPC was cut off")
			require.NoError(t, <-served)
		})
	})
}

func TestServer_Shutdown_closes_a_connection_in_its_handshake(t *testing.T) {
	testCases := []struct {
		name    string
		ctx     func(t *testing.T) context.Context
		wantErr error
	}{
		{name: "and sends its GOAWAY at once", ctx: func(t *testing.T) context.Context { return t.Context() }},
		{name: "and stops at once when its context is done already", ctx: func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		}, wantErr: context.Canceled},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				srv, err := server.New(newSetup(h), callerScope, "localhost:8081", nil)
				require.NoError(t, err)
				require.NoError(t, srv.RegisterService(&testpb.TestService_ServiceDesc, testServer{}))
				served := make(chan error, 1)
				go func() { served <- srv.Serve(t.Context()) }()
				_, err = plaintext(t, h, "localhost:8081").EmptyCall(t.Context(), &testpb.Empty{})
				require.NoError(t, err)

				// A client that never sends its preface holds its connection
				// in grpc-go's handshake for 120s.
				conn, err := h.Dial(t.Context(), "tcp", "localhost:8081")
				require.NoError(t, err)
				defer func() { _ = conn.Close() }()
				synctest.Wait()

				begin := time.Now()
				err = srv.Shutdown(tc.ctx(t))
				if tc.wantErr == nil {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, tc.wantErr)
				}
				require.Zero(t, time.Since(begin), "Shutdown waits for no handshake")
				_, err = io.ReadAll(conn)
				require.NoError(t, err, "the server closed the connection")
				require.NoError(t, <-served)
			})
		})
	}
}
