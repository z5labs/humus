// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client_test

import (
	"context"
	"crypto/tls"
	"errors"
	"math"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus/http3/http3client"
	"github.com/z5labs/humus/humustest"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
// Settings.TLS is checked by tlsField, since "TLS 1.3" names a version.
var fieldNames = []string{
	"Settings", "BaseURL", "Timeout", "AttemptTimeout", "MaxAttempts",
	"BackoffBase", "BackoffMax", "RetryBudget", "MaxTokens", "TokenRatio",
	"Classify", "Breaker", "BreakerPolicy", "Window", "Probes", "OpenFor",
	"ProbeTimeout", "Trip", "Probe",
	"base", "timeout", "attempts", "backoff", "budget", "tokens", "ratio",
	"breaker", "window", "probes", "open",
}

// tlsField matches "TLS" where it is not the start of a version, such as
// "TLS 1.3".
var tlsField = regexp.MustCompile(`TLS(?:[^ ]|$)|TLS [^1]`)

// urlParts are the parts of the URLs TestChecks rejects that an error
// quoting the URL would repeat.
var urlParts = []string{"payments", "secret", "region", "charges", "://"}

// parse parses raw, which must be a URL.
func parse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestChecks(t *testing.T) {
	t.Parallel()

	baseURL := func(raw string) func() error {
		return func() error { return http3client.CheckBaseURL(parse(t, raw)) }
	}
	tlsConfig := func(c *tls.Config) func() error {
		return func() error { return http3client.CheckTLS(c) }
	}
	durations := func(check func(time.Duration) error, ds ...time.Duration) []func() error {
		var fs []func() error
		for _, d := range ds {
			fs = append(fs, func() error { return check(d) })
		}
		return fs
	}
	ints := func(check func(int) error, ns ...int) []func() error {
		var fs []func() error
		for _, n := range ns {
			fs = append(fs, func() error { return check(n) })
		}
		return fs
	}
	ratios := func(rs ...float64) []func() error {
		var fs []func() error
		for _, r := range rs {
			fs = append(fs, func() error { return http3client.CheckRetryBudgetTokenRatio(r) })
		}
		return fs
	}
	positive := []time.Duration{time.Nanosecond, 100 * time.Millisecond, 5 * time.Second, time.Hour}
	notPositive := []time.Duration{0, -time.Nanosecond, -time.Second}
	roots := humustest.NewCA(t).Pool()

	for _, c := range []struct {
		name   string
		accept []func() error
		reject []func() error
	}{
		{
			name: "CheckBaseURL",
			accept: []func() error{
				baseURL("https://payments:8443"),
				baseURL("https://payments"),
				baseURL("https://payments:8443/v1/"),
				baseURL("https://10.0.0.1:8443"),
				baseURL("https://[::1]:8443"),
			},
			reject: []func() error{
				func() error { return http3client.CheckBaseURL(nil) },
				baseURL("http://payments:8080"),
				baseURL("http://payments"),
				baseURL("payments:8443"),
				baseURL("/v1"),
				baseURL("ftp://payments"),
				baseURL("grpc://payments:8443"),
				baseURL("https://"),
				baseURL("https://:8443"),
				baseURL("https:payments"),
				baseURL("https://user:secret@payments:8443"),
				baseURL("https://user@payments:8443"),
				baseURL("https://payments:8443/?region=eu"),
				baseURL("https://payments:8443/?"),
				baseURL("https://payments:8443/#charges"),
			},
		},
		{
			name: "CheckTLS",
			accept: []func() error{
				tlsConfig(nil),
				tlsConfig(&tls.Config{}),
				tlsConfig(&tls.Config{RootCAs: roots}),
				tlsConfig(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}),
				tlsConfig(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS13}),
			},
			reject: []func() error{
				tlsConfig(&tls.Config{InsecureSkipVerify: true}),
				tlsConfig(&tls.Config{RootCAs: roots, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS12}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS11}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS10}),
			},
		},
		{
			name:   "CheckTimeout",
			accept: durations(http3client.CheckTimeout, positive...),
			reject: durations(http3client.CheckTimeout, notPositive...),
		},
		{
			name:   "CheckAttemptTimeout",
			accept: durations(http3client.CheckAttemptTimeout, positive...),
			reject: durations(http3client.CheckAttemptTimeout, notPositive...),
		},
		{
			name:   "CheckBackoffBase",
			accept: durations(http3client.CheckBackoffBase, positive...),
			reject: durations(http3client.CheckBackoffBase, notPositive...),
		},
		{
			name:   "CheckBackoffMax",
			accept: durations(http3client.CheckBackoffMax, positive...),
			reject: durations(http3client.CheckBackoffMax, notPositive...),
		},
		{
			name:   "CheckMaxAttempts",
			accept: ints(http3client.CheckMaxAttempts, 1, 3, math.MaxInt),
			reject: ints(http3client.CheckMaxAttempts, 0, -1, math.MinInt),
		},
		{
			name:   "CheckRetryBudgetMaxTokens",
			accept: ints(http3client.CheckRetryBudgetMaxTokens, 3, 10, retry.TokenLimit),
			reject: ints(http3client.CheckRetryBudgetMaxTokens, 2, 0, -3, retry.TokenLimit+1),
		},
		{
			name:   "CheckRetryBudgetTokenRatio",
			accept: ratios(0.001, 0.1, 1, retry.TokenLimit),
			reject: ratios(0.0009, 0, -0.1, retry.TokenLimit+0.001, math.NaN(), math.Inf(1)),
		},
		{
			name:   "CheckBreakerWindow",
			accept: ints(http3client.CheckBreakerWindow, 1, 20, circuit.MaxCount),
			reject: ints(http3client.CheckBreakerWindow, 0, -1, circuit.MaxCount+1, math.MinInt, math.MaxInt),
		},
		{
			name:   "CheckBreakerProbes",
			accept: ints(http3client.CheckBreakerProbes, 1, 3, circuit.MaxCount),
			reject: ints(http3client.CheckBreakerProbes, 0, -1, circuit.MaxCount+1, math.MinInt, math.MaxInt),
		},
		{
			name:   "CheckBreakerOpenFor",
			accept: durations(http3client.CheckBreakerOpenFor, positive...),
			reject: durations(http3client.CheckBreakerOpenFor, notPositive...),
		},
		{
			name:   "CheckBreakerProbeTimeout",
			accept: durations(http3client.CheckBreakerProbeTimeout, positive...),
			reject: durations(http3client.CheckBreakerProbeTimeout, notPositive...),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for i, accept := range c.accept {
				require.NoError(t, accept(), "in-range value %d", i)
			}
			for i, reject := range c.reject {
				err := reject()
				require.Error(t, err, "out-of-range value %d", i)
				for _, name := range fieldNames {
					require.NotContains(t, err.Error(), name, "the error of out-of-range value %d names a field", i)
				}
				require.NotRegexp(t, tlsField, err.Error(), "the error of out-of-range value %d names a field", i)
				for _, part := range urlParts {
					require.NotContains(t, err.Error(), part, "the error of out-of-range value %d quotes the URL", i)
				}
			}
		})
	}
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	d := http3client.DefaultSettings()
	require.NotNil(t, d.Classify)
	require.NotNil(t, d.BreakerPolicy.Trip)
	require.NotNil(t, d.BreakerPolicy.Probe)
	withoutFuncs := d
	withoutFuncs.Classify = nil
	withoutFuncs.BreakerPolicy = circuit.Policy{}
	require.Equal(t, http3client.Settings{
		BaseURL:        nil,
		TLS:            nil,
		Timeout:        10 * time.Second,
		AttemptTimeout: 5 * time.Second,
		MaxAttempts:    3,
		BackoffBase:    100 * time.Millisecond,
		BackoffMax:     5 * time.Second,
		RetryBudget:    retry.BudgetConfig{MaxTokens: 10, TokenRatio: 0.1},
		Breaker: circuit.Config{
			Window:       20,
			Probes:       1,
			OpenFor:      10 * time.Second,
			ProbeTimeout: 10 * time.Second,
		},
		ForwardScenario: false,
	}, withoutFuncs)

	t.Run("its BreakerPolicy opens after 5 failures in a row, or once half of 20 or more failed", func(t *testing.T) {
		t.Parallel()
		trip := d.BreakerPolicy.Trip
		for _, c := range []struct {
			counts circuit.Counts
			opens  bool
		}{
			{circuit.Counts{Failures: 4, ConsecutiveFailures: 4}, false},
			{circuit.Counts{Failures: 5, ConsecutiveFailures: 5}, true},
			{circuit.Counts{Successes: 15, Failures: 5, ConsecutiveFailures: 5}, true},
			{circuit.Counts{Successes: 10, Failures: 10, ConsecutiveFailures: 4}, true},
			{circuit.Counts{Successes: 11, Failures: 9, ConsecutiveFailures: 4}, false},
			{circuit.Counts{Successes: 9, Failures: 9, ConsecutiveFailures: 4}, false},
			{circuit.Counts{Successes: 20, ConsecutiveSuccesses: 20}, false},
		} {
			require.Equal(t, c.opens, trip(c.counts), "%+v", c.counts)
		}
	})

	t.Run("its BreakerPolicy reopens on a failed probe and closes after 3 successful ones", func(t *testing.T) {
		t.Parallel()
		probe := d.BreakerPolicy.Probe
		for _, c := range []struct {
			counts circuit.Counts
			state  circuit.State
		}{
			{circuit.Counts{Failures: 1, ConsecutiveFailures: 1}, circuit.Open},
			{circuit.Counts{Successes: 2, Failures: 1, ConsecutiveFailures: 1}, circuit.Open},
			{circuit.Counts{Successes: 2, ConsecutiveSuccesses: 2}, circuit.HalfOpen},
			{circuit.Counts{Successes: 3, ConsecutiveSuccesses: 3}, circuit.Closed},
			{circuit.Counts{TimedOut: 1}, circuit.HalfOpen},
			{circuit.Counts{Successes: 3, ConsecutiveSuccesses: 3, TimedOut: 2}, circuit.Closed},
		} {
			require.Equal(t, c.state, probe(c.counts), "%+v", c.counts)
		}
	})

	t.Run("the breaker's checks accept what circuit.New accepts", func(t *testing.T) {
		t.Parallel()
		classify := func(context.Context, error) circuit.Outcome { return circuit.Success }
		observe := func(context.Context, circuit.Transition) {}
		for _, cfg := range []circuit.Config{
			d.Breaker,
			{Window: 1, Probes: 1, OpenFor: time.Nanosecond, ProbeTimeout: time.Nanosecond},
			{Window: circuit.MaxCount, Probes: circuit.MaxCount, OpenFor: time.Hour, ProbeTimeout: time.Hour},
			{Window: 0, Probes: 1, OpenFor: time.Second, ProbeTimeout: time.Second},
			{Window: circuit.MaxCount + 1, Probes: 1, OpenFor: time.Second, ProbeTimeout: time.Second},
			{Window: 20, Probes: 0, OpenFor: time.Second, ProbeTimeout: time.Second},
			{Window: 20, Probes: circuit.MaxCount + 1, OpenFor: time.Second, ProbeTimeout: time.Second},
			{Window: 20, Probes: 1, OpenFor: 0, ProbeTimeout: time.Second},
			{Window: 20, Probes: 1, OpenFor: time.Second, ProbeTimeout: -time.Second},
		} {
			_, newErr := circuit.New(cfg, d.BreakerPolicy, classify, observe)
			checksErr := errors.Join(
				http3client.CheckBreakerWindow(cfg.Window),
				http3client.CheckBreakerProbes(cfg.Probes),
				http3client.CheckBreakerOpenFor(cfg.OpenFor),
				http3client.CheckBreakerProbeTimeout(cfg.ProbeTimeout),
			)
			require.Equal(t, newErr == nil, checksErr == nil, "%+v: New %v, the checks %v", cfg, newErr, checksErr)
		}
	})

	t.Run("the budget's checks accept what retry.NewBudget accepts", func(t *testing.T) {
		t.Parallel()
		for _, cfg := range []retry.BudgetConfig{
			d.RetryBudget,
			{MaxTokens: 3, TokenRatio: 0.001},
			{MaxTokens: retry.TokenLimit, TokenRatio: retry.TokenLimit},
			{MaxTokens: 2, TokenRatio: 0.1},
			{MaxTokens: retry.TokenLimit + 1, TokenRatio: 0.1},
			{MaxTokens: 10, TokenRatio: 0.0009},
			{MaxTokens: 10, TokenRatio: retry.TokenLimit + 0.5},
			{MaxTokens: 10, TokenRatio: math.NaN()},
		} {
			_, budgetErr := retry.NewBudget(cfg)
			checksErr := errors.Join(
				http3client.CheckRetryBudgetMaxTokens(cfg.MaxTokens),
				http3client.CheckRetryBudgetTokenRatio(cfg.TokenRatio),
			)
			require.Equal(t, budgetErr == nil, checksErr == nil, "%+v: NewBudget %v, the checks %v", cfg, budgetErr, checksErr)
		}
	})

	t.Run("passes the checks of the fields that have a default", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, http3client.CheckTLS(d.TLS))
		require.NoError(t, http3client.CheckTimeout(d.Timeout))
		require.NoError(t, http3client.CheckAttemptTimeout(d.AttemptTimeout))
		require.NoError(t, http3client.CheckMaxAttempts(d.MaxAttempts))
		require.NoError(t, http3client.CheckBackoffBase(d.BackoffBase))
		require.NoError(t, http3client.CheckBackoffMax(d.BackoffMax))
		require.NoError(t, http3client.CheckRetryBudgetMaxTokens(d.RetryBudget.MaxTokens))
		require.NoError(t, http3client.CheckRetryBudgetTokenRatio(d.RetryBudget.TokenRatio))
		require.NoError(t, http3client.CheckBreakerWindow(d.Breaker.Window))
		require.NoError(t, http3client.CheckBreakerProbes(d.Breaker.Probes))
		require.NoError(t, http3client.CheckBreakerOpenFor(d.Breaker.OpenFor))
		require.NoError(t, http3client.CheckBreakerProbeTimeout(d.Breaker.ProbeTimeout))
	})

	t.Run("fails the check of BaseURL, which has none", func(t *testing.T) {
		t.Parallel()
		require.Error(t, http3client.CheckBaseURL(d.BaseURL))
	})
}
