// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/telemetry"
)

// UserClaims is the end user's principal: the claims of theirs a handler
// reads.
type UserClaims struct {
	Subject string `json:"sub"`
	Scope   string `json:"scope"`
}

// SecretStore is where the end user's organization keeps its token-signing
// public keys, as PEM, by kid. It isn't a JSON Web Key Set.
type SecretStore interface {
	PublicKeys(ctx context.Context) (map[string][]byte, error)
}

// StoreKeys is a KeySet of the end user's, over their SecretStore. It is a
// fetch and nothing more: the Verifier holds the keys, and decides when to
// call it again.
type StoreKeys struct {
	Store SecretStore
}

// Keys returns every key in the store. A key that isn't PEM, or doesn't
// parse, is left out, so one bad entry doesn't fail the set.
func (k StoreKeys) Keys(ctx context.Context) ([]jwt.Key, error) {
	pems, err := k.Store.PublicKeys(ctx)
	if err != nil {
		return nil, err
	}
	var keys []jwt.Key
	for kid, b := range pems {
		block, _ := pem.Decode(b)
		if block == nil {
			continue
		}
		public, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			continue
		}
		keys = append(keys, jwt.Key{ID: kid, Public: public})
	}
	return keys, nil
}

// memoryStore is a SecretStore in memory, as a test of a verify function
// has one.
type memoryStore map[string][]byte

func (m memoryStore) PublicKeys(context.Context) (map[string][]byte, error) {
	if m == nil {
		return nil, errors.New("the secret store is down")
	}
	return m, nil
}

// Verifying a token against keys from the end user's own secret store, with
// the Verifier as a Bearer scheme's verify function. The store is in
// memory, as in a test of the verify function: no identity provider is
// served.
func Example() {
	// The identity provider's key pair: the store holds the public half.
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		panic(err)
	}
	store := memoryStore{"2026-10": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})}

	// In the end user's Init.
	s := &humus.Setup{Telemetry: telemetry.Noop()}
	settings := jwt.DefaultSettings() // sourced with config.Struct
	settings.Issuer = "https://idp.example.com"
	settings.Audience = "carts"
	tokens, err := jwt.NewVerifier[UserClaims](s, settings, StoreKeys{Store: store})
	if err != nil {
		panic(err)
	}
	bearer := auth.Bearer("bearer", tokens.Verify).Format("JWT")
	_ = bearer // given to openapi.Security, grpcauth or auth.NewHandler

	// A token the identity provider signed.
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.EdDSA,
		Key:       jose.JSONWebKey{Key: private, KeyID: "2026-10"},
	}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		panic(err)
	}
	now := time.Now()
	token, err := josejwt.Signed(signer).Claims(josejwt.Claims{
		Issuer:   "https://idp.example.com",
		Audience: josejwt.Audience{"carts"},
		Subject:  "alice",
		IssuedAt: josejwt.NewNumericDate(now),
		Expiry:   josejwt.NewNumericDate(now.Add(5 * time.Minute)),
	}).Claims(map[string]any{"scope": "carts:read"}).Serialize()
	if err != nil {
		panic(err)
	}

	claims, err := tokens.Verify(context.Background(), token)
	fmt.Println(claims.Subject, claims.Scope, err)

	_, err = tokens.Verify(context.Background(), token+"x")
	fmt.Println(errors.Is(err, auth.ErrUnauthenticated))

	// With the store down and no keys held, the error is the server's.
	down, err := jwt.NewVerifier[UserClaims](s, settings, StoreKeys{Store: memoryStore(nil)})
	if err != nil {
		panic(err)
	}
	_, err = down.Verify(context.Background(), token)
	var unreachable *jwt.KeySetError
	fmt.Println(errors.As(err, &unreachable), errors.Is(err, auth.ErrUnauthenticated))
	// Output:
	// alice carts:read <nil>
	// true
	// true false
}
