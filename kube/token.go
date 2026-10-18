// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// tokenRefresh is how long the Client keeps a token before it calls
// Connection.Token again, as client-go re-reads a token file. It is not a
// setting.
const tokenRefresh = time.Minute

// tokenTransport adds Connection.Token's bearer token to every request that
// doesn't already carry an Authorization header.
type tokenTransport struct {
	token func(ctx context.Context) (string, error)
	next  http.RoundTripper

	// mu is held across a call of token, so requests that arrive while a
	// call is running wait for its token rather than call it again.
	mu     sync.Mutex
	last   string
	held   bool      // whether a call has ever returned a token
	called time.Time // when token was last called, while held
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" {
		return t.next.RoundTrip(req)
	}
	token, err := t.get(req.Context())
	if err != nil {
		closeBody(req)
		return nil, fmt.Errorf("kube: getting the bearer token: %w", err)
	}
	// A RoundTripper must not change its request, so the header is set on
	// a copy.
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+token)
	return t.next.RoundTrip(r)
}

// get returns the token to send. Once a call has returned a token, token is
// called again only when a minute has passed since the last call, whether
// that call succeeded or not, and a call that fails leaves the last token in
// place. Until a call has returned a token, there is none to keep, so each
// request calls it and fails with its error.
func (t *tokenTransport) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.held && now.Sub(t.called) < tokenRefresh {
		return t.last, nil
	}
	token, err := t.token(ctx)
	if err != nil && !t.held {
		return "", err
	}
	t.called = now
	if err != nil {
		// The request goes on with the last token, so the failure is
		// recorded on its span rather than returned.
		trace.SpanFromContext(ctx).RecordError(fmt.Errorf("kube: getting the bearer token, so the last one is sent: %w", err))
		return t.last, nil
	}
	t.last, t.held = token, true
	return t.last, nil
}

// closeBody closes req's body, as a RoundTripper must even when it fails
// without sending the request.
func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}
