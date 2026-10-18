// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
)

// attemptTransport is the layer inside otelhttp, which sees each attempt
// in its span's context. It sets http.request.resend_count on the span of
// an attempt that is not its call's first, passes the attempt to next, and
// classifies its result, once, for the retry layer outside otelhttp.
type attemptTransport struct {
	classify func(resp *http.Response, err error) Classification
	next     http.RoundTripper
}

// RoundTrip implements http.RoundTripper. An error once the call's context
// has ended is what the client decides, and classify isn't asked about it.
// A panic in classify, or a Classification no classifier may return, passes
// to the caller once the attempt's response is closed.
func (t attemptTransport) RoundTrip(r *http.Request) (resp *http.Response, err error) {
	a := attemptFrom(r.Context())
	if a == nil {
		// Only the retry layer sends to this one, so this is a request
		// sent past it, which counts as nothing.
		a = &attempt{call: r.Context(), resend: redirects(r)}
	}
	if a.resend > 0 {
		trace.SpanFromContext(r.Context()).SetAttributes(semconv.HTTPRequestResendCount(a.resend))
	}

	resp, err = t.next.RoundTrip(r)
	if err != nil && ended(a.call) {
		a.class, a.classified = decided(a.call), true
		return nil, err
	}
	if err != nil {
		resp = nil
	}

	classified := false
	defer func() {
		if !classified && resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	c := t.classify(resp, err)
	mustBeValid(c)
	classified = true
	a.class, a.classified = c, true
	return resp, err
}

// ended reports whether call has ended: it is done, or its deadline has
// passed. http.Client's Timeout both sets a deadline on the call's context
// and cancels the request with a timer of its own, so an attempt can fail
// for the deadline a moment before the context reports it.
func ended(call context.Context) bool {
	return call.Err() != nil || pastDeadline(call)
}

// pastDeadline reports whether call has a deadline, and it has passed.
func pastDeadline(call context.Context) bool {
	d, ok := call.Deadline()
	return ok && !time.Now().Before(d)
}

// decided is the Classification of an error once call has ended, which the
// client decides: the call ends, and the breaker counts the call's
// deadline against the dependency, and ignores the caller giving up.
func decided(call context.Context) Classification {
	if errors.Is(call.Err(), context.DeadlineExceeded) || (call.Err() == nil && pastDeadline(call)) {
		return Classification{Retry: retry.Stop, Breaker: circuit.Failure}
	}
	return Classification{Retry: retry.Stop, Breaker: circuit.Ignored}
}

// attemptTimeout is the layer just outside the http.Transport. It cancels
// an attempt's context once d has passed, unless the response's headers
// arrive first, with cause, which wraps context.DeadlineExceeded. Once they
// arrive, the context lives until the response's body is closed.
type attemptTimeout struct {
	d     time.Duration
	cause error
	next  http.RoundTripper
}

// newAttemptTimeout returns the layer that bounds each attempt by d.
func newAttemptTimeout(d time.Duration, next http.RoundTripper) attemptTimeout {
	return attemptTimeout{
		d:     d,
		cause: fmt.Errorf("httpclient: the attempt had no response after %s: %w", d, context.DeadlineExceeded),
		next:  next,
	}
}

// RoundTrip implements http.RoundTripper. An attempt it cancelled fails
// with an error that wraps t.cause, whatever net/http made of the
// cancellation, even if its headers arrived as the timer fired: the
// cancelled context would fail the body's first read.
func (t attemptTimeout) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(r.Context())
	// settled is set once, by whichever comes first: the timer, which then
	// cancels the attempt, or the attempt's end, which then stops it.
	var settled atomic.Bool
	timer := time.AfterFunc(t.d, func() {
		if settled.CompareAndSwap(false, true) {
			cancel(t.cause)
		}
	})
	resp, err := t.next.RoundTrip(r.WithContext(ctx))
	fired := !settled.CompareAndSwap(false, true)
	timer.Stop()
	if err == nil && fired {
		_ = resp.Body.Close()
		resp, err = nil, t.cause
	}
	if err != nil {
		cancel(nil)
		if fired && r.Context().Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("%w: %w", t.cause, err)
		}
		return nil, err
	}
	release := func() { cancel(nil) }
	if rw, ok := resp.Body.(io.ReadWriteCloser); ok && resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body = &releasingReadWriter{ReadWriteCloser: rw, release: release}
	} else {
		resp.Body = &releasingBody{ReadCloser: resp.Body, release: release}
	}
	return resp, nil
}

// releasingBody is a response's body that ends its attempt's context once
// it is closed.
type releasingBody struct {
	io.ReadCloser
	release func()
}

func (b *releasingBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}

// releasingReadWriter is releasingBody for a 101 Switching Protocols
// response, whose body net/http makes writable.
type releasingReadWriter struct {
	io.ReadWriteCloser
	release func()
}

func (b *releasingReadWriter) Close() error {
	defer b.release()
	return b.ReadWriteCloser.Close()
}
