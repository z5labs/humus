// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
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
//
// The rest run a dependency's rollout, and its MaxConnectionAgeGrace, under
// a Client with streams open, as #1176 reported them. The dependency's pods
// run in Apps of their own on the test Host, on grpcserver's defaults, as
// grpcservertest.Start runs them, and the end user's App calls them through
// a Client on DefaultSettings. The end user's job keeps subscriptions open,
// streams on which the dependency sends a message a second, and subscribes
// again whenever one ends with Unavailable, as the gRPC services page's
// Streams at shutdown tells a client to.

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

// pod is one pod of the dependency. Its StreamingOutputCall and
// FullDuplexCall send a message a second, naming the pod, until their
// context ends, as a subscription does. Its EmptyCall answers OK.
type pod struct {
	testpb.UnimplementedTestServiceServer

	name string
}

func (p *pod) EmptyCall(context.Context, *testpb.Empty) (*testpb.Empty, error) {
	return &testpb.Empty{}, nil
}

func (p *pod) StreamingOutputCall(_ *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	return p.publish(stream.Context(), stream.Send)
}

func (p *pod) FullDuplexCall(stream grpc.BidiStreamingServer[testpb.StreamingOutputCallRequest, testpb.StreamingOutputCallResponse]) error {
	return p.publish(stream.Context(), stream.Send)
}

// publish sends a message a second until ctx ends, and then returns ctx's
// error, as a handler that ends on its context does.
func (p *pod) publish(ctx context.Context, send func(*testpb.StreamingOutputCallResponse) error) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := send(&testpb.StreamingOutputCallResponse{Payload: &testpb.Payload{Body: []byte(p.name)}}); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// startPod runs the dependency's pod name at name:9090 on h, in an App of
// its own on grpcserver's defaults, with a certificate for ledger from ca,
// passing opts to grpcserver.NewService. It returns a function that sends
// the pod SIGTERM, and returns its exit status once it has exited.
func startPod(t *testing.T, h *humustest.Host, ca *humustest.CA, name string, opts ...grpc.ServerOption) (stop func() bedrock.ExitStatus) {
	t.Helper()
	addr := name + ":9090"
	cert := ca.Issue(t, "ledger")
	p, exit := grpcservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := grpcserver.Settings{
			Addr: addr,
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		}
		svc, err := grpcserver.NewService(s, settings, opts...)
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, &pod{name: name})
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening(addr)
	return func() bedrock.ExitStatus {
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		return <-exit
	}
}

// service is the dependency's Kubernetes ClusterIP Service. A Dial of
// ledger:9090, through the Host that host returns, reaches the pod in its
// endpoints at the time, as kube-proxy picks a pod for each connection.
type service struct {
	mu  sync.Mutex
	pod string
}

// route puts pod alone in the Service's endpoints.
func (s *service) route(pod string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pod = pod
}

func (s *service) host(h *humustest.Host) humus.Host {
	host := h.Host()
	dial := host.Dial
	host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "ledger:9090" {
			s.mu.Lock()
			address = s.pod + ":9090"
			s.mu.Unlock()
		}
		return dial(ctx, network, address)
	}
	return host
}

// opener opens one stream of a subscription, and returns its Recv.
type opener func(ctx context.Context, c testpb.TestServiceClient) (func() (*testpb.StreamingOutputCallResponse, error), error)

func serverStream(ctx context.Context, c testpb.TestServiceClient) (func() (*testpb.StreamingOutputCallResponse, error), error) {
	stream, err := c.StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{})
	if err != nil {
		return nil, err
	}
	return stream.Recv, nil
}

func bidiStream(ctx context.Context, c testpb.TestServiceClient) (func() (*testpb.StreamingOutputCallResponse, error), error) {
	stream, err := c.FullDuplexCall(ctx)
	if err != nil {
		return nil, err
	}
	return stream.Recv, nil
}

// subscriptions are the end user's: streams opened with open, each opened
// again whenever it ends with Unavailable. A subscription whose open fails
// tries again after reopen, or gives up if reopen is 0. They record what
// happens to each, with the time since start.
type subscriptions struct {
	open   opener
	reopen time.Duration
	start  time.Time

	mu     sync.Mutex
	from   []string // the pod each subscription last heard from
	ended  []int    // how many times each subscription's stream has ended
	heard  []bool   // whether each subscription has heard a message since its stream last ended
	failed []error  // every open that failed
	events []string
}

func newSubscriptions(n int, open opener) *subscriptions {
	return &subscriptions{
		open:  open,
		start: time.Now(),
		from:  make([]string, n),
		ended: make([]int, n),
		heard: make([]bool, n),
	}
}

// event records what happened, with the time since start. Its caller holds
// s.mu, or is the test's goroutine.
func (s *subscriptions) event(format string, args ...any) {
	s.events = append(s.events, fmt.Sprintf("+%07.3fs ", time.Since(s.start).Seconds())+fmt.Sprintf(format, args...))
}

func (s *subscriptions) note(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.event(format, args...)
}

// run runs every subscription through c until ctx ends, and waits for
// each to return.
func (s *subscriptions) run(ctx context.Context, c testpb.TestServiceClient) {
	var wg sync.WaitGroup
	for i := range s.from {
		wg.Go(func() { s.subscribe(ctx, c, i) })
	}
	wg.Wait()
}

func (s *subscriptions) subscribe(ctx context.Context, c testpb.TestServiceClient, i int) {
	for {
		recv, err := s.open(ctx, c)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			s.failed = append(s.failed, err)
			s.event("subscription %d failed to open: %v", i, err)
			s.mu.Unlock()
			if s.reopen == 0 {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.reopen):
			}
			continue
		}
		for {
			resp, err := recv()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				s.mu.Lock()
				s.ended[i]++
				s.heard[i] = false
				s.event("subscription %d, on %s, ended: %v", i, s.from[i], err)
				s.mu.Unlock()
				if status.Code(err) != codes.Unavailable {
					return
				}
				break
			}
			s.mu.Lock()
			if !s.heard[i] {
				s.event("subscription %d heard from %s", i, resp.GetPayload().GetBody())
			}
			s.from[i] = string(resp.GetPayload().GetBody())
			s.heard[i] = true
			s.mu.Unlock()
		}
	}
}

// allOn reports whether every subscription has heard from pod since its
// stream last ended.
func (s *subscriptions) allOn(pod string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.from {
		if s.from[i] != pod || !s.heard[i] {
			return false
		}
	}
	return true
}

// allEnded reports whether every subscription's stream has ended once.
func (s *subscriptions) allEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.ended {
		if n == 0 {
			return false
		}
	}
	return true
}

// allResubscribed reports whether every subscription's stream has ended,
// and every subscription has heard a message since.
func (s *subscriptions) allResubscribed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.from {
		if s.ended[i] == 0 || !s.heard[i] {
			return false
		}
	}
	return true
}

func (s *subscriptions) failedOpens() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.failed...)
}

// log logs every event, and the Client's breaker records in tel, with
// their times since start.
func (s *subscriptions) log(t *testing.T, tel *humustest.Telemetry) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		t.Log(e)
	}
	for _, r := range breakerRecords(tel) {
		a := recordAttrs(r)
		t.Logf("+%07.3fs breaker record %q: previous_state=%v consecutive_failures=%v failures=%v successes=%v",
			r.Timestamp().Sub(s.start).Seconds(), r.Body().AsString(), a["humus.circuit.previous_state"],
			a["humus.circuit.consecutive_failures"], a["humus.circuit.failures"], a["humus.circuit.successes"])
	}
}

// subscriber runs the end user's App on host: a Client of ledger:9090 on
// DefaultSettings, trusting ca, with tel's telemetry, and a job that runs
// subs through it until done is closed. It returns the Client, once Init
// has built it, and the App's exit status.
func subscriber(t *testing.T, host humus.Host, tel *humustest.Telemetry, ca *humustest.CA, subs *subscriptions, done <-chan struct{}) (*grpcclient.Client, <-chan bedrock.ExitStatus) {
	t.Helper()
	built := make(chan *grpcclient.Client, 1)
	_, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		ledger, err := grpcclient.NewClient(s, settingsFor(ca, "ledger:9090"))
		if err != nil {
			return nil, err
		}
		built <- ledger
		return []humus.Service{job.NewService(s, func(ctx context.Context) error {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			ran := make(chan struct{})
			go func() {
				defer close(ran)
				subs.run(ctx, testpb.NewTestServiceClient(ledger))
			}()
			select {
			case <-done:
			case <-ctx.Done():
			}
			cancel()
			<-ran
			return nil
		})}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(tel.Builder())))
	return <-built, exit
}

// within polls cond every 100ms until it holds or d has passed, and
// reports whether it held.
func within(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

func TestClient_a_rollout_of_the_dependency_leaves_the_breaker_closed(t *testing.T) {
	testCases := []struct {
		name    string
		streams int
		open    opener
		// pods are the pods the subscriptions move through, in order. The
		// rollout replaces every one of them but the last.
		pods []string
	}{
		{name: "4 server streams on the pod it replaces", streams: 4, open: serverStream, pods: []string{"ledger-a", "ledger-b"}},
		{name: "5 server streams on the pod it replaces", streams: 5, open: serverStream, pods: []string{"ledger-a", "ledger-b"}},
		{name: "5 bidirectional streams on the pod it replaces", streams: 5, open: bidiStream, pods: []string{"ledger-a", "ledger-b"}},
		{name: "3 server streams on two pods it replaces in turn", streams: 3, open: serverStream, pods: []string{"ledger-a", "ledger-b", "ledger-c"}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				tel := humustest.NewTelemetry()
				svc := &service{}
				svc.route(tc.pods[0])
				var stops []func() bedrock.ExitStatus
				for _, name := range tc.pods {
					stops = append(stops, startPod(t, h, ca, name))
				}

				subs := newSubscriptions(tc.streams, tc.open)
				done := make(chan struct{})
				ledger, exit := subscriber(t, svc.host(h), tel, ca, subs, done)
				opened := within(10*time.Second, func() bool { return subs.allOn(tc.pods[0]) })

				// The rollout: each pod but the last leaves the endpoints and
				// is sent SIGTERM, once the subscriptions are on it.
				var podExits []int
				moved := true
				last := len(tc.pods) - 1
				for i := range last {
					next := tc.pods[i+1]
					svc.route(next)
					subs.note("%s leaves the endpoints, and is sent SIGTERM", tc.pods[i])
					st := stops[i]()
					podExits = append(podExits, st.Code())
					subs.note("%s exited %d", tc.pods[i], st.Code())
					if i+1 < last {
						moved = moved && within(30*time.Second, func() bool { return subs.allOn(next) })
					}
				}

				time.Sleep(time.Second)
				_, unary := testpb.NewTestServiceClient(ledger).EmptyCall(context.Background(), &testpb.Empty{})
				subs.note("EmptyCall: %v", unary)
				resubscribed := within(30*time.Second, func() bool { return subs.allOn(tc.pods[last]) })

				close(done)
				st := <-exit
				podExits = append(podExits, stops[last]().Code())
				subs.log(t, tel)

				require.True(t, opened, "every subscription opened on %s", tc.pods[0])
				require.True(t, moved, "every subscription moved through the pods")
				require.Zero(t, st.Code(), "cause: %v", st.Cause())
				require.Equal(t, make([]int, len(tc.pods)), podExits, "every pod exited 0")

				require.NotContains(t, bodies(breakerRecords(tel)), breakerOpen, "the breaker stays closed: the rollout ended the streams, and every other pod is healthy")
				require.NoError(t, unary, "a call a second after the pod exited reaches the next pod")
				require.Empty(t, subs.failedOpens(), "every subscription opens again")
				require.True(t, resubscribed, "every subscription is on %s", tc.pods[last])
			})
		})
	}
}

func TestClient_a_rollout_of_the_dependency_leaves_unary_calls_answered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		svc := &service{}
		svc.route("ledger-a")
		stopA := startPod(t, h, ca, "ledger-a")
		stopB := startPod(t, h, ca, "ledger-b")

		// The end user's subscriptions try again a second after an open
		// fails.
		subs := newSubscriptions(5, serverStream)
		subs.reopen = time.Second
		done := make(chan struct{})
		ledger, exit := subscriber(t, svc.host(h), tel, ca, subs, done)
		opened := within(10*time.Second, func() bool { return subs.allOn("ledger-a") })

		svc.route("ledger-b")
		subs.note("ledger-a leaves the endpoints, and is sent SIGTERM")
		stA := stopA()
		subs.note("ledger-a exited %d", stA.Code())

		// The end user's App also calls the ledger, once a second, for a
		// minute after ledger-a exited.
		time.Sleep(500 * time.Millisecond)
		var unary []error
		answered := true
		for range 60 {
			_, err := testpb.NewTestServiceClient(ledger).EmptyCall(context.Background(), &testpb.Empty{})
			if err != nil || !answered {
				subs.note("EmptyCall: %v", err)
			}
			answered = err == nil
			if err != nil {
				unary = append(unary, err)
			}
			time.Sleep(time.Second)
		}
		resubscribed := within(30*time.Second, func() bool { return subs.allOn("ledger-b") })

		close(done)
		st := <-exit
		stB := stopB()
		subs.log(t, tel)

		require.True(t, opened, "every subscription opened on ledger-a")
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Zero(t, stA.Code())
		require.Zero(t, stB.Code())

		require.Zero(t, len(unary), "every call after the rollout reaches ledger-b; the calls rejected are logged above")
		require.True(t, resubscribed, "every subscription is on ledger-b")
	})
}

func TestClient_the_dependencys_MaxConnectionAgeGrace_leaves_the_breaker_closed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		svc := &service{}
		svc.route("ledger-a")
		stop := startPod(t, h, ca, "ledger-a",
			grpc.KeepaliveParams(keepalive.ServerParameters{
				MaxConnectionAge:      time.Minute,
				MaxConnectionAgeGrace: 10 * time.Second,
			}),
		)

		subs := newSubscriptions(5, serverStream)
		done := make(chan struct{})
		ledger, exit := subscriber(t, svc.host(h), tel, ca, subs, done)
		opened := within(10*time.Second, func() bool { return subs.allOn("ledger-a") })
		ended := within(3*time.Minute, subs.allEnded)

		time.Sleep(time.Second)
		_, unary := testpb.NewTestServiceClient(ledger).EmptyCall(context.Background(), &testpb.Empty{})
		subs.note("EmptyCall: %v", unary)
		resubscribed := within(30*time.Second, subs.allResubscribed)

		close(done)
		st := <-exit
		podExit := stop().Code()
		subs.log(t, tel)

		require.True(t, opened, "every subscription opened on ledger-a")
		require.True(t, ended, "MaxConnectionAgeGrace ended every stream")
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Zero(t, podExit)

		require.NotContains(t, bodies(breakerRecords(tel)), breakerOpen, "the breaker stays closed: the server's connection age ended the streams")
		require.NoError(t, unary, "a call a second after the streams ended reaches the dependency")
		require.Empty(t, subs.failedOpens(), "every subscription opens again")
		require.True(t, resubscribed, "every subscription is open again")
	})
}
