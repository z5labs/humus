// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt

import (
	"context"
	"errors"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
)

// loggerName is the name of the logger a Verifier logs through.
const loggerName = "github.com/z5labs/humus/auth/jwt"

// Verifier verifies JWTs, and decodes each one's claims into a C. It is
// safe for concurrent use.
type Verifier[C any] struct {
	issuer     string
	audience   string
	algorithms []jose.SignatureAlgorithm
	leeway     time.Duration
	keys       *keyring
}

// NewVerifier runs every check on settings, rejects a nil keys, and
// returns a Verifier that verifies tokens with the keys from keys. It
// calls keys for nothing until the first token. It logs through
// s.Telemetry's LoggerProvider, with the logger name
// "github.com/z5labs/humus/auth/jwt".
//
// It returns one error that names every mistake: each bad field of
// settings, a MaxKeyAge below MinRefreshInterval, a nil keys, and a Setup
// with no LoggerProvider.
func NewVerifier[C any](s *humus.Setup, settings Settings, keys KeySet) (*Verifier[C], error) {
	if s == nil {
		return nil, errors.New("jwt: NewVerifier: the Setup must not be nil")
	}
	errs := checkSettings(settings)
	if keys == nil {
		errs = append(errs, errors.New("the KeySet must not be nil"))
	}
	if s.Telemetry.LoggerProvider == nil {
		errs = append(errs, errors.New("the Setup's Telemetry.LoggerProvider must not be nil"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("jwt: NewVerifier: %w", err)
	}

	algs := make([]jose.SignatureAlgorithm, len(settings.Algorithms))
	for i, a := range settings.Algorithms {
		algs[i] = jose.SignatureAlgorithm(a)
	}
	return &Verifier[C]{
		issuer:     settings.Issuer,
		audience:   settings.Audience,
		algorithms: algs,
		leeway:     settings.Leeway,
		keys: &keyring{
			keySet:      keys,
			minInterval: settings.MinRefreshInterval,
			maxAge:      settings.MaxKeyAge,
			logger:      s.Telemetry.LoggerProvider.Logger(loggerName),
		},
	}, nil
}

// Verify is a verify function for Bearer, OAuth2 and OpenIDConnect. It
// verifies token's signature with the key its kid names, and its iss, aud,
// exp, nbf and iat, and returns its claims. A token that fails is an error
// that wraps auth.ErrUnauthenticated. A token it has no key for because
// the KeySet failed is a *KeySetError.
//
// A token is verified in this order:
//
//  1. It is parsed as a compact JWS signed with one of Algorithms, so none
//     and HMAC are rejected.
//  2. Its header must have a kid, and the keys held must have that key.
//  3. Its signature is verified, and its payload is decoded into the
//     registered claims and into a C.
//  4. iss must be Issuer, aud must hold Audience, exp must be present and
//     not past, and nbf and iat must not be in the future, all within
//     Leeway of time.Now().
//
// When ctx ends while Verify waits for the KeySet, its error is ctx's.
func (v *Verifier[C]) Verify(ctx context.Context, token string) (C, error) {
	var claims C
	tok, err := josejwt.ParseSigned(token, v.algorithms)
	if err != nil {
		return claims, unauthenticated("the token isn't a compact JWS signed with one of Settings.Algorithms: %v", err)
	}
	kid := tok.Headers[0].KeyID
	if kid == "" {
		return claims, unauthenticated("the token's header has no kid")
	}
	key, err := v.keys.key(ctx, kid)
	if err != nil {
		return claims, err
	}

	var registered josejwt.Claims
	if err := tok.Claims(key, &registered, &claims); err != nil {
		var zero C
		return zero, unauthenticated("the token's signature or claims: %v", err)
	}
	if registered.Expiry == nil {
		var zero C
		return zero, unauthenticated("the token has no exp")
	}
	expected := josejwt.Expected{
		Issuer:      v.issuer,
		AnyAudience: josejwt.Audience{v.audience},
		Time:        time.Now(),
	}
	if err := registered.ValidateWithLeeway(expected, v.leeway); err != nil {
		var zero C
		return zero, unauthenticated("the token's claims: %v", err)
	}
	return claims, nil
}

// unauthenticated returns an error that wraps auth.ErrUnauthenticated, with
// why the token was rejected.
func unauthenticated(format string, args ...any) error {
	return fmt.Errorf("jwt: %s: %w", fmt.Sprintf(format, args...), auth.ErrUnauthenticated)
}
