// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/fault"
)

// stripEvent is the span event of a strip. It is humus's own, so it starts
// with humus.: OpenTelemetry has no convention for failure injection.
const stripEvent = "humus.fit.strip"

// stripScenarios is the layer between recordPanics and the end user's
// handler. otelhttp has extracted the request's baggage by the time it
// runs, and none of the end user's code has run yet.
//
// It strips the failure scenario a request carries, with the Injector of
// the Setup the Service is built with, unless trust says the caller is
// trusted. trust is asked only about a request that carries a scenario, and
// only when the App has failure injection, since the zero Injector strips
// nothing. A nil trust trusts no caller. Every other baggage member is kept,
// and each strip adds a humus.fit.strip event to the request's span.
//
// It runs inside recordPanics, so a panic of trust is recorded and fails
// the request, as a panic of the handler does.
type stripScenarios struct {
	next   http.Handler
	faults fault.Injector
	trust  func(r *http.Request) bool
}

func (h stripScenarios) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, carried := h.faults.Strip(r.Context())
	if carried && (h.trust == nil || !h.trust(r)) {
		trace.SpanFromContext(ctx).AddEvent(stripEvent)
		r = r.WithContext(ctx)
	}
	h.next.ServeHTTP(w, r)
}
