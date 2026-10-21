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
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	issuer   = "https://idp.internal"
	audience = "wallet"
)

const jwksPath = "/.well-known/jwks.json"

type idp struct {
	kid string
	key *ecdsa.PrivateKey

	mu   sync.Mutex
	down bool
}

func newIdP(t *testing.T) *idp {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &idp{kid: rand.Text(), key: key}
}

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

func (p *idp) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

func (p *idp) token(t *testing.T, subject string, scopes ...string) string {
	t.Helper()
	raw, err := p.sign(subject, scopes...)
	require.NoError(t, err)
	return raw
}

func (p *idp) sign(subject string, scopes ...string) (string, error) {
	now := time.Now()
	claims := struct {
		josejwt.Claims
		Scope string `json:"scope,omitempty"`
	}{
		Claims: josejwt.Claims{
			Issuer:    issuer,
			Audience:  josejwt.Audience{audience},
			Subject:   subject,
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
	if err != nil {
		return "", err
	}
	return josejwt.Signed(sig).Claims(claims).Serialize()
}

func (p *idp) serve(t *testing.T, host humus.Host, addr string, cert *humustest.Certificate) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	return humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
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
}
