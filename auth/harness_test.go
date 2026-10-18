// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

// loggerName is the logger the package logs through.
const loggerName = "github.com/z5labs/humus/auth"

// user is the principal of every scheme the tests declare.
type user struct {
	name   string
	scopes []string
}

func (u user) HasScope(scope string) bool { return slices.Contains(u.scopes, scope) }

// credentials are the valid credentials of one test, each made with
// crypto/rand, so no credential is fixed. A verifier counts its calls.
type credentials struct {
	users map[string]user // by token, key or password
	calls atomic.Int64
}

// newCredentials returns credentials holding a new random credential for
// each of users, as creds[name].
func newCredentials(users ...user) (*credentials, map[string]string) {
	c := &credentials{users: make(map[string]user)}
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
func (c *credentials) verify(_ context.Context, secret string) (user, error) {
	c.calls.Add(1)
	u, ok := c.users[secret]
	if !ok {
		return user{}, auth.ErrUnauthenticated
	}
	return u, nil
}

// verifyBasic is a verify function of a username and password.
func (c *credentials) verifyBasic(_ context.Context, username, password string) (user, error) {
	c.calls.Add(1)
	u, ok := c.users[password]
	if !ok || u.name != username {
		return user{}, auth.ErrUnauthenticated
	}
	return u, nil
}

// answer returns a handler that answers with status and body.
func answer(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

// answers are the end user's answers, in plain text.
var answers = auth.Answers{
	Unauthenticated: answer(http.StatusUnauthorized, "the end user's 401"),
	Forbidden:       answer(http.StatusForbidden, "the end user's 403"),
	Failed:          answer(http.StatusServiceUnavailable, "the end user's 503"),
}

// server is an App serving a handler through httpserver, on a humustest
// Host.
type server struct {
	t      *testing.T
	h      *humustest.Host
	ca     *humustest.CA
	tel    *humustest.Telemetry
	client *http.Client
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

// serve starts an App whose only Service serves the handler build returns,
// with httpserver over TLS, with a certificate from a new humustest CA.
// edit, when it isn't nil, edits the server's TLS config, given the CA.
func serve(t *testing.T, build func(s *humus.Setup) (http.Handler, error), edit func(c *tls.Config, ca *humustest.CA)) *server {
	t.Helper()
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "localhost")
	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()

	p, status := httpservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		handler, err := build(s)
		if err != nil {
			return nil, err
		}
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		if edit != nil {
			edit(settings.TLS, ca)
		}
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
	select {
	case <-h.Listening("localhost:8443"):
	case st := <-status:
		t.Fatalf("the App exited before it listened: %v", st.Cause())
	}
	return &server{t: t, h: h, ca: ca, tel: tel, client: httpservertest.NewClient(h, ca.Pool()), p: p, status: status}
}

// clientWith returns a client that presents cert in the TLS handshake.
func (s *server) clientWith(cert *humustest.Certificate) *http.Client {
	c := httpservertest.NewClient(s.h, s.ca.Pool())
	c.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{cert.TLS()}
	return c
}

// stop stops the App, so every span the requests started has ended, and
// checks that it exited cleanly, having written nothing to stderr.
func (s *server) stop() {
	s.t.Helper()
	s.p.Signal(syscall.SIGTERM)
	st := <-s.status
	require.Zero(s.t, st.Code(), "cause: %v", st.Cause())
	require.Empty(s.t, s.p.Stderr())
}

// reply is a response, read in full.
type reply struct {
	status int
	header http.Header
	body   string
}

// challenges returns the reply's WWW-Authenticate values.
func (r reply) challenges() []string { return r.header.Values("WWW-Authenticate") }

// request returns a GET of path, edited by each of edits.
func (s *server) request(path string, edits ...func(*http.Request)) *http.Request {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodGet, "https://localhost:8443"+path, nil)
	require.NoError(s.t, err)
	for _, e := range edits {
		e(req)
	}
	return req
}

// get sends a GET of path, edited by each of edits, with s's client.
func (s *server) get(path string, edits ...func(*http.Request)) reply {
	s.t.Helper()
	return s.send(s.client, s.request(path, edits...))
}

// send sends req with c.
func (s *server) send(c *http.Client, req *http.Request) reply {
	s.t.Helper()
	resp, err := c.Do(req)
	require.NoError(s.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	return reply{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

// header returns an edit that adds each of values to the header name.
func header(name string, values ...string) func(*http.Request) {
	return func(r *http.Request) {
		for _, v := range values {
			r.Header.Add(name, v)
		}
	}
}

// bearer returns an edit that sends token as a bearer token.
func bearer(token string) func(*http.Request) {
	return header("Authorization", "Bearer "+token)
}

// serverSpans returns the spans httpserver's otelhttp recorded.
func serverSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range tel.Spans() {
		if s.InstrumentationScope().Name == otelhttp.ScopeName {
			spans = append(spans, s)
		}
	}
	return spans
}

// recordsOf returns the records the logger name logged.
func recordsOf(tel *humustest.Telemetry, name string) []sdklog.Record {
	var recs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == name {
			recs = append(recs, r)
		}
	}
	return recs
}

// records returns the records the package logged.
func records(tel *humustest.Telemetry) []sdklog.Record { return recordsOf(tel, loggerName) }

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

// principals is a handler that writes the principal of each scheme, by
// its name, or "none" when no scheme authenticated the request.
func principals(schemes map[string]auth.Scheme[user]) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got []string
		for _, name := range slices.Sorted(maps.Keys(schemes)) {
			if u, ok := schemes[name].From(r.Context()); ok {
				got = append(got, name+":"+u.name)
			}
		}
		if len(got) == 0 {
			got = []string{"none"}
		}
		_, _ = io.WriteString(w, strings.Join(got, " "))
	})
}
