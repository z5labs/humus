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
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
)

// requireUnauthenticated fails unless err wraps auth.ErrUnauthenticated,
// and isn't a *KeySetError.
func requireUnauthenticated(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, auth.ErrUnauthenticated)
	var kse *jwt.KeySetError
	require.False(t, errors.As(err, &kse), "a rejected token is a *KeySetError: %v", err)
}

// requireKeySetError fails unless err is a *KeySetError that wraps cause,
// and doesn't wrap auth.ErrUnauthenticated.
func requireKeySetError(t *testing.T, err, cause error) {
	t.Helper()
	var kse *jwt.KeySetError
	require.True(t, errors.As(err, &kse), "not a *KeySetError: %v", err)
	require.ErrorIs(t, err, cause)
	require.Same(t, cause, kse.Err)
	require.NotErrorIs(t, err, auth.ErrUnauthenticated)
	require.Contains(t, err.Error(), cause.Error())
}

// unsigned returns a token with alg none, kid and claims, and no signature.
func unsigned(t *testing.T, kid string, claims tokenClaims) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"` + kid + `"}`))
	k := newSigner(t, kid)
	signed := k.sign(t, claims) // to borrow its payload
	payload := strings.Split(signed, ".")[1]
	return header + "." + payload + "."
}

// inBubble runs test in a synctest bubble of its own, as a subtest named
// name, with a Verifier whose KeySet holds k1's public key.
func inBubble(t *testing.T, name string, test func(t *testing.T, v *jwt.Verifier[Claims], k1 *signer, keys *keySet)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			k1 := newSigner(t, "k1")
			keys := &keySet{}
			keys.add(k1.public())
			v, _ := newVerifier(t, keys)
			test(t, v, k1, keys)
		})
	})
}

func TestVerify_a_tokens_claims(t *testing.T) {
	t.Parallel()

	inBubble(t, "a good token returns its claims", func(t *testing.T, v *jwt.Verifier[Claims], k1 *signer, _ *keySet) {
		tok := good("alice", 10*time.Minute)
		tok.Scope = "carts:read"
		claims, err := v.Verify(t.Context(), k1.sign(t, tok))
		require.NoError(t, err)
		require.Equal(t, Claims{Subject: "alice", Scope: "carts:read"}, claims)
	})

	inBubble(t, "an aud that holds the audience among others", func(t *testing.T, v *jwt.Verifier[Claims], k1 *signer, _ *keySet) {
		tok := good("alice", 10*time.Minute)
		tok.Audience = josejwt.Audience{"orders", audience}
		_, err := v.Verify(t.Context(), k1.sign(t, tok))
		require.NoError(t, err)
	})

	rejected := map[string]func(t *testing.T, k1 *signer) string{
		"no exp": func(t *testing.T, k1 *signer) string {
			tok := good("alice", time.Hour)
			tok.Expiry = nil
			return k1.sign(t, tok)
		},
		"no kid": func(t *testing.T, k1 *signer) string {
			return (&signer{alg: k1.alg, key: k1.key}).sign(t, good("alice", time.Hour))
		},
		"the wrong iss": func(t *testing.T, k1 *signer) string {
			tok := good("alice", time.Hour)
			tok.Issuer = "https://evil.example.com"
			return k1.sign(t, tok)
		},
		"no iss": func(t *testing.T, k1 *signer) string {
			tok := good("alice", time.Hour)
			tok.Issuer = ""
			return k1.sign(t, tok)
		},
		"the wrong aud": func(t *testing.T, k1 *signer) string {
			tok := good("alice", time.Hour)
			tok.Audience = josejwt.Audience{"orders"}
			return k1.sign(t, tok)
		},
		"a future nbf": func(t *testing.T, k1 *signer) string {
			tok := good("alice", time.Hour)
			tok.NotBefore = josejwt.NewNumericDate(time.Now().Add(2 * time.Minute))
			return k1.sign(t, tok)
		},
		"a future iat": func(t *testing.T, k1 *signer) string {
			tok := good("alice", time.Hour)
			tok.IssuedAt = josejwt.NewNumericDate(time.Now().Add(2 * time.Minute))
			return k1.sign(t, tok)
		},
		"alg none": func(t *testing.T, _ *signer) string {
			return unsigned(t, "k1", good("alice", time.Hour))
		},
		"a signature by another key with the same kid": func(t *testing.T, _ *signer) string {
			return newSigner(t, "k1").sign(t, good("alice", time.Hour))
		},
		"a claim of the wrong type for C": func(t *testing.T, k1 *signer) string {
			tok := good("alice", time.Hour)
			return k1.sign(t, map[string]any{
				"iss": tok.Issuer, "aud": audience, "exp": tok.Expiry, "scope": 42,
			})
		},
		"not a JWS at all": func(*testing.T, *signer) string {
			return "alice"
		},
		"HMAC": func(t *testing.T, _ *signer) string {
			sig, err := jose.NewSigner(jose.SigningKey{
				Algorithm: jose.HS256,
				Key:       jose.JSONWebKey{Key: []byte(rand.Text() + rand.Text()), KeyID: "k1"},
			}, nil)
			require.NoError(t, err)
			raw, err := josejwt.Signed(sig).Claims(good("alice", time.Hour)).Serialize()
			require.NoError(t, err)
			return raw
		},
	}
	for name, tok := range rejected {
		inBubble(t, name+" is rejected", func(t *testing.T, v *jwt.Verifier[Claims], k1 *signer, _ *keySet) {
			_, err := v.Verify(t.Context(), tok(t, k1))
			requireUnauthenticated(t, err)
		})
	}

	inBubble(t, "an algorithm not in Algorithms is rejected", func(t *testing.T, v *jwt.Verifier[Claims], _ *signer, keys *keySet) {
		key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		require.NoError(t, err)
		k384 := &signer{kid: "k384", alg: jose.ES384, key: key}
		keys.add(k384.public())
		_, err = v.Verify(t.Context(), k384.sign(t, good("alice", time.Hour)))
		requireUnauthenticated(t, err)
	})

	inBubble(t, "expiry is checked within Leeway of now", func(t *testing.T, v *jwt.Verifier[Claims], k1 *signer, _ *keySet) {
		tok := k1.sign(t, good("alice", 10*time.Minute))
		time.Sleep(10*time.Minute + 30*time.Second) // past exp, within the 1m leeway
		_, err := v.Verify(t.Context(), tok)
		require.NoError(t, err)
		time.Sleep(time.Minute) // past the leeway
		_, err = v.Verify(t.Context(), tok)
		requireUnauthenticated(t, err)
	})

	inBubble(t, "a future nbf within Leeway is accepted", func(t *testing.T, v *jwt.Verifier[Claims], k1 *signer, _ *keySet) {
		tok := good("alice", time.Hour)
		tok.NotBefore = josejwt.NewNumericDate(time.Now().Add(30 * time.Second))
		_, err := v.Verify(t.Context(), k1.sign(t, tok))
		require.NoError(t, err)
	})
}

func TestVerify_gets_keys_when_it_needs_them(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		k1, k2, k3 := newSigner(t, "k1"), newSigner(t, "k2"), newSigner(t, "k3")
		keys := &keySet{}
		keys.add(k1.public())
		v, tel := newVerifier(t, keys)
		ctx := t.Context()
		require.Zero(t, keys.count())

		_, err := v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
		require.NoError(t, err)
		require.Equal(t, 1, keys.count(), "the first token calls the KeySet")
		_, err = v.Verify(ctx, k1.sign(t, good("bob", time.Hour)))
		require.NoError(t, err)
		require.Equal(t, 1, keys.count(), "a second token doesn't")

		time.Sleep(time.Minute)
		keys.add(k2.public()) // rotated in
		_, err = v.Verify(ctx, k2.sign(t, good("alice", time.Hour)))
		require.NoError(t, err)
		require.Equal(t, 2, keys.count(), "a rotated-in kid calls it again")

		keys.add(k3.public())
		_, err = v.Verify(ctx, k3.sign(t, good("alice", time.Hour)))
		requireUnauthenticated(t, err)
		require.Equal(t, 2, keys.count(), "a second unknown kid within a minute isn't called for")

		time.Sleep(time.Minute)
		_, err = v.Verify(ctx, k3.sign(t, good("alice", time.Hour)))
		require.NoError(t, err)
		require.Equal(t, 3, keys.count(), "after the minute, it is called and served")

		_, err = v.Verify(ctx, newSigner(t, "made-up").sign(t, good("mallory", time.Hour)))
		requireUnauthenticated(t, err)
		require.Equal(t, 3, keys.count(), "a made-up kid within the minute")
		time.Sleep(time.Minute)
		_, err = v.Verify(ctx, newSigner(t, "made-up").sign(t, good("mallory", time.Hour)))
		requireUnauthenticated(t, err)
		require.Equal(t, 4, keys.count(), "a made-up kid that a successful call lacks is a 401")

		time.Sleep(time.Hour + time.Second) // past MaxKeyAge
		_, err = v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
		require.NoError(t, err)
		require.Equal(t, 5, keys.count(), "keys past MaxKeyAge are got again")

		require.Empty(t, tel.Logs(), "nothing failed, so nothing is logged")
	})
}

func TestVerify_drops_a_key_withdrawn_past_MaxKeyAge(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		k1, k2 := newSigner(t, "k1"), newSigner(t, "k2")
		keys := &keySet{}
		keys.add(k1.public())
		v, _ := newVerifier(t, keys)
		ctx := t.Context()

		_, err := v.Verify(ctx, k1.sign(t, good("alice", 2*time.Hour)))
		require.NoError(t, err)

		keys.mu.Lock()
		keys.keys = []jwt.Key{k2.public()} // k1 withdrawn
		keys.mu.Unlock()
		time.Sleep(30 * time.Minute)
		_, err = v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
		require.NoError(t, err, "within MaxKeyAge, k1 is still held")

		time.Sleep(31 * time.Minute)
		_, err = v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
		requireUnauthenticated(t, err)
		require.Equal(t, 2, keys.count())
	})
}

func TestVerify_shares_one_call(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		k1 := newSigner(t, "k1")
		keys := &keySet{}
		keys.add(k1.public())
		release := keys.hold()
		v, _ := newVerifier(t, keys)

		const n = 10
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				_, errs[i] = v.Verify(t.Context(), k1.sign(t, good("alice", time.Hour)))
			})
		}
		synctest.Wait() // every token is waiting on the one call
		require.Equal(t, 1, keys.count())
		release()
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		require.Equal(t, 1, keys.count(), "concurrent tokens share one call")
	})
}

// ctxKeySet is a KeySet that records the context of each call, and blocks
// until released.
type ctxKeySet struct {
	keySet
	ctxs chan context.Context
}

func (k *ctxKeySet) Keys(ctx context.Context) ([]jwt.Key, error) {
	k.ctxs <- ctx
	return k.keySet.Keys(ctx)
}

type ctxValueKey struct{}

func TestVerify_stops_waiting_when_its_request_ends(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		k1 := newSigner(t, "k1")
		keys := &ctxKeySet{ctxs: make(chan context.Context, 1)}
		keys.add(k1.public())
		release := keys.hold()
		v, _ := newVerifier(t, keys)
		tok := k1.sign(t, good("alice", time.Hour))

		ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxValueKey{}, "the request's"))
		gone := make(chan error, 1)
		go func() {
			_, err := v.Verify(ctx, tok)
			gone <- err
		}()
		callCtx := <-keys.ctxs
		require.Equal(t, "the request's", callCtx.Value(ctxValueKey{}), "the call carries the request's values")

		served := make(chan error, 1)
		go func() {
			_, err := v.Verify(t.Context(), tok)
			served <- err
		}()
		synctest.Wait()

		cancel() // the first client goes away
		err := <-gone
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, auth.ErrUnauthenticated)
		require.NoError(t, callCtx.Err(), "the call isn't canceled with the request")

		release()
		require.NoError(t, <-served, "the requests waiting on the call are served")
		require.Equal(t, 1, keys.count())
	})
}

func TestVerify_keeps_stale_keys_when_a_call_fails(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		k1 := newSigner(t, "k1")
		keys := &keySet{}
		keys.add(k1.public())
		v, tel := newVerifier(t, keys)
		ctx := t.Context()

		_, err := v.Verify(ctx, k1.sign(t, good("alice", 3*time.Hour)))
		require.NoError(t, err)

		down := errors.New("the secret store is down")
		keys.fail(down)
		time.Sleep(time.Hour + time.Second) // past MaxKeyAge
		_, err = v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
		require.NoError(t, err, "stale keys outlive a failed call")
		require.Equal(t, 2, keys.count())
		_, err = v.Verify(ctx, k1.sign(t, good("bob", time.Hour)))
		require.NoError(t, err)
		require.Equal(t, 2, keys.count(), "within the interval, the KeySet isn't called again")

		logs := tel.Logs()
		require.Equal(t, []string{"WARN keys refresh failed"}, bodies(logs))
		attrs := map[string]string{}
		logs[0].WalkAttributes(func(kv attribute.KeyValue) bool {
			attrs[string(kv.Key)] = kv.Value.AsString()
			return true
		})
		require.Equal(t, map[string]string{
			"error.type":        "*errors.errorString",
			"exception.message": "the secret store is down",
		}, attrs)
		require.Equal(t, "github.com/z5labs/humus/auth/jwt", logs[0].InstrumentationScope().Name)

		keys.fail(nil)
		time.Sleep(time.Minute)
		_, err = v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
		require.NoError(t, err)
		require.Equal(t, 3, keys.count(), "once the interval passes, the stale keys are got again")
		_, err = v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
		require.NoError(t, err)
		require.Equal(t, 3, keys.count(), "and are fresh")
		require.Len(t, tel.Logs(), 1)
	})
}

func TestVerify_KeySetError(t *testing.T) {
	t.Parallel()

	t.Run("with no keys held and a failing KeySet", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			k1 := newSigner(t, "k1")
			down := errors.New("the secret store is down")
			keys := &keySet{err: down}
			keys.add(k1.public())
			v, tel := newVerifier(t, keys)
			ctx := t.Context()

			_, err := v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
			requireKeySetError(t, err, down)
			require.Equal(t, "jwt: getting keys from the KeySet: the secret store is down", err.Error())
			require.Equal(t, 1, keys.count())

			time.Sleep(30 * time.Second)
			keys.fail(nil)
			_, err = v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
			requireKeySetError(t, err, down)
			require.Equal(t, 1, keys.count(), "within the interval, it fails at once with the same error")

			time.Sleep(30 * time.Second)
			_, err = v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
			require.NoError(t, err, "once the interval passes and the KeySet is back, it is served")
			require.Equal(t, 2, keys.count())

			require.Empty(t, tel.Logs(), "with no keys held, nothing is logged: the error is Verify's")
		})
	})

	t.Run("an unknown kid while the KeySet fails", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			k1, k2 := newSigner(t, "k1"), newSigner(t, "k2")
			keys := &keySet{}
			keys.add(k1.public())
			v, _ := newVerifier(t, keys)
			ctx := t.Context()

			_, err := v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
			require.NoError(t, err)

			down := errors.New("the secret store is down")
			keys.fail(down)
			keys.add(k2.public())
			time.Sleep(time.Minute)
			_, err = v.Verify(ctx, k2.sign(t, good("alice", time.Hour)))
			requireKeySetError(t, err, down)
			require.Equal(t, 2, keys.count())

			_, err = v.Verify(ctx, k2.sign(t, good("alice", time.Hour)))
			requireKeySetError(t, err, down)
			require.Equal(t, 2, keys.count(), "the last call failed within the interval")

			_, err = v.Verify(ctx, k1.sign(t, good("alice", time.Hour)))
			require.NoError(t, err, "a held kid is still served")

			keys.fail(nil)
			keys.mu.Lock()
			keys.keys = []jwt.Key{k1.public()} // k2 isn't in the set after all
			keys.mu.Unlock()
			time.Sleep(time.Minute)
			_, err = v.Verify(ctx, k2.sign(t, good("alice", time.Hour)))
			requireUnauthenticated(t, err)
			require.Equal(t, 3, keys.count(), "the same kid after a successful call that lacks it is a 401")
		})
	})

	t.Run("a KeySet that panics", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			v, _ := newVerifier(t, panicking{})
			_, err := v.Verify(t.Context(), newSigner(t, "k1").sign(t, good("alice", time.Hour)))
			var kse *jwt.KeySetError
			require.ErrorAs(t, err, &kse)
			require.ErrorContains(t, err, "the KeySet panicked: no keys today")
			require.NotErrorIs(t, err, auth.ErrUnauthenticated)
		})
	})
}

// panicking is a KeySet that panics.
type panicking struct{}

func (panicking) Keys(context.Context) ([]jwt.Key, error) { panic("no keys today") }

func TestVerify_skips_unusable_keys(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ec := newSigner(t, "ec")
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		rs := &signer{kid: "rsa", alg: jose.PS256, key: rsaKey}
		_, edKey, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		ed := &signer{kid: "ed", alg: jose.EdDSA, key: edKey}
		private := newSigner(t, "private")
		noID := newSigner(t, "")

		keys := &keySet{}
		keys.add(jwt.Key{ID: "", Public: noID.key.Public()})               // no ID
		keys.add(jwt.Key{ID: "private", Public: private.key})              // a private key
		keys.add(jwt.Key{ID: "secret", Public: []byte(rand.Text())})       // an unsupported type
		keys.add(jwt.Key{ID: "nil", Public: (*rsa.PublicKey)(nil)})        // no key at all
		keys.add(jwt.Key{ID: "short", Public: ed25519.PublicKey{1, 2, 3}}) // not an Ed25519 key
		keys.add(ec.public())
		keys.add(rs.public())
		keys.add(ed.public())
		v, _ := newVerifier(t, keys)
		ctx := t.Context()

		for _, s := range []*signer{ec, rs, ed} {
			claims, err := v.Verify(ctx, s.sign(t, good(s.kid, time.Hour)))
			require.NoError(t, err, s.kid)
			require.Equal(t, s.kid, claims.Subject)
		}
		_, err = v.Verify(ctx, private.sign(t, good("alice", time.Hour)))
		requireUnauthenticated(t, err)
		require.Equal(t, 1, keys.count())
	})
}
