// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"net/http"
)

// spanName names an attempt's span after its method, such as GET, as the
// HTTP semantic conventions name a client span that has no URL template.
// A client has none: it sends whatever URL service code built. An empty
// method is GET, as net/http sends it. Any other method names its span as
// it is: service code chooses it, not whoever the service serves, so it
// can't grow the span names without bound.
func spanName(_ string, r *http.Request) string {
	if r.Method == "" {
		return http.MethodGet
	}
	return r.Method
}

// resendsBefore returns how many requests the call r belongs to sent
// before r's first attempt. A call's first request has none. A redirect is a
// resend, as the semantic conventions count one, so a redirect follows every
// request its call sent before it: r.Response is the response that caused
// it, and its Request the attempt that response answered.
func resendsBefore(r *http.Request) int {
	if r.Response == nil {
		return 0
	}
	if prev := r.Response.Request; prev != nil {
		if a := attemptFrom(prev.Context()); a != nil {
			return a.resend + 1
		}
	}
	return redirects(r)
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
