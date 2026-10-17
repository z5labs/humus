// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/reflection"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts,
// with certificates from a CA generated for each test, and clients from
// grpcservertest.NewClient. Whole Apps run with humustest.Start, and a 5s
// drain. They use grpc-go's interop/grpc_testing service, as testpb.

// testServer answers EmptyCall at once. Its UnaryCall closes entered, if
// it is set, and then waits for release, if it is set, or for its context.
// It answers with the username "done" when released, and returns its
// context's error, recording when, when its context is done first.
type testServer struct {
	testpb.UnimplementedTestServiceServer

	entered   chan struct{}
	release   chan struct{}
	cancelled chan time.Time
}

func (*testServer) EmptyCall(context.Context, *testpb.Empty) (*testpb.Empty, error) {
	return &testpb.Empty{}, nil
}

func (ts *testServer) UnaryCall(ctx context.Context, _ *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	if ts.entered != nil {
		close(ts.entered)
	}
	select {
	case <-ts.release:
		return &testpb.SimpleResponse{Username: "done"}, nil
	case <-ctx.Done():
		if ts.cancelled != nil {
			ts.cancelled <- time.Now()
		}
		return nil, ctx.Err()
	}
}

// settingsFor returns Settings on addr, over TLS with a certificate for
// carts from ca.
func settingsFor(t *testing.T, ca *humustest.CA, addr string) grpcserver.Settings {
	t.Helper()
	return grpcserver.Settings{
		Addr: addr,
		TLS:  &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "carts").TLS()}},
	}
}

// serves returns an Init whose only Service serves ts on settings, built
// with opts.
func serves(settings grpcserver.Settings, ts testpb.TestServiceServer, opts ...grpc.ServerOption) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, settings, opts...)
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, ts)
		return []humus.Service{svc}, nil
	}
}

// onHost returns the App options of a test App on h: no telemetry, and the
// 5s drain a gRPC service runs with.
func onHost(h *humustest.Host) humustest.Option {
	return humustest.WithApp(
		humus.WithHost(h.Host()),
		humus.WithTelemetry(telemetry.NoopBuilder),
		humus.WithDrain(humus.DrainDelay(5*time.Second)),
	)
}

// newSetup returns a Setup on h, for a test that calls a Service's methods
// itself.
func newSetup(h *humustest.Host) *humus.Setup {
	return &humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}
}

// newService returns a Service on ":9090" of h, with a certificate for
// carts from ca, serving ts. It is shut down when the test ends.
func newService(t *testing.T, h *humustest.Host, ca *humustest.CA, ts testpb.TestServiceServer) *grpcserver.Service {
	t.Helper()
	svc, err := grpcserver.NewService(newSetup(h), settingsFor(t, ca, ":9090"))
	require.NoError(t, err)
	testpb.RegisterTestServiceServer(svc, ts)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })
	return svc
}

// requireRefused fails t unless a Dial to address is refused.
func requireRefused(t *testing.T, h *humustest.Host, address string) {
	t.Helper()
	conn, err := h.Dial(t.Context(), "tcp", address)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("Dial(%q) connected, want ECONNREFUSED", address)
	}
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
}

// call starts a UnaryCall with client in a new goroutine, and returns the
// channel its result arrives on: the username, or the error.
func call(t *testing.T, client testpb.TestServiceClient) <-chan string {
	got := make(chan string, 1)
	go func() {
		resp, err := client.UnaryCall(t.Context(), &testpb.SimpleRequest{FillUsername: true})
		if err != nil {
			got <- err.Error()
			return
		}
		got <- resp.GetUsername()
	}()
	return got
}

// panicked calls f and returns what it panicked with, or nil.
func panicked(f func()) (v any) {
	defer func() { v = recover() }()
	f()
	return nil
}

func TestService_meets_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		cert := humustest.NewCA(t).Issue(t, "carts")
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: ":9090",
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		})
		require.NoError(t, err)
		testpb.RegisterTestServiceServer(svc, &testServer{})
		return svc
	})
}

func TestService_the_grace_period_cuts_off_an_RPC_that_never_returns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		// Never released: the handler returns only when its context is done.
		ts := &testServer{entered: make(chan struct{}), cancelled: make(chan time.Time, 1)}

		p, exit := humustest.Start(t, serves(settingsFor(t, ca, ":9090"), ts), onHost(h),
			humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"}),
		)
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		got := call(t, client)
		<-ts.entered

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)

		require.Equal(t, 7*time.Second, (<-ts.cancelled).Sub(sigterm), "the drain's 5s, then the grace period's 2s")
		require.NotEqual(t, "done", <-got, "the RPC fails")

		st := <-exit
		require.Equal(t, humus.ExitFailure, st.Code())
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
		require.Equal(t, 2*time.Second, gerr.GracePeriod)
		require.ErrorIs(t, st.Cause(), context.DeadlineExceeded)
	})
}

func TestService_serves_an_RPC_during_the_drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, exit := humustest.Start(t, serves(settingsFor(t, ca, ":9090"), &testServer{}), onHost(h))
		<-h.Listening("carts:9090")

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)

		time.Sleep(time.Second)
		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		_, err := client.EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)

		st := <-exit
		require.Equal(t, 5*time.Second, time.Since(sigterm), "the App exits after the drain")
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func TestService_TLS_cannot_be_turned_off(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, exit := humustest.Start(t,
			serves(settingsFor(t, ca, ":9090"), &testServer{}, grpc.Creds(insecure.NewCredentials())),
			onHost(h),
		)
		<-h.Listening("carts:9090")

		secure := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		_, err := secure.EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err, "a client over TLS")

		plain := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", nil,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		))
		_, err = plain.EmptyCall(t.Context(), &testpb.Empty{})
		require.Error(t, err, "a plaintext client")
		require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-exit).Code())
	})
}

func TestNewService_settings(t *testing.T) {
	t.Run("the zero Settings fails, naming Addr and TLS in one error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			svc, err := grpcserver.NewService(newSetup(h), grpcserver.Settings{})
			require.Nil(t, svc)
			require.ErrorContains(t, err, `Settings.Addr "" is not host:port`)
			require.ErrorContains(t, err, "Settings.TLS must not be nil")
		})
	})

	t.Run("a bad Addr fails Init, and the App exits 1", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)

			_, exit := humustest.Start(t, serves(settingsFor(t, ca, "nope"), &testServer{}), onHost(h))

			st := <-exit
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorContains(t, st.Cause(), `init: grpcserver: NewService: Settings.Addr "nope" is not host:port`)
			require.NotContains(t, st.Cause().Error(), "Settings.TLS")
		})
	})

	t.Run("a TLS config with no certificate is named", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			_, err := grpcserver.NewService(newSetup(h), grpcserver.Settings{Addr: ":9090", TLS: &tls.Config{}})
			require.ErrorContains(t, err, "Settings.TLS has no certificate")
			require.NotContains(t, err.Error(), "Settings.Addr")
			requireRefused(t, h, "carts:9090")
		})
	})
}

func TestService_RegisterService_panics_instead_of_exiting(t *testing.T) {
	const name = `"grpc.testing.TestService"`

	t.Run("after Serve", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc, err := grpcserver.NewService(newSetup(h), settingsFor(t, humustest.NewCA(t), ":9090"))
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()
			synctest.Wait()

			v := panicked(func() { testpb.RegisterTestServiceServer(svc, &testServer{}) })
			require.NotNil(t, v)
			require.ErrorContains(t, v.(error), name)
			require.ErrorContains(t, v.(error), "after Serve")
			require.Empty(t, svc.GetServiceInfo())

			require.NoError(t, svc.Shutdown(t.Context()))
			require.NoError(t, <-served)
		})
	})

	t.Run("twice for one service", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc := newService(t, h, humustest.NewCA(t), &testServer{})

			v := panicked(func() { testpb.RegisterTestServiceServer(svc, &testServer{}) })
			require.NotNil(t, v)
			require.ErrorContains(t, v.(error), name)
			require.ErrorContains(t, v.(error), "already registered")
		})
	})

	t.Run("with an implementation of the wrong type", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc, err := grpcserver.NewService(newSetup(h), settingsFor(t, humustest.NewCA(t), ":9090"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })

			v := panicked(func() { svc.RegisterService(&testpb.TestService_ServiceDesc, struct{}{}) })
			require.NotNil(t, v)
			require.ErrorContains(t, v.(error), name)
			require.ErrorContains(t, v.(error), "does not implement grpc_testing.TestServiceServer")
			require.Empty(t, svc.GetServiceInfo())
		})
	})
}

func TestNewService_the_listener(t *testing.T) {
	t.Run("a taken address fails Init", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca, ":9090")
			init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				first, err := grpcserver.NewService(s, settings)
				if err != nil {
					return nil, err
				}
				second, err := grpcserver.NewService(s, settings)
				if err != nil {
					return nil, err
				}
				return []humus.Service{first, second}, nil
			}
			_, exit := humustest.Start(t, init, onHost(h))

			st := <-exit
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorIs(t, st.Cause(), syscall.EADDRINUSE)
			requireRefused(t, h, "carts:9090")
		})
	})

	t.Run("an Init that fails after NewService closes it, and a second App listens on its address", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca, ":9090")
			errInit := errors.New("the database is unreachable")
			failing := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				if _, err := grpcserver.NewService(s, settings); err != nil {
					return nil, err
				}
				return nil, errInit
			}
			_, exit := humustest.Start(t, failing, onHost(h))

			st := <-exit
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorIs(t, st.Cause(), errInit)
			requireRefused(t, h, "carts:9090")

			p, exit := humustest.Start(t, serves(settings, &testServer{}), onHost(h))
			<-h.Listening("carts:9090")
			client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
			_, err := client.EmptyCall(t.Context(), &testpb.Empty{})
			require.NoError(t, err)

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-exit).Code())
		})
	})
}

func TestService_Addr_reports_the_port_the_Host_picked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		settings := settingsFor(t, ca, ":0")
		addrs := make(chan net.Addr, 1)
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := grpcserver.NewService(s, settings)
			if err != nil {
				return nil, err
			}
			testpb.RegisterTestServiceServer(svc, &testServer{})
			addrs <- svc.Addr()
			return []humus.Service{svc}, nil
		}
		p, exit := humustest.Start(t, init, onHost(h))

		_, port, err := net.SplitHostPort((<-addrs).String())
		require.NoError(t, err)
		require.NotEqual(t, "0", port)

		address := "carts:" + port
		<-h.Listening(address)
		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, address, ca.Pool()))
		_, err = client.EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-exit).Code())
	})
}

func TestService_works_with_reflection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		settings := settingsFor(t, ca, ":9090")
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := grpcserver.NewService(s, settings)
			if err != nil {
				return nil, err
			}
			testpb.RegisterTestServiceServer(svc, &testServer{})
			reflection.Register(svc)
			return []humus.Service{svc}, nil
		}
		p, exit := humustest.Start(t, init, onHost(h))
		<-h.Listening("carts:9090")

		conn := grpcservertest.NewClient(t, h, "carts:9090", ca.Pool())
		stream, err := rpb.NewServerReflectionClient(conn).ServerReflectionInfo(t.Context())
		require.NoError(t, err)
		require.NoError(t, stream.Send(&rpb.ServerReflectionRequest{
			MessageRequest: &rpb.ServerReflectionRequest_ListServices{},
		}))
		resp, err := stream.Recv()
		require.NoError(t, err)
		var names []string
		for _, svc := range resp.GetListServicesResponse().GetService() {
			names = append(names, svc.GetName())
		}
		require.Contains(t, names, "grpc.testing.TestService")

		// The stream ends before shutdown, so GracefulStop has nothing to
		// wait for.
		require.NoError(t, stream.CloseSend())
		_, err = stream.Recv()
		require.ErrorIs(t, err, io.EOF)

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-exit).Code())
	})
}

func TestService_registers_no_reflection_by_default(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		svc := newService(t, h, humustest.NewCA(t), &testServer{})

		require.Equal(t, []string{"grpc.testing.TestService"}, serviceNames(svc))
	})
}

// serviceNames returns the names of the services registered on svc.
func serviceNames(svc *grpcserver.Service) []string {
	var names []string
	for name := range svc.GetServiceInfo() {
		names = append(names, name)
	}
	return names
}

func TestService_Shutdown(t *testing.T) {
	t.Run("before Serve, Serve then returns nil at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc := newService(t, h, humustest.NewCA(t), &testServer{})

			require.NoError(t, svc.Shutdown(t.Context()))
			start := time.Now()
			require.NoError(t, svc.Serve(t.Context()))
			require.Zero(t, time.Since(start))
			requireRefused(t, h, "carts:9090")
		})
	})

	t.Run("with a done context, closes every connection", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			ts := &testServer{entered: make(chan struct{}), cancelled: make(chan time.Time, 1)}
			svc := newService(t, h, ca, ts)
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()

			client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
			got := call(t, client)
			<-ts.entered

			done, cancel := context.WithCancel(t.Context())
			cancel()
			start := time.Now()
			err := svc.Shutdown(done)
			require.ErrorIs(t, err, context.Canceled)
			require.Zero(t, time.Since(start))

			select {
			case <-ts.cancelled:
			default:
				t.Fatal("Shutdown returned before the handler did")
			}
			require.NotEqual(t, "done", <-got, "the RPC fails")
			require.NoError(t, <-served)
			requireRefused(t, h, "carts:9090")
		})
	})

	t.Run("when its context is done during the graceful stop, stops at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			ts := &testServer{entered: make(chan struct{}), cancelled: make(chan time.Time, 1)}
			svc := newService(t, h, ca, ts)
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()

			client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
			got := call(t, client)
			<-ts.entered

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			start := time.Now()
			err := svc.Shutdown(ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Equal(t, 3*time.Second, time.Since(start))
			require.Equal(t, start.Add(3*time.Second), <-ts.cancelled)

			require.NotEqual(t, "done", <-got, "the RPC fails")
			require.NoError(t, <-served)
		})
	})

	t.Run("a second call returns nil", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc := newService(t, h, humustest.NewCA(t), &testServer{})
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()
			synctest.Wait()

			done, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(t, svc.Shutdown(done), context.Canceled)
			require.NoError(t, svc.Shutdown(done))
			require.NoError(t, <-served)
		})
	})
}
