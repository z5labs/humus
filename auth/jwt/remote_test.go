// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts.
// The identity provider is an App of the test's own, which serves a JSON
// Web Key Set from the test's handler with httpserver at idp:8443, over
// TLS with a certificate from a CA generated for each test. The end user's
// App fetches it with an httpclient client that trusts that CA through
// RootCAs.

// jwksURL is where the test's identity provider serves its key set.
const jwksURL = "https://idp:8443/.well-known/jwks.json"

// idp is the test's identity provider: a handler that serves the public
// halves of its signers' keys as a JSON Web Key Set, and counts the
// requests it gets and the sets it serves. While it is down, it answers
// 503. A body, when it is set, is served in place of the set.
type idp struct {
	mu       sync.Mutex
	signers  map[string]*signer
	order    []string
	body     []byte
	down     bool
	requests int
	served   int
}

func (p *idp) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests++
	if p.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body := p.body
	if body == nil {
		var set jose.JSONWebKeySet
		for _, kid := range p.order {
			s := p.signers[kid]
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: s.key.Public(), KeyID: kid, Algorithm: string(s.alg), Use: "sig"})
		}
		b, err := json.Marshal(set)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body = b
	}
	p.served++
	w.Header().Set("Content-Type", "application/jwk-set+json")
	_, _ = w.Write(body)
}

// add generates a key with kid, and serves its public half in the set.
func (p *idp) add(t *testing.T, kid string) {
	t.Helper()
	s := newSigner(t, kid)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.signers == nil {
		p.signers = make(map[string]*signer)
	}
	p.signers[kid] = s
	p.order = append(p.order, kid)
}

// sign returns a token for audience, valid for ttl, signed with the key
// whose kid is kid.
func (p *idp) sign(t *testing.T, kid, audience string, ttl time.Duration) string {
	t.Helper()
	p.mu.Lock()
	s := p.signers[kid]
	p.mu.Unlock()
	require.NotNil(t, s, "no key %s", kid)
	c := good("alice", ttl)
	c.Audience = []string{audience}
	return s.sign(t, c)
}

// setDown makes the provider answer 503 while down is true.
func (p *idp) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

// serve makes the provider serve body in place of its set.
func (p *idp) serve(body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.body = body
}

// count returns how many sets the provider served.
func (p *idp) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.served
}

// requested returns how many requests the provider got, while down
// included.
func (p *idp) requested() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests
}

// network is a test's humustest Host and the CA every App on it trusts.
type network struct {
	h    *humustest.Host
	ca   *humustest.CA
	stop func()
}

// newNetwork returns a Host with the test's identity provider serving
// handler at idp:8443. The test stops the provider with stop, before its
// bubble ends.
func newNetwork(t *testing.T, handler http.Handler) *network {
	t.Helper()
	n := &network{h: humustest.NewHost(t), ca: humustest.NewCA(t)}
	cert := n.ca.Issue(t, "idp")
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(n.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	select {
	case <-n.h.Listening("idp:8443"):
	case st := <-status:
		t.Fatalf("the identity provider exited before it listened: %v", st.Cause())
	}
	n.stop = stopper(t, "the identity provider", p, status)
	return n
}

// remoteKeySet is how the end user's Init builds a RemoteKeySet: with an
// httpclient client for the key set's origin, which trusts the provider
// through its TLS.RootCAs.
func remoteKeySet(s *humus.Setup, roots *x509.CertPool) (*jwt.RemoteKeySet, error) {
	u, err := url.Parse(jwksURL)
	if err != nil {
		return nil, err
	}
	settings := httpclient.DefaultSettings()
	settings.BaseURL = &url.URL{Scheme: u.Scheme, Host: u.Host}
	settings.TLS = &tls.Config{RootCAs: roots}
	idp, err := httpclient.NewClient(s, settings)
	if err != nil {
		return nil, err
	}
	return jwt.NewRemoteKeySet(u, idp)
}

// api is the end user's App: an API at carts:9443, behind auth.NewHandler
// with a Bearer scheme whose verify function is a Verifier on a
// RemoteKeySet of the test's identity provider. It answers a token's
// subject, and a verifier's other error with a 503.
type api struct {
	t      *testing.T
	tel    *humustest.Telemetry
	client *http.Client
	stop   func()
}

// startAPI starts the end user's App on n. The test stops it with stop,
// before its bubble ends, which flushes its records.
func startAPI(t *testing.T, n *network) *api {
	t.Helper()
	cert := n.ca.Issue(t, "carts")
	tel := humustest.NewTelemetry()
	p, status := httpservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		keys, err := remoteKeySet(s, n.ca.Pool())
		if err != nil {
			return nil, err
		}
		tokens, err := jwt.NewVerifier[Claims](s, settings(), keys)
		if err != nil {
			return nil, err
		}
		bearer := auth.Bearer("bearer", tokens.Verify).Format("JWT")
		whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, _ := bearer.From(r.Context())
			_, _ = io.WriteString(w, c.Subject)
		})
		handler, err := auth.NewHandler(s, whoami, auth.Answers{
			Unauthenticated: answer(http.StatusUnauthorized),
			Forbidden:       answer(http.StatusForbidden),
			Failed:          answer(http.StatusServiceUnavailable),
		}, auth.Require(bearer))
		if err != nil {
			return nil, err
		}
		settings := httpserver.DefaultSettings()
		settings.Addr = ":9443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(n.h.Host()), humus.WithTelemetry(tel.Builder())))
	select {
	case <-n.h.Listening("carts:9443"):
	case st := <-status:
		t.Fatalf("the App exited before it listened: %v", st.Cause())
	}
	return &api{t: t, tel: tel, client: httpservertest.NewClient(n.h, n.ca.Pool()), stop: stopper(t, "the App", p, status)}
}

// stopper returns a function that stops the App p, once however often it
// is called, and fails t unless the App exits 0 having written nothing to
// stderr.
func stopper(t *testing.T, name string, p *bedrocktest.Process, status <-chan bedrock.ExitStatus) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.Signal(syscall.SIGTERM)
			st := <-status
			if st.Code() != 0 {
				t.Errorf("%s exited %d: %v", name, st.Code(), st.Cause())
			}
			if stderr := p.Stderr(); stderr != "" {
				t.Errorf("%s wrote to stderr: %s", name, stderr)
			}
		})
	}
}

// answer returns a handler that answers code.
func answer(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
}

// get sends a GET with token as a bearer token, and returns the status.
func (a *api) get(token string) int {
	a.t.Helper()
	req, err := http.NewRequestWithContext(a.t.Context(), http.MethodGet, "https://carts:9443/whoami", nil)
	require.NoError(a.t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.client.Do(req)
	require.NoError(a.t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(a.t, err)
	return resp.StatusCode
}

// logs returns the severity and body of each record auth/jwt logged.
func (a *api) logs() []string {
	return bodies(a.records("github.com/z5labs/humus/auth/jwt"))
}

// records returns the records the logger name logged.
func (a *api) records(name string) []sdklog.Record {
	var recs []sdklog.Record
	for _, r := range a.tel.Logs() {
		if r.InstrumentationScope().Name == name {
			recs = append(recs, r)
		}
	}
	return recs
}

// fetch runs the end user's App as a job, on n, whose run calls Keys once
// on a RemoteKeySet of the test's identity provider. It returns what Keys
// returned, once the App has exited.
func fetch(t *testing.T, n *network) ([]jwt.Key, error) {
	t.Helper()
	var (
		keys    []jwt.Key
		keysErr error
	)
	_, st := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		remote, err := remoteKeySet(s, n.ca.Pool())
		if err != nil {
			return nil, err
		}
		return []humus.Service{job.NewService(s, func(ctx context.Context) error {
			keys, keysErr = remote.Keys(ctx)
			return nil
		})}, nil
	}, humustest.WithApp(humus.WithHost(n.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	exit := <-st
	require.Zero(t, exit.Code(), "cause: %v", exit.Cause())
	return keys, keysErr
}

// ids returns each key's ID.
func ids(keys []jwt.Key) []string {
	out := []string{}
	for _, k := range keys {
		out = append(out, k.ID)
	}
	return out
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

func TestVerifier_fetches_rotates_and_rate_limits(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := &idp{}
		p.add(t, "k1")
		n := newNetwork(t, p)
		defer n.stop()
		hs := startAPI(t, n)
		defer hs.stop()

		t1 := p.sign(t, "k1", audience, 10*time.Minute)
		require.Equal(t, http.StatusOK, hs.get(t1))
		require.Equal(t, http.StatusOK, hs.get(t1))
		require.Equal(t, 1, p.count(), "fetched once, on the first token")
		require.Equal(t, http.StatusUnauthorized, hs.get(p.sign(t, "k1", "orders", time.Hour)))

		time.Sleep(12 * time.Minute) // past exp and the 1m leeway
		require.Equal(t, http.StatusUnauthorized, hs.get(t1))

		p.add(t, "k2") // rotated in
		require.Equal(t, http.StatusOK, hs.get(p.sign(t, "k2", audience, time.Hour)))
		require.Equal(t, 2, p.count())

		p.add(t, "k3")
		require.Equal(t, http.StatusUnauthorized, hs.get(p.sign(t, "k3", audience, time.Hour)))
		require.Equal(t, 2, p.count(), "at most one fetch a minute")
		time.Sleep(time.Minute)
		require.Equal(t, http.StatusOK, hs.get(p.sign(t, "k3", audience, time.Hour)))
		require.Equal(t, 3, p.count())

		p.setDown(true)
		time.Sleep(time.Hour + time.Second) // past MaxKeyAge
		require.Equal(t, http.StatusOK, hs.get(p.sign(t, "k3", audience, time.Hour)), "stale keys outlive a failed refresh")

		hs.stop() // flushes its records
		require.Equal(t, []string{"WARN keys refresh failed"}, hs.logs())

		// Each fetch is a span of the end user's httpclient client, and
		// auth/jwt traces and measures nothing of its own.
		var fetches int
		for _, span := range hs.tel.Spans() {
			require.NotEqual(t, "github.com/z5labs/humus/auth/jwt", span.InstrumentationScope().Name)
			if span.SpanKind() != trace.SpanKindClient {
				continue
			}
			for _, kv := range span.Attributes() {
				if kv.Key == "url.full" && kv.Value.AsString() == jwksURL {
					fetches++
				}
			}
		}
		require.Equal(t, p.requested(), fetches)
		rm, err := hs.tel.Metrics(t.Context())
		require.NoError(t, err)
		for _, sm := range rm.ScopeMetrics {
			require.NotEqual(t, "github.com/z5labs/humus/auth/jwt", sm.Scope.Name)
		}
		msg := attrs(hs.records("github.com/z5labs/humus/auth/jwt")[0])["exception.message"]
		require.Contains(t, msg, jwksURL)
		require.Contains(t, msg, "503")
	})
}

func TestVerifier_on_an_unreachable_provider(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := &idp{}
		p.add(t, "k1")
		p.setDown(true)
		n := newNetwork(t, p)
		defer n.stop()
		hs := startAPI(t, n)
		defer hs.stop()

		token := p.sign(t, "k1", audience, time.Hour)
		require.Equal(t, http.StatusServiceUnavailable, hs.get(token))
		requests := p.requested()
		require.Positive(t, requests)
		require.Zero(t, p.count())

		// Within the minute, the next token fails at once, without a fetch.
		time.Sleep(30 * time.Second)
		require.Equal(t, http.StatusServiceUnavailable, hs.get(token))
		require.Equal(t, requests, p.requested())

		// Once the provider is back and the minute has passed, one more
		// fetch serves the token.
		p.setDown(false)
		time.Sleep(30 * time.Second)
		require.Equal(t, http.StatusOK, hs.get(token))
		require.Equal(t, 1, p.count())
		require.Equal(t, http.StatusOK, hs.get(token))
		require.Equal(t, 1, p.count())

		// Each token the provider's outage failed is a *jwt.KeySetError,
		// whose message names the URL, which auth records as the server's.
		hs.stop() // flushes its records
		recs := hs.records("github.com/z5labs/humus/auth")
		require.Len(t, recs, 2)
		for _, r := range recs {
			require.Equal(t, "authentication failed", r.Body().AsString())
			a := attrs(r)
			require.Equal(t, "*jwt.KeySetError", a["error.type"])
			require.Contains(t, a["exception.message"], jwksURL)
		}
		require.Empty(t, hs.logs(), "no keys were held when the fetch failed")
	})
}

// jwk returns key as a JSON Web Key with kid and use.
func jwk(t *testing.T, key any, kid, use string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(jose.JSONWebKey{Key: key, KeyID: kid, Use: use})
	require.NoError(t, err)
	return b
}

// keySetBody returns a JSON Web Key Set of keys.
func keySetBody(t *testing.T, keys ...json.RawMessage) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"keys": keys})
	require.NoError(t, err)
	return b
}

func TestRemoteKeySet_skips_keys_it_cannot_verify_with(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		rs, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		ed, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		enc, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		noKid, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)

		p := &idp{}
		p.serve(keySetBody(t,
			jwk(t, ec.Public(), "ec", "sig"),
			jwk(t, enc.Public(), "enc", "enc"),           // an encryption key
			jwk(t, priv, "private", "sig"),               // with its private parts
			json.RawMessage(`{"kty":"XYZ","kid":"xyz"}`), // a type go-jose doesn't read
			json.RawMessage(`{"kty":"OKP","crv":"X25519","kid":"x25519","x":"`+strings.Repeat("A", 43)+`"}`), // a curve go-jose doesn't read
			jwk(t, noKid.Public(), "", "sig"), // without kid
			jwk(t, &rs.PublicKey, "rsa", ""),  // no use, which is a signing key's
			jwk(t, ed, "ed", "sig"),
		))
		n := newNetwork(t, p)
		defer n.stop()

		keys, err := fetch(t, n)
		require.NoError(t, err)
		require.Equal(t, []string{"ec", "rsa", "ed"}, ids(keys))
		require.Equal(t, ec.Public(), keys[0].Public)
		require.Equal(t, &rs.PublicKey, keys[1].Public)
		require.Equal(t, ed, keys[2].Public)
	})
}

func TestRemoteKeySet_reads_at_most_1_MiB(t *testing.T) {
	t.Parallel()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	set := keySetBody(t, jwk(t, key.Public(), "k1", "sig"))
	pad := func(size int) []byte {
		return append(append([]byte{}, set...), strings.Repeat(" ", size-len(set))...)
	}

	t.Run("a body of 1 MiB is read", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			p := &idp{}
			p.serve(pad(1 << 20))
			n := newNetwork(t, p)
			defer n.stop()
			keys, err := fetch(t, n)
			require.NoError(t, err)
			require.Equal(t, []string{"k1"}, ids(keys))
		})
	})
	t.Run("a body over 1 MiB fails", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			p := &idp{}
			p.serve(pad(1<<20 + 1))
			n := newNetwork(t, p)
			defer n.stop()
			keys, err := fetch(t, n)
			require.Error(t, err)
			require.Nil(t, keys)
			require.Contains(t, err.Error(), jwksURL)
			require.Contains(t, err.Error(), "over 1 MiB")
		})
	})
}

func TestRemoteKeySet_fails_on_what_isnt_a_key_set(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		handler http.Handler
		want    string
	}{
		"a 404": {
			handler: answer(http.StatusNotFound),
			want:    "404 Not Found",
		},
		"a 204": {
			handler: answer(http.StatusNoContent),
			want:    "204 No Content",
		},
		"a body that isn't JSON": {
			handler: serving("<html>a login page</html>"),
			want:    "isn't a JSON Web Key Set",
		},
		"an object with no keys": {
			handler: serving(`{"issuer":"https://idp.example.com"}`),
			want:    "no keys array",
		},
		"keys that aren't an array": {
			handler: serving(`{"keys":{"kid":"k1"}}`),
			want:    "isn't a JSON Web Key Set",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				n := newNetwork(t, c.handler)
				defer n.stop()
				keys, err := fetch(t, n)
				require.Error(t, err)
				require.Nil(t, keys)
				require.Contains(t, err.Error(), jwksURL)
				require.Contains(t, err.Error(), c.want)
			})
		})
	}

	t.Run("an empty set is no keys", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			n := newNetwork(t, serving(`{"keys":[]}`))
			defer n.stop()
			keys, err := fetch(t, n)
			require.NoError(t, err)
			require.Empty(t, keys)
		})
	})
}

// serving returns a handler that answers 200 with b.
func serving(b string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, b) })
}

func TestNewRemoteKeySet_mistakes(t *testing.T) {
	t.Parallel()

	client := &http.Client{}
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		return u
	}

	t.Run("a nil client and a relative URL are reported together", func(t *testing.T) {
		t.Parallel()
		keys, err := jwt.NewRemoteKeySet(parse("/.well-known/jwks.json"), nil)
		require.Nil(t, keys)
		require.ErrorContains(t, err, "jwt: NewRemoteKeySet")
		require.ErrorContains(t, err, "the client must not be nil")
		require.ErrorContains(t, err, `the URL "/.well-known/jwks.json" isn't absolute`)
	})

	cases := map[string]struct {
		u    *url.URL
		want []string
	}{
		"a nil URL":          {u: nil, want: []string{"the URL must not be nil"}},
		"an http URL":        {u: parse("http://idp:8443/jwks"), want: []string{`the URL "http://idp:8443/jwks" isn't https`}},
		"a URL with no host": {u: parse("https:///jwks"), want: []string{`the URL "https:///jwks" has no host`}},
		"an http URL with no host": {u: parse("http:///jwks"), want: []string{
			`the URL "http:///jwks" isn't https`,
			`the URL "http:///jwks" has no host`,
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			keys, err := jwt.NewRemoteKeySet(c.u, client)
			require.Nil(t, keys)
			require.ErrorContains(t, err, "jwt: NewRemoteKeySet")
			for _, w := range c.want {
				require.ErrorContains(t, err, w)
			}
		})
	}

	t.Run("an absolute https URL with a host", func(t *testing.T) {
		t.Parallel()
		keys, err := jwt.NewRemoteKeySet(parse(jwksURL), client)
		require.NoError(t, err)
		var _ jwt.KeySet = keys
	})
}

// roundTripper is an http.RoundTripper of a function.
type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRemoteKeySet_fails_on_a_redirect_away_from_https(t *testing.T) {
	t.Parallel()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	set := keySetBody(t, jwk(t, key.Public(), "k1", "sig"))

	// A client of the end user's, not httpclient's, which follows every
	// redirect: the https URL redirects to the same path over http, which
	// serves a key set.
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(set))), Request: r}
		if r.URL.Scheme == "https" {
			resp.StatusCode = http.StatusFound
			resp.Header.Set("Location", "http://idp:8080/.well-known/jwks.json")
			resp.Body = http.NoBody
		}
		return resp, nil
	})}
	u, err := url.Parse(jwksURL)
	require.NoError(t, err)
	remote, err := jwt.NewRemoteKeySet(u, client)
	require.NoError(t, err)

	keys, err := remote.Keys(t.Context())
	require.Nil(t, keys)
	require.ErrorContains(t, err, jwksURL)
	require.ErrorContains(t, err, `redirected to "http://idp:8080/.well-known/jwks.json", which isn't https`)
}

func TestRemoteKeySet_redacts_a_password_in_its_errors(t *testing.T) {
	t.Parallel()

	password := rand.Text()
	u, err := url.Parse("https://reader:" + password + "@idp:8443/.well-known/jwks.json")
	require.NoError(t, err)
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	})}
	remote, err := jwt.NewRemoteKeySet(u, client)
	require.NoError(t, err)
	_, err = remote.Keys(t.Context())
	require.ErrorContains(t, err, "https://reader:xxxxx@idp:8443/.well-known/jwks.json")
	require.NotContains(t, err.Error(), password)

	u.Scheme = "http"
	_, err = jwt.NewRemoteKeySet(u, client)
	require.ErrorContains(t, err, "isn't https")
	require.NotContains(t, err.Error(), password)
}

func TestRemoteKeySet_not_made_by_NewRemoteKeySet_fails(t *testing.T) {
	t.Parallel()

	var nilSet *jwt.RemoteKeySet
	for _, k := range []*jwt.RemoteKeySet{nilSet, {}} {
		keys, err := k.Keys(t.Context())
		require.Nil(t, keys)
		require.ErrorContains(t, err, "wasn't made by NewRemoteKeySet")
	}
}
