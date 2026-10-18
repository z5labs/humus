// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"google.golang.org/grpc/metadata"
)

// Settings configures a Client. Every field is a plain value, and a field's
// zero value means zero, never "use the default": start from DefaultSettings
// and change the fields you source. Addr has no default, so it is always
// among them.
type Settings struct {
	// Addr is the address the Client dials through the Setup's Host, as
	// host:port. The Host resolves the host when it dials. Addr is also the
	// RPCs' :authority, and its host the name the server's certificate is
	// verified for, unless TLS sets a ServerName, or opts has a
	// grpc.WithAuthority, which then replaces both. It is required.
	//
	// Default: none. Valid: host:port, with a host and a port from 1 to
	// 65535. Check: CheckAddr.
	Addr string

	// TLS is the config the Client dials with. nil means plain text. A
	// client certificate in Certificates makes it mTLS. With RootCAs nil,
	// the server is verified against the system's roots.
	//
	// Default: nil, plain text. Valid: nil, or a config with a MaxVersion
	// of TLS 1.2 or later, or none. Check: CheckTLS.
	TLS *tls.Config

	// Timeout bounds each unary RPC, its retries and the waits between
	// them included, unless the caller's context has an earlier deadline.
	// It doesn't apply to streams.
	//
	// Default: 10s. Valid: positive. Check: CheckTimeout.
	Timeout time.Duration

	// MaxAttempts bounds the attempts of each unary RPC, and of opening
	// each stream, the first included. 1 turns retries off.
	//
	// Default: 3. Valid: 1 or more. Check: CheckMaxAttempts.
	MaxAttempts int

	// BackoffBase and BackoffMax are the schedule between attempts:
	// retry.Exponential(BackoffBase, BackoffMax), exponential backoff with
	// full jitter, so the wait after attempt n is drawn from
	// [0, min(BackoffMax, BackoffBase·2ⁿ⁻¹)). A pushback longer than
	// BackoffMax isn't waited for.
	//
	// Default: 100ms and 5s. Valid: positive, and BackoffMax at least
	// BackoffBase, which NewClient checks. Check: CheckBackoffBase,
	// CheckBackoffMax.
	BackoffBase time.Duration
	BackoffMax  time.Duration

	// RetryBudget limits the retries of every RPC the Client makes, as
	// gRPC's retry throttling does, and as retry.Budget describes. First
	// attempts are never limited.
	//
	// Default: {MaxTokens: 10, TokenRatio: 0.1}. Valid: MaxTokens from 3 to
	// retry.TokenLimit; TokenRatio from 0.001 to retry.TokenLimit, as
	// retry.NewBudget accepts. Check: CheckRetryBudgetMaxTokens,
	// CheckRetryBudgetTokenRatio.
	RetryBudget retry.BudgetConfig

	// Classify decides how each attempt's result counts: whether another
	// attempt follows, and whether the result counts against the
	// dependency. It is set in code, not sourced, and one function may be
	// set on every Client whose dependencies agree on what their codes
	// mean.
	//
	// Classify gets the RPC's full method name, such as
	// "/cart.v1.CartService/AddItem", the error the attempt ended with, nil
	// for OK, and the attempt's trailer, which holds grpc-retry-pushback-ms.
	// status.FromError gives the error's code and its details, such as a
	// google.rpc.RetryInfo. It isn't asked about what the Client decides
	// itself: a call the circuit breaker rejected, and an error once the
	// call's context has ended. It is asked about OK, a nil error, too, so
	// a Classify that returns retry.Retry for every result sends a call
	// that succeeded again: it returns retry.Success for OK, as the
	// default does.
	//
	// For a stream, it is asked about an open that failed, with no
	// trailer, since nothing came back. A stream that opens is never
	// retried, and Classify isn't asked about its open. It is asked about
	// the stream's end, for the breaker alone: when RecvMsg returns an
	// error, with the stream's trailer and io.EOF given as nil, or, for a
	// client-streaming RPC, when RecvMsg returns its one response, given
	// as nil. Only the Breaker of that answer is read.
	//
	// It is called on the goroutine that made the call, once for each
	// attempt, so it must be safe for concurrent use. A Classification
	// with a zero Outcome, or with retry.Retry and a negative RetryAfter,
	// panics, as bedrock's classifiers do.
	//
	// Default: the classification the package doc's "How an attempt's
	// result counts" describes. Valid: set, which NewClient checks.
	Classify func(method string, err error, trailer metadata.MD) Classification

	// Breaker configures the Client's circuit breaker, which stands in
	// front of the dependency, inside the retries: Window is how many of
	// the latest results its counts cover, Probes how many calls it lets
	// through at once while half-open, OpenFor how long it rejects every
	// call once open, and ProbeTimeout how long a probe holds its slot.
	//
	// Default: circuit.Config{Window: 20, Probes: 1, OpenFor: 10 *
	// time.Second, ProbeTimeout: 10 * time.Second}. Valid: Window and
	// Probes from 1 to circuit.MaxCount, OpenFor and ProbeTimeout positive.
	// Checks: CheckBreakerWindow, CheckBreakerProbes, CheckBreakerOpenFor,
	// CheckBreakerProbeTimeout.
	Breaker circuit.Config

	// BreakerPolicy decides when the breaker opens and closes, from its
	// counts. It is set in code, not sourced. A policy whose Trip always
	// returns false turns the breaker off.
	//
	// The default's ratio rule needs 20 results, so with a Breaker.Window
	// below 20 the default trips only on its rule of 5 failures in a row.
	//
	// Its functions run with the breaker locked, on the goroutine of the
	// call whose result they decide on, so they must be fast and safe for
	// concurrent use. A panic in one passes to the goroutine that made the
	// call, and leaves the breaker consistent.
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

// DefaultSettings returns the Client's defaults. Addr has none, so its Addr
// is "", which CheckAddr rejects: the end user always sources it.
func DefaultSettings() Settings {
	return Settings{
		Addr:        "",
		TLS:         nil,
		Timeout:     10 * time.Second,
		MaxAttempts: 3,
		BackoffBase: 100 * time.Millisecond,
		BackoffMax:  5 * time.Second,
		RetryBudget: retry.BudgetConfig{MaxTokens: 10, TokenRatio: 0.1},
		Classify:    defaultClassify,
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

// CheckAddr reports why addr is not a valid Addr, or nil if it is. A valid
// Addr is host:port, with a host, and a port written as a number from 1 to
// 65535. It rejects an empty host, which grpcserver.CheckAddr accepts: a
// client dials a host, it doesn't listen on every name. It rejects a scheme,
// such as dns:///, since the Client never resolves names itself, and a
// named port, such as :grpc. It rejects ?, # and %, which grpc-go's target
// would read as a query, a fragment or an escape, so that the Host would be
// asked to dial something other than addr.
func CheckAddr(addr string) error {
	if strings.Contains(addr, "/") {
		return fmt.Errorf("%q is not host:port: it names a scheme or a path, and only the Host resolves names", addr)
	}
	if strings.ContainsAny(addr, "?#%") {
		return fmt.Errorf("%q is not host:port: it has a ?, # or %%", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if host == "" {
		return fmt.Errorf("%q has no host", addr)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("%q has no port from 1 to 65535", addr)
	}
	return nil
}

// CheckTLS reports why c can't dial a server, or nil if it can. nil is
// valid, and means plain text. A config must allow TLS 1.2 or later, which
// HTTP/2 requires: a MaxVersion of TLS 1.2 or later, or none. A config needs
// no certificate, since a client certificate is only for mTLS.
func CheckTLS(c *tls.Config) error {
	if c == nil {
		return nil
	}
	if c.MaxVersion != 0 && c.MaxVersion < tls.VersionTLS12 {
		return fmt.Errorf("has a MaxVersion of %s, below the TLS 1.2 that HTTP/2 requires", tls.VersionName(c.MaxVersion))
	}
	return nil
}

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errNil         = errors.New("must not be nil")
	errNotPositive = errors.New("must be positive")
)

// CheckTimeout rejects a duration that is not positive. A zero would mean
// no deadline, and an RPC with none can hang for as long as the server
// does.
func CheckTimeout(d time.Duration) error {
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

// CheckBreakerWindow accepts 1 to circuit.MaxCount results, as
// circuit.New does.
func CheckBreakerWindow(n int) error {
	return count(n)
}

// CheckBreakerProbes accepts 1 to circuit.MaxCount probes, as circuit.New
// does.
func CheckBreakerProbes(n int) error {
	return count(n)
}

// CheckBreakerOpenFor rejects a duration that is not positive, as
// circuit.New does.
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

// checkSettings runs every check on settings, and rejects a BackoffMax
// below BackoffBase, a nil Classify, and a BreakerPolicy with a nil Trip
// or Probe. It reports every failure in one
// error, each naming its field.
func checkSettings(settings Settings) error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	check("Settings.Addr", CheckAddr(settings.Addr))
	check("Settings.TLS", CheckTLS(settings.TLS))
	check("Settings.Timeout", CheckTimeout(settings.Timeout))
	check("Settings.MaxAttempts", CheckMaxAttempts(settings.MaxAttempts))
	backoffBaseErr := CheckBackoffBase(settings.BackoffBase)
	backoffMaxErr := CheckBackoffMax(settings.BackoffMax)
	check("Settings.BackoffBase", backoffBaseErr)
	check("Settings.BackoffMax", backoffMaxErr)
	if backoffBaseErr == nil && backoffMaxErr == nil && settings.BackoffMax < settings.BackoffBase {
		check("Settings.BackoffMax", fmt.Errorf("must be at least Settings.BackoffBase, %s, got %s", settings.BackoffBase, settings.BackoffMax))
	}
	check("Settings.RetryBudget.MaxTokens", CheckRetryBudgetMaxTokens(settings.RetryBudget.MaxTokens))
	check("Settings.RetryBudget.TokenRatio", CheckRetryBudgetTokenRatio(settings.RetryBudget.TokenRatio))
	if settings.Classify == nil {
		check("Settings.Classify", errNil)
	}
	check("Settings.Breaker.Window", CheckBreakerWindow(settings.Breaker.Window))
	check("Settings.Breaker.Probes", CheckBreakerProbes(settings.Breaker.Probes))
	check("Settings.Breaker.OpenFor", CheckBreakerOpenFor(settings.Breaker.OpenFor))
	check("Settings.Breaker.ProbeTimeout", CheckBreakerProbeTimeout(settings.Breaker.ProbeTimeout))
	if settings.BreakerPolicy.Trip == nil {
		check("Settings.BreakerPolicy.Trip", errNil)
	}
	if settings.BreakerPolicy.Probe == nil {
		check("Settings.BreakerPolicy.Probe", errNil)
	}
	return errors.Join(errs...)
}
