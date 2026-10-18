// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/humustest"
)

// The issuer and audience every test's Verifier expects.
const (
	issuer   = "https://idp.example.com"
	audience = "carts"
)

// Claims is the test's principal: the registered claims it reads, and a
// claim of its own.
type Claims struct {
	Subject string `json:"sub"`
	Scope   string `json:"scope"`
}

// keySet is the test's own KeySet, in memory, which counts its calls. A
// call fails with err when it is set, and waits for release when it is
// set, until the test closes it.
type keySet struct {
	mu      sync.Mutex
	keys    []jwt.Key
	err     error
	release chan struct{}
	calls   int
}

func (k *keySet) Keys(context.Context) ([]jwt.Key, error) {
	k.mu.Lock()
	k.calls++
	release := k.release
	k.mu.Unlock()
	if release != nil {
		<-release
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.err != nil {
		return nil, k.err
	}
	return append([]jwt.Key(nil), k.keys...), nil
}

// count returns how many times the KeySet was called.
func (k *keySet) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls
}

// add adds a public key to the set.
func (k *keySet) add(key jwt.Key) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, key)
}

// fail makes every later call fail with err, or succeed when err is nil.
func (k *keySet) fail(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

// hold makes every later call wait until the returned function is called.
func (k *keySet) hold() func() {
	k.mu.Lock()
	defer k.mu.Unlock()
	c := make(chan struct{})
	k.release = c
	return func() {
		k.mu.Lock()
		k.release = nil
		k.mu.Unlock()
		close(c)
	}
}

// signer signs tokens with one private key, under one kid.
type signer struct {
	kid string
	alg jose.SignatureAlgorithm
	key crypto.Signer
}

// newSigner returns a signer of a P-256 key generated with crypto/rand,
// whose kid is kid.
func newSigner(t *testing.T, kid string) *signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &signer{kid: kid, alg: jose.ES256, key: key}
}

// public returns the signer's public key, as a KeySet returns it.
func (s *signer) public() jwt.Key {
	return jwt.Key{ID: s.kid, Public: s.key.Public()}
}

// tokenClaims is what a test puts in a token: its registered claims, which are
// good ones unless the test changes them, and the test's own.
type tokenClaims struct {
	josejwt.Claims
	Scope string `json:"scope,omitempty"`
}

// good returns the registered claims of a token the test's Verifier
// accepts, for subject, valid from now for ttl.
func good(subject string, ttl time.Duration) tokenClaims {
	now := time.Now()
	return tokenClaims{Claims: josejwt.Claims{
		Issuer:    issuer,
		Audience:  josejwt.Audience{audience},
		Subject:   subject,
		IssuedAt:  josejwt.NewNumericDate(now),
		NotBefore: josejwt.NewNumericDate(now),
		Expiry:    josejwt.NewNumericDate(now.Add(ttl)),
	}}
}

// sign signs tok, a token or any claims, with the signer's kid in its
// header, or with no kid when the signer's kid is empty.
func (s *signer) sign(t *testing.T, tok any) string {
	t.Helper()
	var key any = s.key
	if s.kid != "" {
		key = jose.JSONWebKey{Key: s.key, KeyID: s.kid}
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: s.alg, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	require.NoError(t, err)
	raw, err := josejwt.Signed(sig).Claims(tok).Serialize()
	require.NoError(t, err)
	return raw
}

// settings returns DefaultSettings with the test's issuer and audience.
func settings() jwt.Settings {
	s := jwt.DefaultSettings()
	s.Issuer = issuer
	s.Audience = audience
	return s
}

// setup returns a Setup whose LoggerProvider records every record, and the
// Telemetry it records into.
func setup(t *testing.T) (*humus.Setup, *humustest.Telemetry) {
	t.Helper()
	tel := humustest.NewTelemetry()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	return &humus.Setup{Telemetry: sdk}, tel
}

// newVerifier returns a Verifier of Claims on keys, with the test's
// settings, and the Telemetry it logs into.
func newVerifier(t *testing.T, keys jwt.KeySet) (*jwt.Verifier[Claims], *humustest.Telemetry) {
	t.Helper()
	s, tel := setup(t)
	v, err := jwt.NewVerifier[Claims](s, settings(), keys)
	require.NoError(t, err)
	return v, tel
}

// bodies returns each record's severity and body, as "WARN keys refresh
// failed".
func bodies(records []sdklog.Record) []string {
	var out []string
	for _, r := range records {
		out = append(out, r.SeverityText()+" "+r.Body().AsString())
	}
	return out
}
