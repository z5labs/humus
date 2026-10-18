// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"net/http"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// spanName names an attempt's span after its method, such as GET, as the
// HTTP semantic conventions name a client span that has no URL template.
// A client has none: it sends whatever URL service code built.
func spanName(_ string, r *http.Request) string {
	return r.Method
}

// resendCount sets http.request.resend_count on each attempt's span, which
// otelhttp, outside it, has put in the attempt's context, then passes the
// attempt to next. The first attempt of a call is not a resend, so its span
// has no count. A redirect is a resend, as the semantic conventions count
// one: the first redirect's span has 1.
type resendCount struct {
	next http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t resendCount) RoundTrip(r *http.Request) (*http.Response, error) {
	if n := redirects(r); n > 0 {
		trace.SpanFromContext(r.Context()).SetAttributes(semconv.HTTPRequestResendCount(n))
	}
	return t.next.RoundTrip(r)
}

// redirects returns how many redirects led to r. http.Client sets a
// redirect's Response to the response that caused it, whose Request is the
// request before, and so on back to the call's first request, which has no
// Response.
func redirects(r *http.Request) int {
	n := 0
	for resp := r.Response; resp != nil; {
		n++
		if resp.Request == nil {
			break
		}
		resp = resp.Request.Response
	}
	return n
}
