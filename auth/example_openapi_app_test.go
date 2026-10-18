// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The issuer and audience of the test's identity provider's tokens.
const (
	testIssuer   = "https://idp.internal"
	testAudience = "carts"
)

// identityProvider is the test's identity provider: a handler that serves
// the public half of a key generated with crypto/rand as a JSON Web Key
// Set, and signs the tokens the test sends with its private half. While it
// is down, it answers 503.
type identityProvider struct {
	key *ecdsa.PrivateKey
	kid string

	mu   sync.Mutex
	down bool
}

// newIdentityProvider returns a provider with a new P-256 key.
func newIdentityProvider(t *testing.T) *identityProvider {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &identityProvider{key: key, kid: rand.Text()}
}

func (p *identityProvider) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	down := p.down
	p.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: p.key.Public(), KeyID: p.kid, Algorithm: string(jose.ES256), Use: "sig"},
	}}
	w.Header().Set("Content-Type", "application/jwk-set+json")
	_ = json.NewEncoder(w).Encode(set)
}

// setDown makes the provider answer 503 while down is true.
func (p *identityProvider) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

// token returns a token for subject, granted scope, valid for an hour.
func (p *identityProvider) token(t *testing.T, subject, scope string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.ES256,
		Key:       jose.JSONWebKey{Key: p.key, KeyID: p.kid},
	}, (&jose.SignerOptions{}).WithType("JWT"))
	require.NoError(t, err)
	now := time.Now()
	token, err := josejwt.Signed(signer).Claims(josejwt.Claims{
		Issuer:   testIssuer,
		Audience: josejwt.Audience{testAudience},
		Subject:  subject,
		IssuedAt: josejwt.NewNumericDate(now),
		Expiry:   josejwt.NewNumericDate(now.Add(time.Hour)),
	}).Claims(map[string]any{"scope": scope}).Serialize()
	require.NoError(t, err)
	return token
}

// serveIdentityProvider serves p at idp.internal:443 on h, with httpserver
// over TLS, with a certificate from ca. It returns the function that stops
// it, which the test calls before its bubble ends.
func serveIdentityProvider(t *testing.T, h *humustest.Host, ca *humustest.CA, p *identityProvider) func() {
	t.Helper()
	cert := ca.Issue(t, "idp.internal")
	proc, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, p)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("idp.internal:443")
	return func() {
		proc.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}

// startCarts starts CartsInit on h, trusting ca for the identity
// provider's key set and its own certificate, and waits for it to listen.
func startCarts(t *testing.T, h *humustest.Host, ca *humustest.CA) *server {
	t.Helper()
	tokens := jwt.DefaultSettings()
	tokens.Issuer = testIssuer
	tokens.Audience = testAudience
	keySet, err := url.Parse("https://idp.internal/.well-known/jwks.json")
	require.NoError(t, err)
	idp := IdP{KeySet: keySet, Roots: ca.Pool(), Tokens: tokens}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}

	tel := humustest.NewTelemetry()
	p, status := httpservertest.Start(t, CartsInit(idp, tlsConfig),
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
	select {
	case <-h.Listening("localhost:8443"):
	case st := <-status:
		t.Fatalf("the App exited before it listened: %v", st.Cause())
	}
	return &server{t: t, h: h, ca: ca, tel: tel, client: httpservertest.NewClient(h, ca.Pool()), p: p, status: status}
}

// method returns an edit that sets the request's method.
func method(m string) func(*http.Request) {
	return func(r *http.Request) { r.Method = m }
}

func TestCartsInit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		idp := newIdentityProvider(t)
		idp.setDown(true) // until the first token has been answered
		stopIdP := serveIdentityProvider(t, h, ca, idp)
		defer stopIdP()
		s := startCarts(t, h, ca)

		reader := idp.token(t, "alice", "carts:read")
		writer := idp.token(t, "bob", "carts:read carts:write")

		// With the key set down and no keys held, a token may be valid: a
		// 503, never a 401.
		r := s.get("/carts/mine", bearer(reader))
		require.Equal(t, http.StatusServiceUnavailable, r.status)
		require.Empty(t, r.challenges())
		require.JSONEq(t, `{"type":"about:blank","title":"Service Unavailable","status":503,"detail":"The server can't verify tokens right now. Try again later."}`, r.body)

		// The key set is back, and called again once MinRefreshInterval
		// has passed.
		idp.setDown(false)
		time.Sleep(time.Minute)

		r = s.get("/carts/mine", bearer(reader))
		require.Equal(t, http.StatusOK, r.status)
		require.JSONEq(t, `{"owner":"alice"}`, r.body)

		r = s.get("/carts/mine")
		require.Equal(t, http.StatusUnauthorized, r.status)
		require.Equal(t, []string{"Bearer"}, r.challenges())
		require.JSONEq(t, `{"type":"about:blank","title":"Unauthorized","status":401,"detail":"unauthenticated"}`, r.body)

		r = s.get("/carts/mine", bearer(reader+"x"))
		require.Equal(t, http.StatusUnauthorized, r.status)
		require.Equal(t, []string{`Bearer error="invalid_token"`}, r.challenges())

		r = s.get("/carts/mine", bearer(reader), method(http.MethodDelete))
		require.Equal(t, http.StatusForbidden, r.status)
		require.Equal(t, []string{`Bearer error="insufficient_scope", scope="carts:write"`}, r.challenges())

		r = s.get("/carts/mine", bearer(writer), method(http.MethodDelete))
		require.Equal(t, http.StatusNoContent, r.status)

		// The greeting's authentication is optional, but a bad token is
		// refused, never served anonymously.
		r = s.get("/greeting")
		require.Equal(t, http.StatusOK, r.status)
		require.JSONEq(t, `{"message":"hello, stranger"}`, r.body)
		r = s.get("/greeting", bearer(reader))
		require.Equal(t, http.StatusOK, r.status)
		require.JSONEq(t, `{"message":"hello, alice"}`, r.body)
		r = s.get("/greeting", bearer(reader+"x"))
		require.Equal(t, http.StatusUnauthorized, r.status)

		// The document and the probes are open.
		r = s.get("/openapi.json")
		require.Equal(t, http.StatusOK, r.status)
		require.Contains(t, r.body, `"bearerFormat":"JWT"`)
		r = s.get("/health/liveness")
		require.Equal(t, http.StatusOK, r.status)

		s.stop()
		// A refusal is the client's mistake, and the 503 is an Error's
		// answer: neither is logged.
		require.Empty(t, recordsOf(s.tel, "github.com/z5labs/humus/openapi/v3"))
		require.Empty(t, records(s.tel))
	})
}
