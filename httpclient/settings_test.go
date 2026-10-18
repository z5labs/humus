// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

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

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
// Settings.TLS is checked by tlsField, since "TLS 1.2" names a version.
var fieldNames = []string{
	"Settings", "BaseURL", "Timeout", "AttemptTimeout", "MaxAttempts",
	"BackoffBase", "BackoffMax", "RetryBudget", "MaxTokens", "TokenRatio",
	"Classify", "Breaker", "BreakerPolicy", "Window", "Probes", "OpenFor",
	"ProbeTimeout", "Trip", "Probe",
	"base", "timeout", "attempts", "backoff", "budget", "tokens", "ratio",
	"breaker", "window", "probes", "open",
}

// tlsField matches "TLS" where it is not the start of a version, such as
// "TLS 1.2".
var tlsField = regexp.MustCompile(`TLS(?:[^ ]|$)|TLS [^1]`)

func TestChecks(t *testing.T) {
	t.Parallel()

	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		return u
	}
	baseURL := func(raw string) func() error {
		return func() error { return httpclient.CheckBaseURL(parse(raw)) }
	}
	tlsConfig := func(c *tls.Config) func() error {
		return func() error { return httpclient.CheckTLS(c) }
	}
	timeout := func(d time.Duration) func() error {
		return func() error { return httpclient.CheckTimeout(d) }
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
			fs = append(fs, func() error { return httpclient.CheckRetryBudgetTokenRatio(r) })
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
				baseURL("http://payments:8080"),
				baseURL("https://payments:8443/v1/"),
				baseURL("https://10.0.0.1:8443"),
				baseURL("https://[::1]:8443"),
			},
			reject: []func() error{
				func() error { return httpclient.CheckBaseURL(nil) },
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
				tlsConfig(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS12}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS13}),
			},
			reject: []func() error{
				tlsConfig(&tls.Config{InsecureSkipVerify: true}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS11}),
				tlsConfig(&tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS10}),
			},
		},
		{
			name: "CheckTimeout",
			accept: []func() error{
				timeout(time.Nanosecond),
				timeout(10 * time.Second),
				timeout(time.Hour),
			},
			reject: []func() error{
				timeout(0),
				timeout(-time.Nanosecond),
				timeout(-time.Second),
			},
		},
		{
			name:   "CheckAttemptTimeout",
			accept: durations(httpclient.CheckAttemptTimeout, positive...),
			reject: durations(httpclient.CheckAttemptTimeout, notPositive...),
		},
		{
			name:   "CheckBackoffBase",
			accept: durations(httpclient.CheckBackoffBase, positive...),
			reject: durations(httpclient.CheckBackoffBase, notPositive...),
		},
		{
			name:   "CheckBackoffMax",
			accept: durations(httpclient.CheckBackoffMax, positive...),
			reject: durations(httpclient.CheckBackoffMax, notPositive...),
		},
		{
			name:   "CheckMaxAttempts",
			accept: ints(httpclient.CheckMaxAttempts, 1, 3, math.MaxInt),
			reject: ints(httpclient.CheckMaxAttempts, 0, -1, math.MinInt),
		},
		{
			name:   "CheckRetryBudgetMaxTokens",
			accept: ints(httpclient.CheckRetryBudgetMaxTokens, 3, 10, retry.TokenLimit),
			reject: ints(httpclient.CheckRetryBudgetMaxTokens, 2, 0, -3, retry.TokenLimit+1),
		},
		{
			name:   "CheckRetryBudgetTokenRatio",
			accept: ratios(0.001, 0.1, 1, retry.TokenLimit),
			reject: ratios(0.0009, 0, -0.1, retry.TokenLimit+0.001, math.NaN(), math.Inf(1)),
		},
		{
			name:   "CheckBreakerWindow",
			accept: ints(httpclient.CheckBreakerWindow, 1, 20, circuit.MaxCount),
			reject: ints(httpclient.CheckBreakerWindow, 0, -1, circuit.MaxCount+1, math.MinInt, math.MaxInt),
		},
		{
			name:   "CheckBreakerProbes",
			accept: ints(httpclient.CheckBreakerProbes, 1, 3, circuit.MaxCount),
			reject: ints(httpclient.CheckBreakerProbes, 0, -1, circuit.MaxCount+1, math.MinInt, math.MaxInt),
		},
		{
			name:   "CheckBreakerOpenFor",
			accept: durations(httpclient.CheckBreakerOpenFor, positive...),
			reject: durations(httpclient.CheckBreakerOpenFor, notPositive...),
		},
		{
			name:   "CheckBreakerProbeTimeout",
			accept: durations(httpclient.CheckBreakerProbeTimeout, positive...),
			reject: durations(httpclient.CheckBreakerProbeTimeout, notPositive...),
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
				require.NotContains(t, err.Error(), "secret", "the error of out-of-range value %d repeats a password", i)
			}
		})
	}
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	d := httpclient.DefaultSettings()
	require.NotNil(t, d.Classify)
	require.NotNil(t, d.BreakerPolicy.Trip)
	require.NotNil(t, d.BreakerPolicy.Probe)
	withoutFuncs := d
	withoutFuncs.Classify = nil
	withoutFuncs.BreakerPolicy = circuit.Policy{}
	require.Equal(t, httpclient.Settings{
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
				httpclient.CheckBreakerWindow(cfg.Window),
				httpclient.CheckBreakerProbes(cfg.Probes),
				httpclient.CheckBreakerOpenFor(cfg.OpenFor),
				httpclient.CheckBreakerProbeTimeout(cfg.ProbeTimeout),
			)
			require.Equal(t, newErr == nil, checksErr == nil, "%+v: New %v, the checks %v", cfg, newErr, checksErr)
		}
	})

	t.Run("passes the checks of the fields that have a default", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, httpclient.CheckTLS(d.TLS))
		require.NoError(t, httpclient.CheckTimeout(d.Timeout))
		require.NoError(t, httpclient.CheckAttemptTimeout(d.AttemptTimeout))
		require.NoError(t, httpclient.CheckMaxAttempts(d.MaxAttempts))
		require.NoError(t, httpclient.CheckBackoffBase(d.BackoffBase))
		require.NoError(t, httpclient.CheckBackoffMax(d.BackoffMax))
		require.NoError(t, httpclient.CheckRetryBudgetMaxTokens(d.RetryBudget.MaxTokens))
		require.NoError(t, httpclient.CheckRetryBudgetTokenRatio(d.RetryBudget.TokenRatio))
		require.NoError(t, httpclient.CheckBreakerWindow(d.Breaker.Window))
		require.NoError(t, httpclient.CheckBreakerProbes(d.Breaker.Probes))
		require.NoError(t, httpclient.CheckBreakerOpenFor(d.Breaker.OpenFor))
		require.NoError(t, httpclient.CheckBreakerProbeTimeout(d.Breaker.ProbeTimeout))
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
				httpclient.CheckRetryBudgetMaxTokens(cfg.MaxTokens),
				httpclient.CheckRetryBudgetTokenRatio(cfg.TokenRatio),
			)
			require.Equal(t, budgetErr == nil, checksErr == nil, "%+v: NewBudget %v, the checks %v", cfg, budgetErr, checksErr)
		}
	})

	t.Run("fails the check of BaseURL, which has none", func(t *testing.T) {
		t.Parallel()
		require.Error(t, httpclient.CheckBaseURL(d.BaseURL))
	})
}

// newSetup returns a Setup on a new humustest Host, with telemetry that
// discards everything, for a test that builds a client and sends nothing.
func newSetup(t *testing.T) *humus.Setup {
	t.Helper()
	return &humus.Setup{Host: humustest.NewHost(t).Host(), Telemetry: telemetry.Noop()}
}

func TestNewClient_the_Setup(t *testing.T) {
	t.Parallel()

	settings := httpclient.DefaultSettings()
	settings.BaseURL = mustParse(t, "http://payments:8080")

	t.Run("a nil Setup is rejected", func(t *testing.T) {
		t.Parallel()
		c, err := httpclient.NewClient(nil, settings)
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: the Setup must not be nil")
	})

	t.Run("a Host with no Dial is rejected, rather than dialling the real network", func(t *testing.T) {
		t.Parallel()
		c, err := httpclient.NewClient(&humus.Setup{Telemetry: telemetry.Noop()}, settings)
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: the Setup's Host.Dial must not be nil")
	})

	t.Run("a Telemetry with no LoggerProvider is rejected, rather than panic", func(t *testing.T) {
		t.Parallel()
		c, err := httpclient.NewClient(&humus.Setup{Host: humustest.NewHost(t).Host()}, settings)
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: the Setup's Telemetry.LoggerProvider must not be nil")
	})

	t.Run("alongside the bad fields", func(t *testing.T) {
		t.Parallel()
		_, err := httpclient.NewClient(&humus.Setup{}, httpclient.DefaultSettings())
		require.ErrorContains(t, err, "the Setup's Host.Dial must not be nil")
		require.ErrorContains(t, err, "the Setup's Telemetry.LoggerProvider must not be nil")
		require.ErrorContains(t, err, "Settings.BaseURL must not be nil")
	})
}

func TestNewClient_settings(t *testing.T) {
	t.Parallel()

	https := func(t *testing.T) httpclient.Settings {
		settings := httpclient.DefaultSettings()
		settings.BaseURL = mustParse(t, "https://payments:8443")
		settings.TLS = &tls.Config{RootCAs: humustest.NewCA(t).Pool()}
		return settings
	}

	t.Run("DefaultSettings alone fails, naming BaseURL", func(t *testing.T) {
		t.Parallel()
		c, err := httpclient.NewClient(newSetup(t), httpclient.DefaultSettings())
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: Settings.BaseURL must not be nil")
	})

	t.Run("every bad field is named in one error", func(t *testing.T) {
		t.Parallel()
		settings := https(t)
		settings.TLS.InsecureSkipVerify = true
		settings.Timeout = 0
		settings.AttemptTimeout = 0
		settings.MaxAttempts = 0
		settings.BackoffBase = 0
		settings.BackoffMax = -time.Second
		settings.RetryBudget = retry.BudgetConfig{MaxTokens: 2, TokenRatio: 0}
		settings.Classify = nil
		settings.Breaker = circuit.Config{Window: 0, Probes: circuit.MaxCount + 1, OpenFor: 0, ProbeTimeout: -time.Second}
		settings.BreakerPolicy = circuit.Policy{}

		_, err := httpclient.NewClient(newSetup(t), settings)
		require.ErrorContains(t, err, "httpclient: NewClient: ")
		require.ErrorContains(t, err, "Settings.TLS sets InsecureSkipVerify")
		require.ErrorContains(t, err, "Settings.Timeout must be positive")
		require.ErrorContains(t, err, "Settings.AttemptTimeout must be positive")
		require.ErrorContains(t, err, "Settings.MaxAttempts must be at least 1")
		require.ErrorContains(t, err, "Settings.BackoffBase must be positive")
		require.ErrorContains(t, err, "Settings.BackoffMax must be positive")
		require.ErrorContains(t, err, "Settings.RetryBudget.MaxTokens must be from 3")
		require.ErrorContains(t, err, "Settings.RetryBudget.TokenRatio must be from 0.001")
		require.ErrorContains(t, err, "Settings.Classify must not be nil")
		require.ErrorContains(t, err, "Settings.Breaker.Window must be from 1 to 65536, got 0")
		require.ErrorContains(t, err, "Settings.Breaker.Probes must be from 1 to 65536, got 65537")
		require.ErrorContains(t, err, "Settings.Breaker.OpenFor must be positive")
		require.ErrorContains(t, err, "Settings.Breaker.ProbeTimeout must be positive")
		require.ErrorContains(t, err, "Settings.BreakerPolicy.Trip must not be nil")
		require.ErrorContains(t, err, "Settings.BreakerPolicy.Probe must not be nil")
		require.NotContains(t, err.Error(), "Settings.BaseURL")
	})

	t.Run("a BackoffMax below BackoffBase is rejected, next to the other bad fields", func(t *testing.T) {
		t.Parallel()
		settings := https(t)
		settings.BackoffBase = 2 * time.Second
		settings.BackoffMax = time.Second
		settings.Classify = nil

		c, err := httpclient.NewClient(newSetup(t), settings)
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: Settings.BackoffMax must be at least Settings.BackoffBase, 2s, got 1s\n"+
			"Settings.Classify must not be nil")
	})

	t.Run("a BackoffMax equal to BackoffBase passes", func(t *testing.T) {
		t.Parallel()
		settings := https(t)
		settings.BackoffBase = time.Second
		settings.BackoffMax = time.Second
		_, err := httpclient.NewClient(newSetup(t), settings)
		require.NoError(t, err)
	})

	t.Run("each field is named", func(t *testing.T) {
		t.Parallel()
		for field, change := range map[string]func(*httpclient.Settings){
			"Settings.BaseURL":                func(s *httpclient.Settings) { s.BaseURL = mustParse(t, "https://payments:8443/?q=1") },
			"Settings.TLS":                    func(s *httpclient.Settings) { s.TLS.MaxVersion = tls.VersionTLS11 },
			"Settings.Timeout":                func(s *httpclient.Settings) { s.Timeout = -time.Second },
			"Settings.AttemptTimeout":         func(s *httpclient.Settings) { s.AttemptTimeout = -time.Second },
			"Settings.MaxAttempts":            func(s *httpclient.Settings) { s.MaxAttempts = 0 },
			"Settings.BackoffBase":            func(s *httpclient.Settings) { s.BackoffBase = 0 },
			"Settings.BackoffMax":             func(s *httpclient.Settings) { s.BackoffMax = 0 },
			"Settings.RetryBudget.MaxTokens":  func(s *httpclient.Settings) { s.RetryBudget.MaxTokens = 1 },
			"Settings.RetryBudget.TokenRatio": func(s *httpclient.Settings) { s.RetryBudget.TokenRatio = -1 },
			"Settings.Classify":               func(s *httpclient.Settings) { s.Classify = nil },
			"Settings.Breaker.Window":         func(s *httpclient.Settings) { s.Breaker.Window = 0 },
			"Settings.Breaker.Probes":         func(s *httpclient.Settings) { s.Breaker.Probes = circuit.MaxCount + 1 },
			"Settings.Breaker.OpenFor":        func(s *httpclient.Settings) { s.Breaker.OpenFor = 0 },
			"Settings.Breaker.ProbeTimeout":   func(s *httpclient.Settings) { s.Breaker.ProbeTimeout = 0 },
			"Settings.BreakerPolicy.Trip":     func(s *httpclient.Settings) { s.BreakerPolicy.Trip = nil },
			"Settings.BreakerPolicy.Probe":    func(s *httpclient.Settings) { s.BreakerPolicy.Probe = nil },
		} {
			settings := https(t)
			change(&settings)
			_, err := httpclient.NewClient(newSetup(t), settings)
			require.ErrorContains(t, err, field)
		}
	})

	t.Run("an https BaseURL with a nil TLS is rejected", func(t *testing.T) {
		t.Parallel()
		settings := https(t)
		settings.TLS = nil
		c, err := httpclient.NewClient(newSetup(t), settings)
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: Settings.TLS must be set for an https Settings.BaseURL")
	})

	t.Run("an http BaseURL with a TLS config is rejected", func(t *testing.T) {
		t.Parallel()
		settings := https(t)
		settings.BaseURL = mustParse(t, "http://payments:8080")
		c, err := httpclient.NewClient(newSetup(t), settings)
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: Settings.TLS must be nil for an http Settings.BaseURL, which is plain text")
	})

	t.Run("DefaultSettings with an http BaseURL passes", func(t *testing.T) {
		t.Parallel()
		settings := httpclient.DefaultSettings()
		settings.BaseURL = mustParse(t, "http://payments:8080")
		c, err := httpclient.NewClient(newSetup(t), settings)
		require.NoError(t, err)
		require.NotNil(t, c)
		require.Equal(t, 10*time.Second, c.Timeout)
		require.Nil(t, c.Jar)
	})

	t.Run("DefaultSettings with an https BaseURL and a TLS config passes", func(t *testing.T) {
		t.Parallel()
		c, err := httpclient.NewClient(newSetup(t), https(t))
		require.NoError(t, err)
		require.NotNil(t, c)
	})
}
