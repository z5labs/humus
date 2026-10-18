// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
)

// Classification is how one attempt's result counts, as Settings.Classify
// decides it.
type Classification struct {
	// Retry is whether another attempt follows. retry.Success and
	// retry.Stop end the call. retry.Retry asks for another attempt, which
	// the request, MaxAttempts, the budget and the call's deadline must
	// all allow.
	Retry retry.Outcome

	// RetryAfter is, with retry.Retry, the least time to wait before the
	// next attempt, such as the server's Retry-After, or 0. A RetryAfter
	// longer than BackoffMax isn't waited for: the call ends, and returns
	// the response as it came.
	RetryAfter time.Duration

	// Breaker is how the result counts in the dependency's circuit
	// breaker: circuit.Success, circuit.Failure, or circuit.Ignored for a
	// result that says nothing about the dependency.
	Breaker circuit.Outcome
}

// defaultClassify is DefaultSettings' Classify. It reads a response's
// status and its Retry-After header, and never its body:
//
//   - a status below 500, other than 408 and 429: Success, Success;
//   - 408: Retry, Success;
//   - 429: Retry after Retry-After, Success;
//   - 500: Stop, Failure;
//   - 502 and 504: Retry, Failure;
//   - 503: Retry after Retry-After, Failure;
//   - any other status: Stop, Success;
//   - a server certificate the client doesn't trust: Stop, Failure;
//   - any other error, AttemptTimeout's included: Retry, Failure.
func defaultClassify(resp *http.Response, err error) Classification {
	if err != nil {
		if untrusted(err) {
			return Classification{Retry: retry.Stop, Breaker: circuit.Failure}
		}
		return Classification{Retry: retry.Retry, Breaker: circuit.Failure}
	}
	switch code := resp.StatusCode; {
	case code == http.StatusRequestTimeout:
		return Classification{Retry: retry.Retry, Breaker: circuit.Success}
	case code == http.StatusTooManyRequests:
		return Classification{Retry: retry.Retry, RetryAfter: retryAfter(resp.Header, time.Now()), Breaker: circuit.Success}
	case code < http.StatusInternalServerError:
		return Classification{Retry: retry.Success, Breaker: circuit.Success}
	case code == http.StatusInternalServerError:
		return Classification{Retry: retry.Stop, Breaker: circuit.Failure}
	case code == http.StatusBadGateway, code == http.StatusGatewayTimeout:
		return Classification{Retry: retry.Retry, Breaker: circuit.Failure}
	case code == http.StatusServiceUnavailable:
		return Classification{Retry: retry.Retry, RetryAfter: retryAfter(resp.Header, time.Now()), Breaker: circuit.Failure}
	default:
		return Classification{Retry: retry.Stop, Breaker: circuit.Success}
	}
}

// untrusted reports whether err is the client refusing the server's
// certificate, which another attempt meets again.
func untrusted(err error) bool {
	var (
		verify   *tls.CertificateVerificationError
		unknown  x509.UnknownAuthorityError
		invalid  x509.CertificateInvalidError
		hostname x509.HostnameError
	)
	return errors.As(err, &verify) || errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &hostname)
}

// retryAfter returns the wait h's Retry-After header asks for, in either
// of RFC 9110's forms: a number of seconds, used as it is, or an HTTP-date,
// measured from now and clamped at 0. A missing or malformed header is 0. A
// number of seconds too large for a time.Duration is the largest one, which
// is longer than any BackoffMax.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if isDigits(v) {
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs > math.MaxInt64/int64(time.Second) {
			return math.MaxInt64
		}
		return time.Duration(secs) * time.Second
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	return max(0, t.Sub(now))
}

// isDigits reports whether s is one or more ASCII digits, RFC 9110's
// delay-seconds.
func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// mustBeValid panics if c is a Classification no classifier may return: a
// zero or unknown Outcome, or retry.Retry with a negative RetryAfter.
func mustBeValid(c Classification) {
	switch c.Retry {
	case retry.Success, retry.Retry, retry.Stop:
	default:
		panic("httpclient: Settings.Classify returned a Classification whose Retry is " + c.Retry.String() + ", not a retry.Outcome")
	}
	switch c.Breaker {
	case circuit.Success, circuit.Failure, circuit.Ignored:
	default:
		panic("httpclient: Settings.Classify returned a Classification whose Breaker is " + c.Breaker.String() + ", not a circuit.Outcome")
	}
	if c.Retry == retry.Retry && c.RetryAfter < 0 {
		panic("httpclient: Settings.Classify returned retry.Retry with a negative RetryAfter, " + c.RetryAfter.String())
	}
}
