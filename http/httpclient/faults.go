// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"net"
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/fault"
)

// stripEvent is the span event of a strip. It is humus's own, so it starts
// with humus.: OpenTelemetry has no convention for failure injection.
const stripEvent = "humus.fit.strip"

// pointOf returns the injection point of the dependency at o:
// http:<host>:<port>, such as http:payments:8443.
func pointOf(o origin) string {
	return "http:" + net.JoinHostPort(o.host, o.port)
}

// injectFaults is the layer just inside the attempt timeout, around the
// call to the http.Transport: the injection layer. Each attempt the breaker
// admits evaluates point with faults, in the attempt's context, whose span
// is otelhttp's. A Delay is waited there, inside AttemptTimeout. An abort,
// or a Delay the attempt's context cuts short, fails the attempt with a
// *fault.InjectedError, and nothing is sent.
//
// Unless forward is set, the layer then strips the scenario from what the
// attempt sends. otelhttp has written the attempt's headers by now, from
// the attempt's context, so the layer clones the request, deletes
// propagator's fields, and injects them again from the stripped context:
// the attempt keeps its span context and every other baggage member.
type injectFaults struct {
	faults     fault.Injector
	point      string
	forward    bool
	propagator propagation.TextMapPropagator
	next       http.RoundTripper
}

// RoundTrip implements http.RoundTripper. An attempt a fault ends closes
// the request's body, as RoundTrip must.
func (t injectFaults) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx := r.Context()
	f := t.faults.Eval(ctx, t.point)
	if err := f.Wait(ctx); err != nil {
		closeBody(r)
		return nil, &fault.InjectedError{Point: t.point, Err: err}
	}
	if f.Abort {
		closeBody(r)
		return nil, &fault.InjectedError{Point: t.point}
	}
	if !t.forward {
		if stripped, carried := t.faults.Strip(ctx); carried {
			trace.SpanFromContext(ctx).AddEvent(stripEvent)
			r = r.Clone(ctx)
			for _, field := range t.propagator.Fields() {
				r.Header.Del(field)
			}
			t.propagator.Inject(stripped, propagation.HeaderCarrier(r.Header))
		}
	}
	return t.next.RoundTrip(r)
}

// closeBody closes r's body, if it has one.
func closeBody(r *http.Request) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
}
