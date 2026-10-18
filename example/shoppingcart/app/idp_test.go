// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

// The tests are the shoppingcart's identity provider. This file is that
// provider: a key generated for each test, whose public half a handler of
// the test's own serves as a JSON Web Key Set, with httpserver, and whose
// private half signs the tokens the test sends. The unit tests serve it on
// their humustest Host, and the container test on the OS Host.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The token claims the shoppingcart is configured to check, as JWT_ISSUER
// and JWT_AUDIENCE.
const (
	issuer   = "https://idp.internal"
	audience = "shoppingcart"
)

// jwksPath is where the identity provider serves its key set.
const jwksPath = "/.well-known/jwks.json"

// idp is the tests' identity provider. While it is down, it answers 503.
type idp struct {
	kid string
	key *ecdsa.PrivateKey

	mu   sync.Mutex
	down bool
}

// newIdP returns an identity provider with a P-256 key and a kid generated
// with crypto/rand.
func newIdP(t *testing.T) *idp {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &idp{kid: rand.Text(), key: key}
}

// ServeHTTP serves the public half of the provider's key as a JSON Web Key
// Set, or 503 while the provider is down.
func (p *idp) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	down := p.down
	p.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       p.key.Public(),
		KeyID:     p.kid,
		Algorithm: string(jose.ES256),
		Use:       "sig",
	}}}
	b, err := json.Marshal(set)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/jwk-set+json")
	_, _ = w.Write(b)
}

// setDown makes the provider answer 503 while down is true.
func (p *idp) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

// token returns a token for the shoppingcart, valid for an hour from now,
// granted scopes, and signed with the provider's key.
func (p *idp) token(t *testing.T, scopes ...string) string {
	t.Helper()
	now := time.Now()
	claims := struct {
		josejwt.Claims
		Scope string `json:"scope,omitempty"`
	}{
		Claims: josejwt.Claims{
			Issuer:    issuer,
			Audience:  josejwt.Audience{audience},
			Subject:   "shopper",
			IssuedAt:  josejwt.NewNumericDate(now),
			NotBefore: josejwt.NewNumericDate(now),
			Expiry:    josejwt.NewNumericDate(now.Add(time.Hour)),
		},
		// endpoint.Claims reads the scopes from here.
		Scope: strings.Join(scopes, " "),
	}
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: p.key, KeyID: p.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)
	raw, err := josejwt.Signed(sig).Claims(claims).Serialize()
	require.NoError(t, err)
	return raw
}

// serve runs the provider as an App of its own on host, listening on addr
// over TLS with cert, and returns the function that stops it, which checks
// that it exits 0. The caller waits for it to listen.
func (p *idp) serve(t *testing.T, host humus.Host, addr string, cert *humustest.Certificate) (stop func()) {
	t.Helper()
	proc, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = addr
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		mux := http.NewServeMux()
		mux.Handle("GET "+jwksPath, p)
		svc, err := httpserver.NewService(s, settings, mux)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))
	return func() {
		t.Helper()
		<-proc.Notified(syscall.SIGTERM)
		proc.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "the identity provider's cause: %v", st.Cause())
	}
}

// caPEM returns the certificate of the CA that issued cert, PEM-encoded, as
// the shoppingcart reads it from JWKS_CA_FILE. A humustest Certificate's
// chain is its leaf, then its CA.
func caPEM(cert *humustest.Certificate) []byte {
	chain := cert.TLS().Certificate
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]})
}
