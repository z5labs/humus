// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// result is what the Client passes to retry.Attempt.Done: the attempt's
// error and its Classification, which the classifier given to retry.New
// reads, as retry's doc says a result reaches its classifier, "as an error
// value that the caller builds".
type result struct {
	err   error
	class Classification
}

func (r *result) Error() string {
	return fmt.Sprintf("grpcclient: an attempt classified %s: %v", r.class.Retry, r.err)
}

// newRetrier builds the Client's retry.Retrier from settings, which
// checkSettings has passed: one budget and one schedule, shared by every
// method and every goroutine.
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
			panic(fmt.Sprintf("grpcclient: an attempt's result is a %T", err))
		}
		c := r.class
		if c.Retry == retry.Retry && c.RetryAfter > backoffMax {
			// A server that asks for longer than BackoffMax isn't one a
			// call in flight waits on: the call ends with the attempt's
			// error.
			return retry.Stop, 0
		}
		return c.Retry, c.RetryAfter
	}
	return retry.New(settings.MaxAttempts, schedule, classify, budget)
}

// attempt makes one attempt of a unary call to method with call, through
// the breaker, in the order the package doc's "The circuit breaker" sets
// out: Allow, with a deferred Release; failure injection's seam, inject;
// the call, unless a fault ended the attempt, with the context inject
// returned; the classification, once, with the trailer call wrote; and
// Done, with the result carrying it. A rejected attempt evaluates nothing
// and makes no call, and is the Client's to classify.
func (c *Client) attempt(ctx context.Context, method string, trailer *metadata.MD, call func(context.Context) error) *result {
	t, err := c.breaker.Allow(ctx)
	if err != nil {
		c.metrics.rejected(ctx)
		return c.reject(ctx, method, err)
	}
	defer t.Release()

	callCtx, err := c.inject(ctx, method)
	if err == nil {
		err = call(callCtx)
	}
	r := &result{err: err, class: c.classifyAttempt(ctx, method, err, *trailer)}
	t.Done(r)
	return r
}

// open makes one attempt to open a stream to method, through the breaker.
// One that fails is classified as a unary attempt is, with no trailer,
// since nothing came back, and settles the attempt's ticket at once. A
// stream that opens is the Client's to classify for retry, without asking
// Classify, and takes the ticket, which it settles when it ends, after open
// has returned: so the guard is a flag set once the ticket is handed off,
// not a deferred Release. Between Allow and grpc-go is failure
// injection's seam, inject, as for a unary attempt: an open a fault ended
// fails as one that grpc-go failed does.
func (c *Client) open(ctx context.Context, desc *grpc.StreamDesc, method string, opts []grpc.CallOption) (grpc.ClientStream, *result) {
	t, err := c.breaker.Allow(ctx)
	if err != nil {
		c.metrics.rejected(ctx)
		return nil, c.reject(ctx, method, err)
	}
	handedOff := false
	defer func() {
		if !handedOff {
			t.Release()
		}
	}()

	callCtx, err := c.inject(ctx, method)
	var s grpc.ClientStream
	if err == nil {
		s, err = c.cc.NewStream(callCtx, desc, method, opts...)
	}
	if err != nil {
		r := &result{err: err, class: c.classifyAttempt(ctx, method, err, nil)}
		t.Done(r)
		return nil, r
	}
	ws := c.watch(ctx, desc, method, s, t)
	handedOff = true
	return ws, &result{class: opened}
}

// classifyAttempt is how an attempt's result counts: what the Client
// decides itself, and Settings.Classify the rest. An error once the call's
// context has ended is the Client's, and Classify isn't asked about it. A
// panic in Classify, or a Classification no classifier may return, passes
// to the goroutine that made the call.
func (c *Client) classifyAttempt(ctx context.Context, method string, err error, trailer metadata.MD) Classification {
	if err != nil && ended(ctx) {
		return decided(ctx)
	}
	class := c.classify(method, err, trailer)
	mustBeValid(class)
	return class
}

// ended reports whether call has ended: it is done, or its deadline has
// passed. grpc-go ends an RPC at its deadline with a timer of its own, so
// an attempt can fail for the deadline a moment before the context reports
// it.
func ended(call context.Context) bool {
	return call.Err() != nil || pastDeadline(call)
}

// pastDeadline reports whether call has a deadline, and it has passed.
func pastDeadline(call context.Context) bool {
	d, ok := call.Deadline()
	return ok && !time.Now().Before(d)
}

// decided is the Classification of an error once call has ended, which the
// Client decides: the call ends, and the breaker counts the call's
// deadline against the dependency, Settings.Timeout or a deadline the
// caller set or inherited, and ignores the caller giving up.
func decided(call context.Context) Classification {
	if errors.Is(call.Err(), context.DeadlineExceeded) || (call.Err() == nil && pastDeadline(call)) {
		return Classification{Retry: retry.Stop, Breaker: circuit.Failure}
	}
	return Classification{Retry: retry.Stop, Breaker: circuit.Ignored}
}

// opened is the Classification of a stream that opened, which the Client
// decides without asking Classify: the call is over for retry, which never
// retries an open stream. Only its Retry is read. Its Breaker is never
// counted: the stream takes the attempt's ticket, and the breaker counts a
// stream once, at its end, with the Classification of how it ended.
var opened = Classification{Retry: retry.Success, Breaker: circuit.Success}
