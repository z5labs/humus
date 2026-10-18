// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcauth_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/grpc/grpcauth"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/humustest"
)

// loggerName is the logger the package logs through.
const loggerName = "github.com/z5labs/humus/grpc/grpcauth"

// user is the principal of every scheme the tests declare.
type user struct {
	name   string
	scopes []string
}

func (u user) HasScope(scope string) bool { return slices.Contains(u.scopes, scope) }

// secrets are the valid credentials of one test, each made with
// crypto/rand, so no credential is fixed. A verifier counts its calls.
type secrets struct {
	users map[string]user // by token or key
	calls atomic.Int64
}

// newSecrets returns secrets holding a new random credential for each of
// users, as creds[name].
func newSecrets(users ...user) (*secrets, map[string]string) {
	c := &secrets{users: make(map[string]user)}
	creds := make(map[string]string)
	for _, u := range users {
		secret := rand.Text()
		c.users[secret] = u
		creds[u.name] = secret
	}
	return c, creds
}

// verify is a verify function of a token or a key: an unknown one wraps
// auth.ErrUnauthenticated.
func (c *secrets) verify(_ context.Context, secret string) (user, error) {
	c.calls.Add(1)
	u, ok := c.users[secret]
	if !ok {
		return user{}, auth.ErrUnauthenticated
	}
	return u, nil
}

// whoami returns the principal of each of schemes in ctx, by the scheme's
// name, or "none" when no scheme authenticated the RPC.
func whoami(ctx context.Context, schemes map[string]auth.Scheme[user]) string {
	var got []string
	for _, name := range slices.Sorted(maps.Keys(schemes)) {
		if u, ok := schemes[name].From(ctx); ok {
			got = append(got, name+":"+u.name)
		}
	}
	if len(got) == 0 {
		return "none"
	}
	return strings.Join(got, " ")
}

// testServer answers UnaryCall with the principals in the handler's
// context, as the response's Username, and StreamingOutputCall with one
// message whose payload is the principals in the stream's context.
type testServer struct {
	testpb.UnimplementedTestServiceServer

	schemes map[string]auth.Scheme[user]
}

func (testServer) EmptyCall(context.Context, *testpb.Empty) (*testpb.Empty, error) {
	return &testpb.Empty{}, nil
}

func (ts testServer) UnaryCall(ctx context.Context, _ *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	return &testpb.SimpleResponse{Username: whoami(ctx, ts.schemes)}, nil
}

func (ts testServer) StreamingOutputCall(_ *testpb.StreamingOutputCallRequest, stream grpc.ServerStreamingServer[testpb.StreamingOutputCallResponse]) error {
	return stream.Send(&testpb.StreamingOutputCallResponse{
		Payload: &testpb.Payload{Body: []byte(whoami(stream.Context(), ts.schemes))},
	})
}

// app is an App serving testServer and gRPC health through grpcserver,
// secured by grpcauth, on a humustest Host.
type app struct {
	t      *testing.T
	h      *humustest.Host
	ca     *humustest.CA
	tel    *humustest.Telemetry
	conn   *grpc.ClientConn
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

// serve starts an App whose gRPC Service, at carts:9090, serves a
// testServer of schemes and a health server, behind the interceptors of
// rules, over TLS with a certificate from a new humustest CA. edit, when
// it isn't nil, edits the Service's TLS config, given the CA.
//
// It must be called inside a synctest bubble.
func serve(t *testing.T, schemes map[string]auth.Scheme[user], rules []grpcauth.Rule, edit func(c *tls.Config, ca *humustest.CA)) *app {
	t.Helper()
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "carts")
	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()

	p, status := grpcservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		unary, stream, err := grpcauth.Interceptors(s, rules...)
		if err != nil {
			return nil, err
		}
		settings := grpcserver.Settings{
			Addr: ":9090",
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		}
		if edit != nil {
			edit(settings.TLS, ca)
		}
		svc, err := grpcserver.NewService(s, settings, unary, stream)
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, testServer{schemes: schemes})
		hs := health.NewService()
		healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, hs.Probes()))
		return []humus.Service{svc, hs}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
	select {
	case <-h.Listening("carts:9090"):
	case st := <-status:
		t.Fatalf("the App exited before it listened: %v", st.Cause())
	}
	synctest.Wait()
	return &app{t: t, h: h, ca: ca, tel: tel, conn: grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()), p: p, status: status}
}

// clientWith returns a connection to the App whose client presents cert in
// the TLS handshake.
func (a *app) clientWith(cert *humustest.Certificate) *grpc.ClientConn {
	return grpcservertest.NewClient(a.t, a.h, "carts:9090", a.ca.Pool(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs:      a.ca.Pool(),
		Certificates: []tls.Certificate{cert.TLS()},
	})))
}

// tests returns a TestService client of the App.
func (a *app) tests() testpb.TestServiceClient { return testpb.NewTestServiceClient(a.conn) }

// stop stops the App, so every span the RPCs started has ended, and checks
// that it exited cleanly, having written nothing to stderr.
func (a *app) stop() {
	a.t.Helper()
	a.p.Signal(syscall.SIGTERM)
	st := <-a.status
	require.Zero(a.t, st.Code(), "cause: %v", st.Cause())
	require.Empty(a.t, a.p.Stderr())
}

// with returns the test's context, carrying the outgoing metadata of kv,
// key and value pairs.
func with(t *testing.T, kv ...string) context.Context {
	return metadata.AppendToOutgoingContext(t.Context(), kv...)
}

// bearer returns the test's context, carrying token as a bearer token.
func bearer(t *testing.T, token string) context.Context {
	return with(t, "authorization", "Bearer "+token)
}

// requireStatus fails t unless err is a gRPC status of code and msg.
func requireStatus(t *testing.T, err error, code codes.Code, msg string) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "%v is not a gRPC status", err)
	require.Equal(t, code, st.Code(), "status: %v", st)
	require.Equal(t, msg, st.Message())
}

// serverSpans returns the spans grpcserver's otelgrpc recorded.
func serverSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range tel.Spans() {
		if s.InstrumentationScope().Name == otelgrpc.ScopeName {
			spans = append(spans, s)
		}
	}
	return spans
}

// records returns the records the package logged.
func records(tel *humustest.Telemetry) []sdklog.Record {
	var recs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == loggerName {
			recs = append(recs, r)
		}
	}
	return recs
}

// attrs returns r's attributes as strings.
func attrs(r sdklog.Record) map[string]string {
	m := make(map[string]string)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		m[string(kv.Key)] = kv.Value.String()
		return true
	})
	return m
}

// exceptionMessages returns the exception.message of each exception event
// on span.
func exceptionMessages(span sdktrace.ReadOnlySpan) []string {
	var msgs []string
	for _, e := range span.Events() {
		if e.Name != "exception" {
			continue
		}
		for _, kv := range e.Attributes {
			if kv.Key == "exception.message" {
				msgs = append(msgs, kv.Value.AsString())
			}
		}
	}
	return msgs
}
