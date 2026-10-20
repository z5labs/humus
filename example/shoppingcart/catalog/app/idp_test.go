// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"strings"
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

const (
	issuer   = "https://idp.internal"
	audience = "catalog"
	jwksPath = "/.well-known/jwks.json"
)

type idp struct {
	kid string
	key *ecdsa.PrivateKey
}

func newIdP(t *testing.T) *idp {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &idp{kid: rand.Text(), key: key}
}

func (p *idp) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
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
			Subject:   "operator",
			IssuedAt:  josejwt.NewNumericDate(now),
			NotBefore: josejwt.NewNumericDate(now),
			Expiry:    josejwt.NewNumericDate(now.Add(time.Hour)),
		},
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
