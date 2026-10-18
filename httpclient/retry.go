// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
)

// maxBuffered is how much of a response's body the client reads into
// memory before another attempt replaces the response. A body larger than
// it isn't retried.
const maxBuffered = 64 << 10

// attemptKey is the context key of an attempt's *attempt, which the retry
// layer puts in the attempt's context, for the attempt layer inside
// otelhttp to find.
type attemptKey struct{}

// attempt is one attempt of a call, shared by the retry layer, which makes
// it, and the attempt layer, which classifies its result.
type attempt struct {
	// call is the call's context, which outlives the attempt's.
	call context.Context

	// resend is the attempt's http.request.resend_count: how many requests
	// the call sent before it, its retries and redirects together.
	resend int

	// class is the attempt's Classification, once the attempt layer has
	// made it. classified is false until then.
	class      Classification
	classified bool
}

// attemptFrom returns the attempt in ctx, or nil if there is none.
func attemptFrom(ctx context.Context) *attempt {
	a, _ := ctx.Value(attemptKey{}).(*attempt)
	return a
}

// result is the error the retry layer passes to retry.Attempt.Done: it
// carries the attempt's Classification to the classifier given to
// retry.New.
type result struct {
	class Classification
}

func (r *result) Error() string {
	return fmt.Sprintf("httpclient: an attempt classified %s", r.class.Retry)
}

// newRetrier builds the client's retry.Retrier from settings, which
// checkSettings has passed.
func newRetrier(settings Settings) (*retry.Retrier, error) {
	budget, err := retry.NewBudget(settings.RetryBudget)
	if err != nil {
		return nil, err
	}
	schedule, err := retry.Exponential(settings.BackoffBase, settings.BackoffMax)
	if err != nil {
		return nil, err
	}
	backoffMax := settings.BackoffMax
	classify := func(_ context.Context, err error) (retry.Outcome, time.Duration) {
		var r *result
		if !errors.As(err, &r) {
			panic(fmt.Sprintf("httpclient: an attempt's result is a %T", err))
		}
		c := r.class
		if c.Retry == retry.Retry && c.RetryAfter > backoffMax {
			// A server that asks for longer than BackoffMax isn't one a
			// call in flight waits on: the call ends with the response as
			// it came.
			return retry.Stop, 0
		}
		return c.Retry, c.RetryAfter
	}
	return retry.New(settings.MaxAttempts, schedule, classify, budget)
}

// retryTransport is the client's outermost layer. It refuses every request
// to an origin other than its own, before anything is sent, and retries
// the rest through next with retrier. conns is the http.Transport at the
// bottom of next, which holds the client's connections.
type retryTransport struct {
	origin  origin
	retrier *retry.Retrier
	next    http.RoundTripper
	conns   *http.Transport
}

// RoundTrip implements http.RoundTripper. A refused request's body is
// closed, as RoundTrip must close it even on an error.
func (t *retryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if o := originOf(r.URL); o != t.origin {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, fmt.Errorf("httpclient: request to %s on a client for %s", o, t.origin)
	}

	call := r.Context()
	replayable := safe(r) && (r.Body == nil || r.Body == http.NoBody || r.GetBody != nil)
	resends := resendsBefore(r)

	var (
		resp *http.Response
		err  error
	)
	for at := range t.retrier.Attempts(call) {
		req := r
		if at.Number() > 1 && r.Body != nil && r.Body != http.NoBody {
			body, bodyErr := r.GetBody()
			if bodyErr != nil {
				at.Done(&result{class: Classification{Retry: retry.Stop, Breaker: circuit.Ignored}})
				return nil, fmt.Errorf("httpclient: rewinding the request's body: %w", bodyErr)
			}
			req = r.Clone(call)
			req.Body = body
		}

		a := &attempt{call: call, resend: resends + at.Number() - 1}
		resp, err = t.next.RoundTrip(req.WithContext(context.WithValue(call, attemptKey{}, a)))
		if !a.classified {
			// Nothing inside otelhttp ran, so there is no result to count.
			a.class = Classification{Retry: retry.Stop, Breaker: circuit.Ignored}
		}
		if at.Done(&result{class: a.class}) != retry.Again || !replayable {
			break
		}
		if resp != nil {
			var fits bool
			if resp, fits = buffer(resp); !fits {
				break
			}
		}
	}
	if resp == nil && err != nil && call.Err() != nil && !errors.Is(err, call.Err()) {
		// The call ended during the wait after a failed attempt: the
		// caller sees why it ended, and why the attempt before failed.
		err = fmt.Errorf("%w; the last attempt failed: %w", context.Cause(call), err)
	}
	return resp, err
}

// CloseIdleConnections closes conns's idle connections, so that
// http.Client.CloseIdleConnections reaches them through the layers between,
// which otelhttp's Transport doesn't pass it through.
func (t *retryTransport) CloseIdleConnections() {
	t.conns.CloseIdleConnections()
}

// safe reports whether r may be sent twice: its method is idempotent under
// RFC 9110, or it carries an idempotency key, as net/http's Transport
// decides it, with PUT and DELETE added.
func safe(r *http.Request) bool {
	switch r.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	if _, ok := r.Header["Idempotency-Key"]; ok {
		return true
	}
	_, ok := r.Header["X-Idempotency-Key"]
	return ok
}

// buffer reads resp's body into memory, up to maxBuffered bytes, and closes
// it, before another attempt replaces resp. It returns resp with the bytes
// it read as its body, so that a call whose wait is cut short returns it.
// A body larger than maxBuffered isn't buffered: buffer returns resp with
// its body intact and fits false. A read that fails is buffered as it
// came: the bytes read before it, and then its error.
func buffer(resp *http.Response) (_ *http.Response, fits bool) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBuffered+1))
	if err == nil && len(b) > maxBuffered {
		resp.Body = &prefixedBody{Reader: io.MultiReader(bytes.NewReader(b), resp.Body), Closer: resp.Body}
		return resp, false
	}
	_ = resp.Body.Close()
	var rest io.Reader = bytes.NewReader(b)
	if err != nil {
		rest = io.MultiReader(rest, failedRead{err: err})
	}
	resp.Body = io.NopCloser(rest)
	return resp, true
}

// failedRead is a reader whose every read fails with err.
type failedRead struct{ err error }

func (r failedRead) Read([]byte) (int, error) { return 0, r.err }

// prefixedBody is a body whose first bytes were read already, and closes
// the body they came from.
type prefixedBody struct {
	io.Reader
	io.Closer
}
