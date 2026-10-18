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
	"net/http"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/humustest"
)

// The tests in this file run whole Apps inside synctest bubbles, on
// humustest Hosts, over TLS from humustest.NewCA. The test is the edge: it
// makes RPCs whose baggage, in their metadata, carries a scenario, written
// with jsonscenario, and a tenant member.

// scenarioKey is the baggage member the tests' codec reads.
const scenarioKey = "acme.fault"

// stripEvent is the span event of a strip.
const stripEvent = "humus.fit.strip"

// The RPCs the tests make, as their server spans are named.
const (
	unaryCall   = "grpc.testing.TestService/UnaryCall"
	streamCall  = "grpc.testing.TestService/StreamingOutputCall"
	healthCheck = "grpc.health.v1.Health/Check"
)

// codec returns the tests' jsonscenario Codec.
func codec(t *testing.T) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	return c
}

// abortPayments is the scenario the edge sends: it aborts
// grpc:payments:9090.
var abortPayments = fit.Scenario{Rules: []fit.Rule{{Point: "grpc:payments:9090", Fault: fit.Fault{Abort: true}}}}

// sight is what one layer of the server saw of an RPC's context.
type sight struct {
	scenario bool
	rules    []fit.Rule
	tenant   string
}

// seen records, in order, each layer an RPC reached: "trust" for
// TrustScenario, "unary interceptor" or "stream interceptor" for the end
// user's interceptors, and "handler". It keeps the last sight of each.
type seen struct {
	c jsonscenario.Codec

	mu     sync.Mutex
	order  []string
	sights map[string]sight
}

func (s *seen) record(ctx context.Context, layer string) {
	sc, ok, err := fit.ScenarioFromContext(ctx, s.c)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, layer)
	if s.sights == nil {
		s.sights = make(map[string]sight)
	}
	s.sights[layer] = sight{
		scenario: ok && err == nil,
		rules:    sc.Rules,
		tenant:   baggage.FromContext(ctx).Member("tenant").Value(),
	}
}

// layers returns the layers the RPCs reached, in order.
func (s *seen) layers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// sightOf returns what layer saw, and whether it saw anything.
func (s *seen) sightOf(layer string) (sight, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sights[layer]
	return v, ok
}

// recordingServer records the context of its UnaryCall and of its
// StreamingOutputCall, which sends one message.
type recordingServer struct {
	testpb.UnimplementedTestServiceServer

	seen *seen
}

func (rs recordingServer) UnaryCall(ctx context.Context, _ *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	rs.seen.record(ctx, "handler")
	return &testpb.SimpleResponse{}, nil
}

func (rs recordingServer) StreamingOutputCall(_ *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	rs.seen.record(stream.Context(), "handler")
	return stream.Send(&testpb.StreamingOutputCallResponse{})
}

// faultsRun configures a run of an App serving one Service, and the one
// RPC the edge makes to it.
type faultsRun struct {
	// faults turns failure injection on, with the tests' codec.
	faults bool
	// trust is the Service's TrustScenario. Its calls are recorded as
	// "trust".
	trust func(ctx context.Context, method string) bool
	// clientCerts makes the Service ask for a client certificate, verified
	// against the CA, and the client present one.
	clientCerts bool
	// scenario makes the edge's RPC carry abortPayments.
	scenario bool
	// rpc is the RPC the edge makes: unaryCall, streamCall or healthCheck.
	rpc string
}

// faultsResult is what a run recorded.
type faultsResult struct {
	err   error
	seen  *seen
	spans []sdktrace.ReadOnlySpan
	panic panicRun
}

// runFaults runs an App configured by run, makes one RPC to it from the
// edge, then stops the App and returns what happened. The Service has the
// end user's unary and stream interceptors, which record what they see,
// and a grpchealth.NewServer registered on it.
func runFaults(t *testing.T, run faultsRun) faultsResult {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()
	c := codec(t)
	got := &seen{c: c}

	settings := settingsFor(t, ca, ":9090")
	if run.trust != nil {
		settings.TrustScenario = func(ctx context.Context, method string) bool {
			got.record(ctx, "trust")
			return run.trust(ctx, method)
		}
	}
	if run.clientCerts {
		settings.TLS.ClientAuth = tls.VerifyClientCertIfGiven
		settings.TLS.ClientCAs = ca.Pool()
	}
	unary := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		got.record(ctx, "unary interceptor")
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		got.record(ss.Context(), "stream interceptor")
		return handler(srv, ss)
	}
	init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, settings, grpc.ChainUnaryInterceptor(unary), grpc.ChainStreamInterceptor(stream))
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, recordingServer{seen: got})
		healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, health.Probes{Liveness: &health.Binary{}, Readiness: &health.Binary{}}))
		return []humus.Service{svc}, nil
	}

	opts := []humus.Option{humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())}
	if run.faults {
		opts = append(opts, humus.WithFaults(func(context.Context, *humus.Setup) (fault.Settings, error) {
			d := fault.DefaultSettings()
			d.Decoder = c
			return d, nil
		}))
	}
	p, exit := grpcservertest.Start(t, init, humustest.WithApp(opts...))
	<-h.Listening("carts:9090")

	var dial []grpc.DialOption
	if run.clientCerts {
		dial = append(dial, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs:      ca.Pool(),
			Certificates: []tls.Certificate{ca.Issue(t, "orders").TLS()},
		})))
	}
	conn := grpcservertest.NewClient(t, h, "carts:9090", ca.Pool(), dial...)
	ctx := edgeContext(t, c, run.scenario)
	var rpcErr error
	switch run.rpc {
	case unaryCall:
		_, rpcErr = testpb.NewTestServiceClient(conn).UnaryCall(ctx, &testpb.SimpleRequest{})
	case streamCall:
		rpcErr = drain(testpb.NewTestServiceClient(conn).StreamingOutputCall(ctx, &testpb.StreamingOutputCallRequest{}))
	case healthCheck:
		_, rpcErr = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	default:
		t.Fatalf("unknown RPC %q", run.rpc)
	}

	p.Signal(syscall.SIGTERM)
	st := <-exit
	require.Zero(t, st.Code(), "cause: %v", st.Cause())

	res := faultsResult{err: rpcErr, seen: got, spans: serverSpans(tel)}
	res.panic = panicRun{spans: res.spans, stderr: p.Stderr()}
	for _, r := range tel.Logs() {
		if r.Severity() == log.SeverityError {
			res.panic.errors = append(res.panic.errors, r)
		}
	}
	return res
}

// drain receives from stream until it ends, and returns nil if it ended
// cleanly.
func drain(stream grpc.ServerStreamingClient[testpb.StreamingOutputCallResponse], err error) error {
	if err != nil {
		return err
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// edgeContext returns a context whose outgoing metadata carries baggage
// with tenant=acme and, when scenario is set, abortPayments written with c.
func edgeContext(t *testing.T, c jsonscenario.Codec, scenario bool) context.Context {
	t.Helper()
	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(tenant)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), b)
	if scenario {
		ctx, err = fit.ContextWithScenario(ctx, c, abortPayments)
		require.NoError(t, err)
	}
	header := http.Header{}
	propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(header))
	return metadata.AppendToOutgoingContext(t.Context(), "baggage", header.Get("baggage"))
}

// eventsNamed returns the events of span named name.
func eventsNamed(span sdktrace.ReadOnlySpan, name string) []sdktrace.Event {
	var evs []sdktrace.Event
	for _, ev := range span.Events() {
		if ev.Name == name {
			evs = append(evs, ev)
		}
	}
	return evs
}

// requireServed fails t unless the RPC succeeded, its handler saw
// tenant=acme, and the App wrote nothing to stderr. It returns what the
// handler saw and the RPC's server span, named name.
func requireServed(t *testing.T, res faultsResult, name string) (sight, sdktrace.ReadOnlySpan) {
	t.Helper()
	require.NoError(t, res.err)
	require.Empty(t, res.panic.stderr)
	handler, ok := res.seen.sightOf("handler")
	require.True(t, ok, "the handler never ran")
	require.Equal(t, "acme", handler.tenant, "a baggage member other than the scenario was lost")
	require.Len(t, res.spans, 1)
	require.Equal(t, name, res.spans[0].Name())
	return handler, res.spans[0]
}

func TestService_strips_a_failure_scenario(t *testing.T) {
	t.Run("nil trusts no one", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, rpc: unaryCall})
			handler, span := requireServed(t, res, unaryCall)

			require.False(t, handler.scenario, "the handler saw the scenario of a caller nothing trusts")
			interceptor, ok := res.seen.sightOf("unary interceptor")
			require.True(t, ok)
			require.False(t, interceptor.scenario, "the end user's interceptor saw the scenario of a caller nothing trusts")
			require.Equal(t, "acme", interceptor.tenant)
			strips := eventsNamed(span, stripEvent)
			require.Len(t, strips, 1)
			require.Empty(t, strips[0].Attributes)
		})
	})

	t.Run("a caller TrustScenario doesn't trust", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var method string
			res := runFaults(t, faultsRun{faults: true, scenario: true, rpc: unaryCall, trust: func(_ context.Context, m string) bool {
				method = m
				return false
			}})
			handler, span := requireServed(t, res, unaryCall)

			require.Equal(t, "/"+unaryCall, method, "TrustScenario wasn't given the RPC's full method")
			require.False(t, handler.scenario, "the handler saw the scenario of a caller TrustScenario refused")
			require.Len(t, eventsNamed(span, stripEvent), 1)
		})
	})

	t.Run("streams: the handler and the end user's stream interceptors see the stripped context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, rpc: streamCall})
			handler, span := requireServed(t, res, streamCall)

			require.False(t, handler.scenario, "the stream's handler saw the scenario of a caller nothing trusts")
			interceptor, ok := res.seen.sightOf("stream interceptor")
			require.True(t, ok)
			require.False(t, interceptor.scenario, "the end user's stream interceptor saw the scenario of a caller nothing trusts")
			require.Equal(t, "acme", interceptor.tenant)
			strips := eventsNamed(span, stripEvent)
			require.Len(t, strips, 1)
			require.Empty(t, strips[0].Attributes)
		})
	})

	t.Run("a trusted caller keeps its scenario", func(t *testing.T) {
		for _, rpc := range []string{unaryCall, streamCall} {
			t.Run(rpc, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					res := runFaults(t, faultsRun{faults: true, scenario: true, rpc: rpc, trust: func(context.Context, string) bool { return true }})
					handler, span := requireServed(t, res, rpc)

					require.True(t, handler.scenario, "the handler didn't see the scenario of a trusted caller")
					require.Equal(t, abortPayments.Rules, handler.rules)
					require.Empty(t, eventsNamed(span, stripEvent))
				})
			})
		}
	})

	t.Run("a caller with a client certificate the CA verified", func(t *testing.T) {
		verified := func(ctx context.Context, _ string) bool {
			p, ok := peer.FromContext(ctx)
			if !ok {
				return false
			}
			info, ok := p.AuthInfo.(credentials.TLSInfo)
			return ok && len(info.State.VerifiedChains) > 0
		}

		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, clientCerts: true, rpc: unaryCall, trust: verified})
			handler, span := requireServed(t, res, unaryCall)
			require.True(t, handler.scenario, "the handler didn't see the scenario of a verified caller")
			require.Empty(t, eventsNamed(span, stripEvent))
		})

		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, rpc: unaryCall, trust: verified})
			handler, span := requireServed(t, res, unaryCall)
			require.False(t, handler.scenario, "the handler saw the scenario of a caller with no certificate")
			require.Len(t, eventsNamed(span, stripEvent), 1)
		})
	})

	t.Run("a grpchealth server registered on the Service is behind the strip", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var method string
			res := runFaults(t, faultsRun{faults: true, scenario: true, rpc: healthCheck, trust: func(_ context.Context, m string) bool {
				method = m
				return false
			}})
			require.NoError(t, res.err)
			require.Empty(t, res.panic.stderr)
			require.Equal(t, "/"+healthCheck, method)
			require.Len(t, res.spans, 1)
			require.Equal(t, healthCheck, res.spans[0].Name())
			require.Len(t, eventsNamed(res.spans[0], stripEvent), 1)
		})
	})

	t.Run("off: with no humus.WithFaults, the scenario reaches the handler", func(t *testing.T) {
		for _, rpc := range []string{unaryCall, streamCall} {
			t.Run(rpc, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					res := runFaults(t, faultsRun{scenario: true, rpc: rpc})
					handler, span := requireServed(t, res, rpc)

					require.True(t, handler.scenario, "the handler didn't see the scenario, with failure injection off")
					require.Equal(t, abortPayments.Rules, handler.rules)
					require.Empty(t, eventsNamed(span, stripEvent))
				})
			})
		}
	})
}

func TestService_asks_TrustScenario_only_when_it_must(t *testing.T) {
	for _, tc := range []struct {
		name     string
		faults   bool
		scenario bool
		rpc      string
		layers   []string
	}{
		{"a unary RPC with a scenario, with failure injection", true, true, unaryCall, []string{"trust", "unary interceptor", "handler"}},
		{"a stream with a scenario, with failure injection", true, true, streamCall, []string{"trust", "stream interceptor", "handler"}},
		{"an RPC without a scenario", true, false, unaryCall, []string{"unary interceptor", "handler"}},
		{"a stream without a scenario", true, false, streamCall, []string{"stream interceptor", "handler"}},
		{"an App without failure injection", false, true, unaryCall, []string{"unary interceptor", "handler"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				res := runFaults(t, faultsRun{faults: tc.faults, scenario: tc.scenario, rpc: tc.rpc, trust: func(context.Context, string) bool { return true }})
				handler, span := requireServed(t, res, tc.rpc)

				require.Equal(t, tc.layers, res.seen.layers(), "TrustScenario was asked when it needn't be, or after the end user's interceptors")
				require.Equal(t, tc.scenario, handler.scenario)
				require.Empty(t, eventsNamed(span, stripEvent))
			})
		})
	}

	t.Run("TrustScenario is given the scenario, before it is stripped", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, rpc: unaryCall, trust: func(context.Context, string) bool { return false }})
			requireServed(t, res, unaryCall)

			trust, ok := res.seen.sightOf("trust")
			require.True(t, ok)
			require.True(t, trust.scenario, "TrustScenario was given a context without the scenario")
			require.Equal(t, abortPayments.Rules, trust.rules)
		})
	})
}

func TestService_a_TrustScenario_panic(t *testing.T) {
	for _, rpc := range []string{unaryCall, streamCall} {
		t.Run("fails the RPC with Internal and is recorded, as a handler's is: "+rpc, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				res := runFaults(t, faultsRun{faults: true, scenario: true, rpc: rpc, trust: func(context.Context, string) bool { panic(errBoom) }})

				requireInternal(t, res.err)
				require.NotContains(t, res.err.Error(), "boom", "the panic's value reached the client")
				require.Equal(t, []string{"trust"}, res.seen.layers(), "the end user's code ran after TrustScenario panicked")

				require.Len(t, res.spans, 1)
				span := res.spans[0]
				require.Equal(t, rpc, span.Name())
				requireRecordedIn(t, res.panic, span, "faults_test.go")
				require.Empty(t, eventsNamed(span, stripEvent))
			})
		})
	}
}
