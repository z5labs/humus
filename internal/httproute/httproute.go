// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package httproute carries the route an openapi API finds back up to
// httpserver, which names the request's span after it.
//
// otelhttp names the span, and sets http.route on its metrics, from its own
// request once the handler returns. A handler that sets Request.Pattern sets
// it on the request it was given, so a handler on the way that passes on a
// copy, such as http.MaxBytesHandler, hides it. The request's context is
// shared by every copy, so the route goes back through it instead.
package httproute

import (
	"context"
	"net/http"
	"sync/atomic"
)

type ctxKey struct{}

// Track returns a copy of r whose context carries where Record records a
// route, and a func that returns the route recorded last, or "" when none
// was.
func Track(r *http.Request) (*http.Request, func() string) {
	var found atomic.Pointer[string]
	tracked := r.WithContext(context.WithValue(r.Context(), ctxKey{}, &found))
	return tracked, func() string {
		if p := found.Load(); p != nil {
			return *p
		}
		return ""
	}
}

// Record records pattern as the route of the request ctx is the context of,
// when Track tracks it. A handler may run on after a handler above it has
// returned, as one behind http.TimeoutHandler does, so the route is
// recorded atomically.
func Record(ctx context.Context, pattern string) {
	found, ok := ctx.Value(ctxKey{}).(*atomic.Pointer[string])
	if !ok {
		return
	}
	found.Store(&pattern)
}
