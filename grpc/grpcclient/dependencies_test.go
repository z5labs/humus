// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcgohealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file are the samples of the docs site's Calling gRPC
// services page that are not already in the package's other tests: an App
// with two Clients of one address, a dependency whose server sets
// MaxConnectionAge, an Init that puts Connected in its readiness, and a
// monitor of a dependency's grpc.health.v1.

// startServing runs a dependency on h, in an App of its own, with a
// certificate for name from ca, serving b at addr, and passing opts to
// grpcserver.NewService. If hs is not nil, it registers grpc-go's health
// server too. It returns a function that stops the dependency and requires
// it to exit 0.
func startServing(t *testing.T, h *humustest.Host, ca *humustest.CA, name, addr string, b *backend, hs *grpcgohealth.Server, opts ...grpc.ServerOption) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, name)
	p, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := grpcserver.Settings{
			Addr: addr,
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		}
		svc, err := grpcserver.NewService(s, settings, opts...)
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, b)
		if hs != nil {
			healthpb.RegisterHealthServer(svc, hs)
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening(addr)
	return func() {
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}

// healthyWithin checks m with a context that ends after probeTimeout, as a
// probe's own timeout would end it, and reports whether it was healthy.
func healthyWithin(m health.Monitor) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	ok, err := m.Healthy(ctx)
	return ok && err == nil
}

// gateway is the backend of the two APIs behind one address: EmptyCall,
// the payments API, is down, and UnaryCall, the ledger API, aborts the
// first write, which raced another, and answers OK after that.
func gateway(b *backend, ledgerCalls *atomic.Int32) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		method, _ := grpc.Method(ctx)
		if method == "/grpc.testing.TestService/EmptyCall" {
			return status.Error(codes.Unavailable, "payments is down")
		}
		if ledgerCalls.Add(1) == 1 {
			return status.Error(codes.Aborted, "another write to the account raced this one")
		}
		return nil
	}
}

func TestClient_two_Clients_of_one_address_have_a_breaker_each(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		var ledgerCalls atomic.Int32
		gw := &backend{}
		gw.answer = gateway(gw, &ledgerCalls)
		stop := startServing(t, h, ca, "gateway", "gateway:9090", gw, nil)
		defer stop()

		paymentsSettings := settingsFor(ca, "gateway:9090")
		ledgerSettings := paymentsSettings

		// The APIs at gateway:9090 share a convention: Aborted says
		// another write to the account raced this one, which is over by
		// the next attempt. One Classify says so, and is set on the
		// Client of each.
		classify := grpcclient.DefaultSettings().Classify
		ourAPIs := func(method string, err error, trailer metadata.MD) grpcclient.Classification {
			if status.Code(err) == codes.Aborted {
				return grpcclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
			}
			return classify(method, err, trailer)
		}
		paymentsSettings.Classify = ourAPIs
		ledgerSettings.Classify = ourAPIs

		var (
			rejected       error
			paymentsAfter  error
			ledgerResponse error
		)
		_, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			// Two Clients of one address, each with its own Settings, and
			// so its own breaker, built once in Init and shared by
			// everything that calls them.
			payments, err := grpcclient.NewClient(s, paymentsSettings)
			if err != nil {
				return nil, err
			}
			ledger, err := grpcclient.NewClient(s, ledgerSettings)
			if err != nil {
				return nil, err
			}
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				p := testpb.NewTestServiceClient(payments)
				for range 10 {
					if _, err := p.EmptyCall(ctx, &testpb.Empty{}); errors.Is(err, circuit.ErrRejected) {
						rejected = err
						break
					}
				}
				_, paymentsAfter = p.EmptyCall(ctx, &testpb.Empty{})
				_, ledgerResponse = testpb.NewTestServiceClient(ledger).UnaryCall(ctx, &testpb.SimpleRequest{})
				return nil
			})}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.ErrorIs(t, rejected, circuit.ErrRejected, "the payments Client's breaker opened")
		require.Equal(t, codes.Unavailable, status.Code(rejected))
		require.ErrorIs(t, paymentsAfter, circuit.ErrRejected, "and stays open")
		require.NoError(t, ledgerResponse, "the ledger Client's breaker is its own, and the Aborted was retried")
		require.EqualValues(t, 2, ledgerCalls.Load(), "the Aborted, then the OK")
	})
}

func TestClient_moves_when_the_servers_MaxConnectionAge_passes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		b := &backend{}
		stop := startServing(t, h, ca, "carts", "carts:9090", b, nil,
			// Every connection is closed with a GOAWAY once it is about a
			// minute old, so its clients dial again, through the
			// Kubernetes Service, and reach whichever pod it picks.
			grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: time.Minute}),
		)
		defer stop()

		counter := &rpcCounter{}
		var errs []error
		_, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			carts, err := grpcclient.NewClient(s, settingsFor(ca, "carts:9090"), grpc.WithStatsHandler(counter))
			if err != nil {
				return nil, err
			}
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				c := testpb.NewTestServiceClient(carts)
				for range 3 {
					_, err := c.EmptyCall(ctx, &testpb.Empty{})
					errs = append(errs, err)
					time.Sleep(2 * time.Minute)
				}
				return nil
			})}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.Equal(t, []error{nil, nil, nil}, errs)
		require.EqualValues(t, 3, b.calls.Load())
		require.EqualValues(t, 3, counter.connsBegun.Load(), "a connection for each call, each older than MaxConnectionAge by the next")
	})
}

func TestInit_turns_ready_once_its_Client_has_connected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		cartsSettings := settingsFor(ca, "carts:9090")
		serverSettings := grpcserver.Settings{Addr: ":8443", TLS: serving(t, ca, "orders")}
		probeSettings := grpchealth.Settings{Addr: ":8081"}

		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			carts, err := grpcclient.NewClient(s, cartsSettings)
			if err != nil {
				return nil, err
			}
			svc, err := grpcserver.NewService(s, serverSettings)
			if err != nil {
				return nil, err
			}
			testpb.RegisterTestServiceServer(svc, &api{carts: testpb.NewTestServiceClient(carts)})

			// The service turns ready once its Client has connected to
			// carts, and stays ready through an outage of carts after
			// that.
			probes, err := grpchealth.NewService(s, probeSettings, health.Readiness(carts.Connected()))
			if err != nil {
				return nil, err
			}
			return []humus.Service{svc, probes}, nil
		}
		p, exit := grpcservertest.Start(t, init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening("orders:8081")
		synctest.Wait()

		probe := healthpb.NewHealthClient(grpcservertest.NewClient(t, h, "orders:8081", nil,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		))
		ready := func() bool {
			ctx, cancel := context.WithTimeout(t.Context(), probeTimeout)
			defer cancel()
			// A check that waits out the probe's timeout fails, as a
			// kubelet's probe does.
			resp, err := probe.Check(ctx, &healthpb.HealthCheckRequest{Service: "readiness"})
			return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
		}

		require.False(t, ready(), "unready while carts isn't there")

		// stopCarts is safe to call twice, so it is deferred, and called
		// again for the outage.
		stopCarts := sync.OnceFunc(startCarts(t, h, ca))
		defer stopCarts()
		ok := false
		// A probe a second, as a kubelet's are a period apart, while
		// grpc-go's connection backoff passes.
		for range 120 {
			if ok = ready(); ok {
				break
			}
			time.Sleep(time.Second)
		}
		require.True(t, ok, "ready once carts serves")

		stopCarts()
		require.True(t, ready(), "still ready in an outage of carts: Connected latches")

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func TestClient_a_monitor_of_the_dependencys_grpc_health_v1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		hs := grpcgohealth.NewServer()
		hs.SetServingStatus("grpc.testing.TestService", healthpb.HealthCheckResponse_SERVING)
		// stopCarts is safe to call twice, so it is deferred, and called
		// again for the outage.
		stopCarts := sync.OnceFunc(startServing(t, h, ca, "carts", "carts:9090", &backend{}, hs))
		defer stopCarts()

		checked, stopped := make(chan struct{}), make(chan struct{})
		var before, after struct{ monitor, connected bool }
		_, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			carts, err := grpcclient.NewClient(s, settingsFor(ca, "carts:9090"))
			if err != nil {
				return nil, err
			}
			monitor := health.MonitorFunc(func(ctx context.Context) (bool, error) {
				resp, err := healthpb.NewHealthClient(carts).Check(ctx, &healthpb.HealthCheckRequest{Service: "grpc.testing.TestService"})
				return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING, nil
			})
			return []humus.Service{job.NewService(s, func(context.Context) error {
				before.monitor, before.connected = healthyWithin(monitor), healthyWithin(carts.Connected())
				close(checked)
				<-stopped
				after.monitor, after.connected = healthyWithin(monitor), healthyWithin(carts.Connected())
				return nil
			})}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))

		<-checked
		stopCarts()
		close(stopped)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.True(t, before.monitor, "healthy while carts serves")
		require.True(t, before.connected)
		require.False(t, after.monitor, "the monitor turns unhealthy in an outage of carts")
		require.True(t, after.connected, "Connected latches")
	})
}
