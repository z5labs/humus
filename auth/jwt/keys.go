// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Key is a public key, which verifies the tokens whose header's kid is ID.
type Key struct {
	ID string

	// Public is an *rsa.PublicKey, an *ecdsa.PublicKey or an
	// ed25519.PublicKey.
	Public crypto.PublicKey
}

// KeySet gets the keys tokens are signed with, from wherever the end user
// keeps them. The Verifier holds the keys Keys returns, and calls it only
// when it needs keys: at most once per MinRefreshInterval, with concurrent
// requests sharing one call. So a KeySet needs no cache of its own.
//
// Keys is called on a goroutine of the Verifier's, with a context that
// carries the request's values, such as its trace, and is never canceled:
// a KeySet bounds its own calls. It returns every key it has, not only the
// one a token named. A key whose ID is empty, or whose Public isn't one of
// the three public key types, is skipped, and the others are kept.
type KeySet interface {
	Keys(ctx context.Context) ([]Key, error)
}

// KeySetError is Verify's error when it had no key to verify a token with
// because its KeySet failed. Err is the KeySet's error. It doesn't wrap
// auth.ErrUnauthenticated, so it is the server's to answer, such as with
// openapi.Error[*jwt.KeySetError] at 503.
type KeySetError struct {
	Err error
}

// Error returns the KeySet's error, after what failed.
func (e *KeySetError) Error() string {
	return "jwt: getting keys from the KeySet: " + e.Err.Error()
}

// Unwrap returns the KeySet's error.
func (e *KeySetError) Unwrap() error {
	return e.Err
}

// call is one call of the KeySet, which concurrent requests share. done is
// closed once the call's result is in the keyring.
type call struct {
	done chan struct{}
}

// keyring holds the keys a KeySet returned, and decides when to call it
// again: with the first token, when a token names a kid it lacks, and when
// its keys are older than maxAge, but at most once per minInterval.
type keyring struct {
	keySet      KeySet
	minInterval time.Duration
	maxAge      time.Duration
	logger      log.Logger

	mu       sync.Mutex
	held     map[string]crypto.PublicKey // by kid; nil until a call succeeds
	gotAt    time.Time                   // when the held keys were got
	calledAt time.Time                   // when the last call started; zero before the first
	lastErr  error                       // the last call's error, nil if it succeeded
	inflight *call                       // the call running, if any
}

// key returns the public key whose ID is kid. It calls the KeySet when it
// needs to, or waits for the call running, and stops waiting when ctx ends.
//
// Its error wraps auth.ErrUnauthenticated when the keys lack kid after a
// call that succeeded, or within minInterval of one. It is a *KeySetError
// when the keys lack kid and the last call failed: whether this request's
// call or one within minInterval. It is ctx's error when ctx ended first.
func (r *keyring) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	waited := false
	for {
		r.mu.Lock()
		if c := r.inflight; c != nil {
			r.mu.Unlock()
			if err := wait(ctx, c); err != nil {
				return nil, err
			}
			waited = true
			continue
		}

		now := time.Now()
		key, held := r.held[kid]
		if held && now.Sub(r.gotAt) <= r.maxAge {
			r.mu.Unlock()
			return key, nil
		}
		// Once a request has waited for a call, it is answered by that
		// call's result, whatever the call was for: it never makes another.
		if !waited && (r.calledAt.IsZero() || now.Sub(r.calledAt) >= r.minInterval) {
			c := r.start(ctx, now)
			r.mu.Unlock()
			if err := wait(ctx, c); err != nil {
				return nil, err
			}
			waited = true
			continue
		}
		lastErr := r.lastErr
		r.mu.Unlock()

		switch {
		case held:
			// The keys are past maxAge, and the call to replace them
			// failed: stale keys over none.
			return key, nil
		case lastErr != nil:
			return nil, &KeySetError{Err: lastErr}
		default:
			return nil, unauthenticated("no key has the token's kid")
		}
	}
}

// start starts a call of the KeySet, with a context that carries ctx's
// values but is never canceled, so a request that goes away doesn't fail
// the call for the requests waiting on it. r.mu must be held.
func (r *keyring) start(ctx context.Context, now time.Time) *call {
	c := &call{done: make(chan struct{})}
	r.inflight = c
	r.calledAt = now
	go r.run(context.WithoutCancel(ctx), c)
	return c
}

// run calls the KeySet, puts the result in the keyring, and ends c. A call
// that fails while keys are held logs one WARN record, "keys refresh
// failed", and keeps them.
func (r *keyring) run(ctx context.Context, c *call) {
	keys, err := r.keys(ctx)

	r.mu.Lock()
	logFailure := err != nil && r.held != nil
	if err != nil {
		r.lastErr = err
	} else {
		r.held = usable(keys)
		r.gotAt = time.Now()
		r.lastErr = nil
	}
	r.inflight = nil
	r.mu.Unlock()

	// The record is emitted before the waiting requests go on, so it is
	// in the log by the time any of them is answered.
	if logFailure {
		r.warn(ctx, err)
	}
	close(c.done)
}

// keys calls the KeySet. A panic in it is the call's error, since it runs
// on a goroutine of the Verifier's, where nothing would recover it.
func (r *keyring) keys(ctx context.Context) (keys []Key, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("the KeySet panicked: %v", v)
		}
	}()
	return r.keySet.Keys(ctx)
}

// warn logs one WARN record, "keys refresh failed", for err, in ctx, which
// carries the request's trace.
func (r *keyring) warn(ctx context.Context, err error) {
	var rec log.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(log.SeverityWarn)
	rec.SetSeverityText(log.SeverityWarn.String())
	rec.SetBody(attribute.StringValue("keys refresh failed"))
	rec.AddAttributes(
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
	)
	r.logger.Emit(ctx, rec)
}

// wait waits for c to end, or for ctx to.
func wait(ctx context.Context, c *call) error {
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("jwt: waiting for the KeySet: %w", ctx.Err())
	}
}

// usable returns the keys that can verify a token, by ID. A key whose ID is
// empty, or whose Public isn't an RSA, ECDSA or Ed25519 public key, a
// private key among them, is skipped. Of two keys with one ID, the first is
// kept. The map is never nil, so an empty set still counts as got.
func usable(keys []Key) map[string]crypto.PublicKey {
	held := make(map[string]crypto.PublicKey, len(keys))
	for _, k := range keys {
		if k.ID == "" || !public(k.Public) {
			continue
		}
		if _, ok := held[k.ID]; !ok {
			held[k.ID] = k.Public
		}
	}
	return held
}

// public reports whether k is a public key a token can be verified with.
func public(k crypto.PublicKey) bool {
	switch k := k.(type) {
	case *rsa.PublicKey:
		return k != nil && k.N != nil
	case *ecdsa.PublicKey:
		return k != nil && k.Curve != nil
	case ed25519.PublicKey:
		return len(k) == ed25519.PublicKeySize
	default:
		return false
	}
}
