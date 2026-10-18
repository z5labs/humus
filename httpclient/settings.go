// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
)

// Settings configures a client of one dependency. Every field is a plain
// value, and a field's zero value means zero, never "use the default": start
// from DefaultSettings and change the fields you source. BaseURL has no
// default, so it is always among them.
type Settings struct {
	// BaseURL is the dependency. Its scheme, host and port are the origin
	// the client sends requests to, and the only one. Its path, if any, is
	// for service code to join request paths onto; the client doesn't use it.
	//
	// Default: none. Valid: an absolute http or https URL with a host, and
	// no user info, query or fragment. Check: CheckBaseURL.
	BaseURL *url.URL

	// TLS is the config the client dials the dependency with: the roots it
	// trusts in RootCAs, and for mTLS, a client certificate in
	// Certificates. nil means plain text. An https BaseURL requires a
	// config, and an http one requires nil. A config with no RootCAs trusts
	// the system's roots.
	//
	// Default: nil, which means plain text. Valid: nil, or a config that
	// verifies the server's certificate and allows TLS 1.2 or later.
	// Check: CheckTLS.
	TLS *tls.Config

	// Timeout bounds a whole call: every attempt, the waits between them,
	// and reading the response's body. A deadline the caller's context has
	// already, if it is earlier, wins. It is the http.Client's Timeout.
	//
	// Default: 10s. Valid: positive. Check: CheckTimeout.
	Timeout time.Duration

	// AttemptTimeout bounds one attempt, from dialling to the response's
	// headers. An attempt that runs past it fails with an error that wraps
	// context.DeadlineExceeded, and is retried as any failed attempt is.
	//
	// Default: 5s. Valid: positive. Check: CheckAttemptTimeout.
	AttemptTimeout time.Duration

	// MaxAttempts bounds the attempts of each request a call sends, the
	// first included. A redirect is a request of its own. 1 turns retries
	// off.
	//
	// Default: 3. Valid: 1 or more. Check: CheckMaxAttempts.
	MaxAttempts int

	// BackoffBase and BackoffMax are retry.Exponential's arguments: the wait
	// after attempt n is drawn from [0, min(BackoffMax, BackoffBase·2ⁿ⁻¹)).
	// A Retry-After longer than BackoffMax isn't waited for.
	//
	// Default: 100ms and 5s. Valid: positive, and BackoffMax at least
	// BackoffBase. Check: CheckBackoffBase, CheckBackoffMax.
	BackoffBase time.Duration
	BackoffMax  time.Duration

	// RetryBudget limits the retries of every call to the dependency
	// together, as retry.Budget describes.
	//
	// Default: {MaxTokens: 10, TokenRatio: 0.1}. Valid: MaxTokens from 3 to
	// retry.TokenLimit; TokenRatio from 0.001 to retry.TokenLimit. Check:
	// CheckRetryBudgetMaxTokens, CheckRetryBudgetTokenRatio.
	RetryBudget retry.BudgetConfig

	// Classify decides how each attempt's result counts: whether another
	// attempt follows, and whether the result counts against the
	// dependency. It is set in code, not sourced, and one function may be
	// set on every client whose dependencies agree on what their answers
	// mean.
	//
	// Classify gets the attempt's response, with its status and headers
	// and its body unread, or the error the attempt failed with, never
	// both. resp.Request is the request the attempt sent. It isn't asked
	// about what the client decides itself: an error once the call's
	// context has ended, and a body that fails once service code reads it.
	//
	// Classify may read resp.Body, to classify what the dependency sent,
	// such as a 200 that carries an error. It then closes the body it read,
	// and leaves in resp.Body what service code is to read, such as the
	// bytes it read.
	//
	// It is called on the goroutine that made the call, once for each
	// attempt, so it must be safe for concurrent use. A Classification
	// with a zero Outcome, or with retry.Retry and a negative RetryAfter,
	// panics, as bedrock's classifiers do.
	//
	// Default: the classification the package doc's "How an attempt's
	// result counts" describes. Valid: set, which NewClient checks.
	Classify func(resp *http.Response, err error) Classification

	// Breaker configures the circuit breaker in front of the dependency,
	// which counts the Breaker outcome of each attempt's Classification.
	//
	// Default: circuit.Config{Window: 20, Probes: 1, OpenFor: 10 *
	// time.Second, ProbeTimeout: 10 * time.Second}. Valid: Window and
	// Probes from 1 to circuit.MaxCount, OpenFor and ProbeTimeout positive.
	// Checks: CheckBreakerWindow, CheckBreakerProbes, CheckBreakerOpenFor,
	// CheckBreakerProbeTimeout.
	Breaker circuit.Config

	// BreakerPolicy decides when the breaker opens and closes. It is set in
	// code, not sourced: how the dependency fails is the end user's
	// knowledge, not an operator's choice. A policy whose Trip always
	// returns false turns the breaker off.
	//
	// It runs with the breaker locked, on the goroutine of the call whose
	// result it decides on, so it must be fast and safe for concurrent use.
	// A panic in it passes to that goroutine, and leaves the breaker
	// consistent.
	//
	// Default: open after 5 failures in a row, or once half of the last 20
	// or more results failed; while half-open, reopen on a failed probe and
	// close after 3 successful ones. Valid: Trip and Probe set, which
	// NewClient checks. Check: none.
	BreakerPolicy circuit.Policy

	// ForwardScenario sends the failure scenario a call carries on to the
	// dependency, for a dependency that takes part in failure injection
	// with the same codec. When it is false, the scenario is stripped from
	// what the client sends, and the rest of the call's baggage still goes.
	//
	// Default: false. Valid: either. Check: none.
	ForwardScenario bool
}

// DefaultSettings returns the client's defaults. BaseURL has none, so its
// BaseURL is nil, which CheckBaseURL rejects: the end user always sources it.
func DefaultSettings() Settings {
	return Settings{
		BaseURL:        nil,
		TLS:            nil,
		Timeout:        10 * time.Second,
		AttemptTimeout: 5 * time.Second,
		MaxAttempts:    3,
		BackoffBase:    100 * time.Millisecond,
		BackoffMax:     5 * time.Second,
		RetryBudget:    retry.BudgetConfig{MaxTokens: 10, TokenRatio: 0.1},
		Classify:       defaultClassify,
		Breaker: circuit.Config{
			Window:       20,
			Probes:       1,
			OpenFor:      10 * time.Second,
			ProbeTimeout: 10 * time.Second,
		},
		BreakerPolicy: circuit.Policy{
			Trip:  defaultTrip,
			Probe: defaultProbe,
		},
		ForwardScenario: false,
	}
}

// defaultTrip opens the breaker after 5 failures in a row, or once half of
// the last 20 or more results failed.
func defaultTrip(c circuit.Counts) bool {
	n := c.Successes + c.Failures
	return c.ConsecutiveFailures >= 5 || (n >= 20 && 2*c.Failures >= n)
}

// defaultProbe reopens the breaker on a failed probe, closes it after 3
// successful ones, and keeps probing otherwise, including after a probe
// times out.
func defaultProbe(c circuit.Counts) circuit.State {
	switch {
	case c.Failures > 0:
		return circuit.Open
	case c.ConsecutiveSuccesses >= 3:
		return circuit.Closed
	default:
		return circuit.HalfOpen
	}
}

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errNil         = errors.New("must not be nil")
	errNotPositive = errors.New("must be positive")
)

// CheckBaseURL reports why u can't be a client's dependency, or nil if it
// can. u must not be nil, and must be an absolute http or https URL with a
// host. It must have no user info, since credentials in a URL would sit in
// a setting and in every error that names the client's origin, and no query
// or fragment, since the client sends requests to many paths of one origin.
// No error quotes u, so a password in it is never repeated.
func CheckBaseURL(u *url.URL) error {
	switch {
	case u == nil:
		return errNil
	case u.Scheme == "":
		return errors.New("must be absolute, with the scheme http or https")
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("has the scheme %q, not http or https", u.Scheme)
	case u.User != nil:
		return errors.New("has user info: set credentials in a request's header instead")
	case u.Hostname() == "":
		return errors.New("has no host")
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("has a query")
	case u.Fragment != "":
		return errors.New("has a fragment")
	}
	return nil
}

// CheckTLS reports why c can't dial a dependency, or nil if it can. nil is
// valid, and means plain text. A config must verify the server's
// certificate, so InsecureSkipVerify is rejected: a test trusts its CA
// through RootCAs instead. And it must allow TLS 1.2 or later: a MaxVersion
// of TLS 1.2 or later, or none.
func CheckTLS(c *tls.Config) error {
	if c == nil {
		return nil
	}
	if c.InsecureSkipVerify {
		return errors.New("sets InsecureSkipVerify, so it doesn't verify the server's certificate")
	}
	if c.MaxVersion != 0 && c.MaxVersion < tls.VersionTLS12 {
		return fmt.Errorf("has a MaxVersion of %s, below TLS 1.2", tls.VersionName(c.MaxVersion))
	}
	return nil
}

// CheckTimeout rejects a duration that is not positive: a client with no
// bound on a call holds a goroutine and a connection for as long as a
// dependency hangs.
func CheckTimeout(d time.Duration) error {
	return positive(d)
}

// CheckAttemptTimeout rejects a duration that is not positive: an attempt
// with no bound holds the call until Timeout, leaving no time to retry a
// dependency that hangs.
func CheckAttemptTimeout(d time.Duration) error {
	return positive(d)
}

// CheckMaxAttempts rejects fewer than 1 attempt: every call makes at least
// its first. 1 turns retries off.
func CheckMaxAttempts(n int) error {
	if n < 1 {
		return fmt.Errorf("must be at least 1, got %d", n)
	}
	return nil
}

// CheckBackoffBase rejects a duration that is not positive, as
// retry.Exponential does.
func CheckBackoffBase(d time.Duration) error {
	return positive(d)
}

// CheckBackoffMax rejects a duration that is not positive. NewClient also
// rejects a BackoffMax below BackoffBase, which takes both fields to tell.
func CheckBackoffMax(d time.Duration) error {
	return positive(d)
}

// CheckRetryBudgetMaxTokens accepts 3 to retry.TokenLimit tokens, as
// retry.NewBudget does: a budget allows no retry while it holds half of its
// tokens or fewer, so one of 1 or 2 tokens would never allow one.
func CheckRetryBudgetMaxTokens(n int) error {
	if n < 3 || n > retry.TokenLimit {
		return fmt.Errorf("must be from 3 to %d, got %d", retry.TokenLimit, n)
	}
	return nil
}

// CheckRetryBudgetTokenRatio accepts 0.001 to retry.TokenLimit tokens, as
// retry.NewBudget does. NaN is rejected.
func CheckRetryBudgetTokenRatio(r float64) error {
	if !(r >= 0.001 && r <= retry.TokenLimit) {
		return fmt.Errorf("must be from 0.001 to %d, got %v", retry.TokenLimit, r)
	}
	return nil
}

// CheckBreakerWindow accepts a window of 1 to circuit.MaxCount results, as
// circuit.New does.
func CheckBreakerWindow(n int) error {
	return count(n)
}

// CheckBreakerProbes accepts 1 to circuit.MaxCount probes in flight at
// once, as circuit.New does.
func CheckBreakerProbes(n int) error {
	return count(n)
}

// CheckBreakerOpenFor rejects a duration that is not positive, as
// circuit.New does: a breaker that is open for no time sheds nothing.
func CheckBreakerOpenFor(d time.Duration) error {
	return positive(d)
}

// CheckBreakerProbeTimeout rejects a duration that is not positive, as
// circuit.New does.
func CheckBreakerProbeTimeout(d time.Duration) error {
	return positive(d)
}

// count rejects a count that is not from 1 to circuit.MaxCount.
func count(n int) error {
	if n < 1 || n > circuit.MaxCount {
		return fmt.Errorf("must be from 1 to %d, got %d", circuit.MaxCount, n)
	}
	return nil
}

// positive rejects a duration that is not positive.
func positive(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}
