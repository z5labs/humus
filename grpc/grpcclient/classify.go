// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Classification is how one attempt's result counts, as Settings.Classify
// decides it.
type Classification struct {
	// Retry is whether another attempt follows. retry.Success and
	// retry.Stop end the call. retry.Retry asks for another attempt, which
	// MaxAttempts, the budget and the call's deadline must all allow.
	Retry retry.Outcome

	// RetryAfter is, with retry.Retry, the least time to wait before the
	// next attempt, such as the server's pushback, or 0. A RetryAfter
	// longer than BackoffMax isn't waited for: the call ends, and returns
	// the attempt's error.
	RetryAfter time.Duration

	// Breaker is how the result counts in the dependency's circuit
	// breaker: circuit.Success, circuit.Failure, or circuit.Ignored for a
	// result that says nothing about the dependency.
	Breaker circuit.Outcome
}

// pushbackKey is gRFC A6's trailer, in which a server says how long to wait
// before the next attempt, in milliseconds, or, with a negative value, not
// to try again.
const pushbackKey = "grpc-retry-pushback-ms"

// defaultClassify is DefaultSettings' Classify. It reads the attempt's code,
// which status.Code gives, so an error with no gRPC status is Unknown, and
// its trailer's grpc-retry-pushback-ms, and never a status's details:
//
//   - OK: Success, Success;
//   - Unavailable: Retry, after the pushback if there is one, Failure;
//   - ResourceExhausted with a pushback of 0 or more: Retry, after the
//     pushback, Failure;
//   - ResourceExhausted without a pushback: Stop, Failure;
//   - DeadlineExceeded, Internal, Unknown and DataLoss: Stop, Failure;
//   - Canceled: Stop, Ignored;
//   - any other code: Stop, Success.
//
// A pushback that is negative or not a number makes any of these but OK
// Stop: the server asked not to be tried again.
func defaultClassify(_ string, err error, trailer metadata.MD) Classification {
	code := status.Code(err)
	if code == codes.OK {
		return Classification{Retry: retry.Success, Breaker: circuit.Success}
	}

	var c Classification
	after, sent, ok := pushback(trailer)
	switch code {
	case codes.Unavailable:
		c = Classification{Retry: retry.Retry, RetryAfter: after, Breaker: circuit.Failure}
	case codes.ResourceExhausted:
		c = Classification{Retry: retry.Stop, Breaker: circuit.Failure}
		if sent {
			c = Classification{Retry: retry.Retry, RetryAfter: after, Breaker: circuit.Failure}
		}
	case codes.DeadlineExceeded, codes.Internal, codes.Unknown, codes.DataLoss:
		c = Classification{Retry: retry.Stop, Breaker: circuit.Failure}
	case codes.Canceled:
		c = Classification{Retry: retry.Stop, Breaker: circuit.Ignored}
	default:
		c = Classification{Retry: retry.Stop, Breaker: circuit.Success}
	}
	if !ok {
		c.Retry, c.RetryAfter = retry.Stop, 0
	}
	return c
}

// pushback returns the wait trailer's grpc-retry-pushback-ms asks for,
// whether the trailer has one, and whether another attempt may follow at
// all. A trailer without one is 0, false and true. One that is negative,
// not a number, or given more than once is 0, true and false: the server
// asked not to be tried again, or said nothing a client can act on. A
// number of milliseconds too large for a time.Duration is the largest one,
// which is longer than any BackoffMax.
func pushback(trailer metadata.MD) (after time.Duration, sent, ok bool) {
	vs := trailer.Get(pushbackKey)
	switch len(vs) {
	case 0:
		return 0, false, true
	case 1:
	default:
		return 0, true, false
	}
	ms, err := strconv.ParseInt(vs[0], 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange) && ms > 0, ms > math.MaxInt64/int64(time.Millisecond):
		return math.MaxInt64, true, true
	case err != nil, ms < 0:
		return 0, true, false
	}
	return time.Duration(ms) * time.Millisecond, true, true
}

// mustBeValid panics if c is a Classification no classifier may return: a
// zero or unknown Outcome, or retry.Retry with a negative RetryAfter.
func mustBeValid(c Classification) {
	switch c.Retry {
	case retry.Success, retry.Retry, retry.Stop:
	default:
		panic("grpcclient: Settings.Classify returned a Classification whose Retry is " + c.Retry.String() + ", not a retry.Outcome")
	}
	switch c.Breaker {
	case circuit.Success, circuit.Failure, circuit.Ignored:
	default:
		panic("grpcclient: Settings.Classify returned a Classification whose Breaker is " + c.Breaker.String() + ", not a circuit.Outcome")
	}
	if c.Retry == retry.Retry && c.RetryAfter < 0 {
		panic("grpcclient: Settings.Classify returned retry.Retry with a negative RetryAfter, " + c.RetryAfter.String())
	}
}
