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
	// itself: an error once the call's context has ended. It is asked
	// about OK, a nil error, too, so a Classify that returns retry.Retry
	// for every result sends a call that succeeded again: it returns
	// retry.Success for OK, as the default does.
	//
	// For a stream, it is asked about an open that failed, with no
	// trailer, since nothing came back. A stream that opens is never
	// retried, and Classify isn't asked about it.
	//
	// It is called on the goroutine that made the call, once for each
	// attempt, so it must be safe for concurrent use. A Classification
	// with a zero Outcome, or with retry.Retry and a negative RetryAfter,
	// panics, as bedrock's classifiers do.
	//
	// Default: the classification the package doc's "How an attempt's
	// result counts" describes. Valid: set, which NewClient checks.
	Classify func(method string, err error, trailer metadata.MD) Classification
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

// positive rejects a duration that is not positive.
func positive(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}

// checkSettings runs every check on settings, and rejects a BackoffMax
// below BackoffBase and a nil Classify. It reports every failure in one
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
	return errors.Join(errs...)
}
