// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"math"
	"regexp"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/retry"
	"google.golang.org/grpc"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
// Settings.TLS is checked by tlsField, since "TLS 1.2" names a version.
var fieldNames = []string{
	"Settings", "Addr", "Timeout", "MaxAttempts", "BackoffBase", "BackoffMax",
	"RetryBudget", "MaxTokens", "TokenRatio", "Classify",
	"addr", "timeout", "attempts", "backoff", "budget", "tokens", "ratio",
}

// tlsField matches "TLS" where it is not the start of a version, such as
// "TLS 1.2".
var tlsField = regexp.MustCompile(`TLS(?:[^ ]|$)|TLS [^1]`)

func TestChecks(t *testing.T) {
	t.Parallel()

	addr := func(a string) func() error {
		return func() error { return grpcclient.CheckAddr(a) }
	}
	tlsConfig := func(c *tls.Config) func() error {
		return func() error { return grpcclient.CheckTLS(c) }
	}
	timeout := func(d time.Duration) func() error {
		return func() error { return grpcclient.CheckTimeout(d) }
	}
	roots := humustest.NewCA(t).Pool()
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
	floats := func(check func(float64) error, rs ...float64) []func() error {
		var fs []func() error
		for _, r := range rs {
			fs = append(fs, func() error { return check(r) })
		}
		return fs
	}
	positive := []time.Duration{time.Nanosecond, 100 * time.Millisecond, 5 * time.Second, time.Hour}
	notPositive := []time.Duration{0, -time.Nanosecond, -time.Second}

	for _, c := range []struct {
		name   string
		accept []func() error
		reject []func() error
	}{
		{
			name: "CheckAddr",
			accept: []func() error{
				addr("carts:9090"),
				addr("carts.shop.svc.cluster.local:443"),
				addr("10.0.0.1:9090"),
				addr("[::1]:9090"),
				addr("carts:1"),
				addr("carts:65535"),
			},
			reject: []func() error{
				addr(""),
				addr(":9090"),
				addr("carts"),
				addr("carts:"),
				addr("carts:0"),
				addr("carts:65536"),
				addr("carts:-1"),
				addr("carts:grpc"),
				addr("dns:///carts:9090"),
				addr("passthrough:///carts:9090"),
				addr("unix:///run/carts.sock"),
				addr("carts:9090/v1"),
				addr("carts?x:9090"),
				addr("carts#x:9090"),
				addr("ca%41rts:9090"),
				addr("car%zz:9090"),
				addr("::1:9090"),
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
			name:   "CheckMaxAttempts",
			accept: ints(grpcclient.CheckMaxAttempts, 1, 3, math.MaxInt),
			reject: ints(grpcclient.CheckMaxAttempts, 0, -1, math.MinInt),
		},
		{
			name:   "CheckBackoffBase",
			accept: durations(grpcclient.CheckBackoffBase, positive...),
			reject: durations(grpcclient.CheckBackoffBase, notPositive...),
		},
		{
			name:   "CheckBackoffMax",
			accept: durations(grpcclient.CheckBackoffMax, positive...),
			reject: durations(grpcclient.CheckBackoffMax, notPositive...),
		},
		{
			name:   "CheckRetryBudgetMaxTokens",
			accept: ints(grpcclient.CheckRetryBudgetMaxTokens, 3, 10, retry.TokenLimit),
			reject: ints(grpcclient.CheckRetryBudgetMaxTokens, 2, 0, -1, retry.TokenLimit+1),
		},
		{
			name:   "CheckRetryBudgetTokenRatio",
			accept: floats(grpcclient.CheckRetryBudgetTokenRatio, 0.001, 0.1, 1, retry.TokenLimit),
			reject: floats(grpcclient.CheckRetryBudgetTokenRatio, 0.0009, 0, -0.1, retry.TokenLimit+0.001, math.NaN(), math.Inf(1)),
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
			}
		})
	}
}

// TestChecks_accept_what_retry_NewBudget_accepts checks the two budget
// checks against retry.NewBudget itself, at and around each bound, so that
// NewClient never meets a budget its checks passed and NewBudget rejects.
func TestChecks_accept_what_retry_NewBudget_accepts(t *testing.T) {
	t.Parallel()

	for _, n := range []int{math.MinInt, -1, 0, 2, 3, 10, retry.TokenLimit, retry.TokenLimit + 1, math.MaxInt} {
		_, err := retry.NewBudget(retry.BudgetConfig{MaxTokens: n, TokenRatio: 0.1})
		require.Equal(t, err == nil, grpcclient.CheckRetryBudgetMaxTokens(n) == nil, "MaxTokens %d", n)
	}
	for _, r := range []float64{math.Inf(-1), -0.1, 0, 0.0009, 0.001, 0.1, 1, retry.TokenLimit, retry.TokenLimit + 0.001, math.Inf(1), math.NaN()} {
		_, err := retry.NewBudget(retry.BudgetConfig{MaxTokens: 10, TokenRatio: r})
		require.Equal(t, err == nil, grpcclient.CheckRetryBudgetTokenRatio(r) == nil, "TokenRatio %v", r)
	}
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	d := grpcclient.DefaultSettings()
	require.NotNil(t, d.Classify)
	withoutClassify := d
	withoutClassify.Classify = nil
	require.Equal(t, grpcclient.Settings{
		Addr:        "",
		TLS:         nil,
		Timeout:     10 * time.Second,
		MaxAttempts: 3,
		BackoffBase: 100 * time.Millisecond,
		BackoffMax:  5 * time.Second,
		RetryBudget: retry.BudgetConfig{MaxTokens: 10, TokenRatio: 0.1},
	}, withoutClassify)

	t.Run("passes the checks of the fields that have a default", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, grpcclient.CheckTLS(d.TLS))
		require.NoError(t, grpcclient.CheckTimeout(d.Timeout))
		require.NoError(t, grpcclient.CheckMaxAttempts(d.MaxAttempts))
		require.NoError(t, grpcclient.CheckBackoffBase(d.BackoffBase))
		require.NoError(t, grpcclient.CheckBackoffMax(d.BackoffMax))
		require.NoError(t, grpcclient.CheckRetryBudgetMaxTokens(d.RetryBudget.MaxTokens))
		require.NoError(t, grpcclient.CheckRetryBudgetTokenRatio(d.RetryBudget.TokenRatio))
		_, err := retry.NewBudget(d.RetryBudget)
		require.NoError(t, err, "retry.NewBudget accepts the default budget")
	})

	t.Run("fails the check of Addr, which has none", func(t *testing.T) {
		t.Parallel()
		require.Error(t, grpcclient.CheckAddr(d.Addr))
	})
}

// newSetup returns a Setup on a new humustest Host, for a test that builds
// a Client and makes no RPC.
func newSetup(t *testing.T) *humus.Setup {
	t.Helper()
	return &humus.Setup{Host: humustest.NewHost(t).Host()}
}

func TestNewClient_the_Setup(t *testing.T) {
	t.Parallel()

	settings := grpcclient.DefaultSettings()
	settings.Addr = "carts:9090"

	t.Run("a nil Setup is rejected", func(t *testing.T) {
		t.Parallel()
		c, err := grpcclient.NewClient(nil, settings)
		require.Nil(t, c)
		require.EqualError(t, err, "grpcclient: NewClient: the Setup must not be nil")
	})

	t.Run("a Host with no Dial is rejected, rather than dialling the real network", func(t *testing.T) {
		t.Parallel()
		c, err := grpcclient.NewClient(&humus.Setup{}, settings)
		require.Nil(t, c)
		require.EqualError(t, err, "grpcclient: NewClient: the Setup's Host.Dial must not be nil")
	})

	t.Run("alongside the bad fields", func(t *testing.T) {
		t.Parallel()
		_, err := grpcclient.NewClient(&humus.Setup{}, grpcclient.DefaultSettings())
		require.ErrorContains(t, err, "the Setup's Host.Dial must not be nil")
		require.ErrorContains(t, err, `Settings.Addr "" is not host:port`)
	})
}

func TestNewClient_settings(t *testing.T) {
	t.Parallel()

	t.Run("DefaultSettings alone fails, naming Addr", func(t *testing.T) {
		t.Parallel()
		c, err := grpcclient.NewClient(newSetup(t), grpcclient.DefaultSettings())
		require.Nil(t, c)
		require.EqualError(t, err, `grpcclient: NewClient: Settings.Addr "" is not host:port`)
	})

	t.Run("every bad field is named in one error", func(t *testing.T) {
		t.Parallel()
		settings := grpcclient.DefaultSettings()
		settings.Addr = "carts:9090"
		settings.TLS = &tls.Config{MaxVersion: tls.VersionTLS11}
		settings.Timeout = 0
		settings.MaxAttempts = 0
		settings.BackoffBase = 0
		settings.BackoffMax = -time.Second
		settings.RetryBudget = retry.BudgetConfig{MaxTokens: 2, TokenRatio: 0}
		settings.Classify = nil

		c, err := grpcclient.NewClient(newSetup(t), settings)
		require.Nil(t, c)
		require.ErrorContains(t, err, "grpcclient: NewClient: ")
		require.ErrorContains(t, err, "Settings.TLS has a MaxVersion of TLS 1.1")
		require.ErrorContains(t, err, "Settings.Timeout must be positive")
		require.ErrorContains(t, err, "Settings.MaxAttempts must be at least 1, got 0")
		require.ErrorContains(t, err, "Settings.BackoffBase must be positive")
		require.ErrorContains(t, err, "Settings.BackoffMax must be positive")
		require.ErrorContains(t, err, "Settings.RetryBudget.MaxTokens must be from 3 to 65536, got 2")
		require.ErrorContains(t, err, "Settings.RetryBudget.TokenRatio must be from 0.001 to 65536, got 0")
		require.ErrorContains(t, err, "Settings.Classify must not be nil")
		require.NotContains(t, err.Error(), "Settings.Addr")
	})

	t.Run("a BackoffMax below BackoffBase is rejected, next to the other bad fields", func(t *testing.T) {
		t.Parallel()
		settings := grpcclient.DefaultSettings()
		settings.Addr = "carts:9090"
		settings.BackoffBase = 2 * time.Second
		settings.BackoffMax = time.Second
		settings.Classify = nil

		c, err := grpcclient.NewClient(newSetup(t), settings)
		require.Nil(t, c)
		require.EqualError(t, err, "grpcclient: NewClient: Settings.BackoffMax must be at least Settings.BackoffBase, 2s, got 1s\n"+
			"Settings.Classify must not be nil")
	})

	t.Run("a BackoffMax equal to BackoffBase passes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			settings := grpcclient.DefaultSettings()
			settings.Addr = "carts:9090"
			settings.BackoffBase = time.Second
			settings.BackoffMax = time.Second

			// Init fails after NewClient, so the App's OnStop functions
			// close the connection.
			errInit := errors.New("init failed")
			var newErr error
			_, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				_, newErr = grpcclient.NewClient(s, settings)
				return nil, errInit
			}, humustest.WithApp(humus.WithHost(humustest.NewHost(t).Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
			st := <-exit
			require.ErrorIs(t, st.Cause(), errInit)
			require.NoError(t, newErr)
		})
	})

	t.Run("each field is named", func(t *testing.T) {
		t.Parallel()
		for field, change := range map[string]func(*grpcclient.Settings){
			"Settings.Addr":                   func(s *grpcclient.Settings) { s.Addr = "dns:///carts:9090" },
			"Settings.TLS":                    func(s *grpcclient.Settings) { s.TLS = &tls.Config{MaxVersion: tls.VersionTLS10} },
			"Settings.Timeout":                func(s *grpcclient.Settings) { s.Timeout = -time.Second },
			"Settings.MaxAttempts":            func(s *grpcclient.Settings) { s.MaxAttempts = 0 },
			"Settings.BackoffBase":            func(s *grpcclient.Settings) { s.BackoffBase = 0 },
			"Settings.BackoffMax":             func(s *grpcclient.Settings) { s.BackoffMax = 0 },
			"Settings.RetryBudget.MaxTokens":  func(s *grpcclient.Settings) { s.RetryBudget.MaxTokens = retry.TokenLimit + 1 },
			"Settings.RetryBudget.TokenRatio": func(s *grpcclient.Settings) { s.RetryBudget.TokenRatio = math.NaN() },
			"Settings.Classify":               func(s *grpcclient.Settings) { s.Classify = nil },
		} {
			settings := grpcclient.DefaultSettings()
			settings.Addr = "carts:9090"
			change(&settings)
			_, err := grpcclient.NewClient(newSetup(t), settings)
			require.ErrorContains(t, err, field)
		}
	})

	t.Run("DefaultSettings with an Addr passes, and dials nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			host, dials := recordDials(h)
			settings := grpcclient.DefaultSettings()
			settings.Addr = "carts:9090"

			// Init fails after NewClient, so the App's OnStop functions
			// close the connection.
			errInit := errors.New("init failed")
			var (
				c      *grpcclient.Client
				newErr error
			)
			_, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				c, newErr = grpcclient.NewClient(s, settings)
				return nil, errInit
			}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))
			st := <-exit
			require.ErrorIs(t, st.Cause(), errInit)

			require.NoError(t, newErr)
			require.NotNil(t, c)
			require.Empty(t, dials.all(), "NewClient dialled")
		})
	})

	t.Run("an option grpc-go rejects fails NewClient", func(t *testing.T) {
		t.Parallel()
		settings := grpcclient.DefaultSettings()
		settings.Addr = "carts:9090"
		c, err := grpcclient.NewClient(newSetup(t), settings, grpc.WithDefaultServiceConfig("not JSON"))
		require.Nil(t, c)
		require.ErrorContains(t, err, "grpcclient: NewClient: ")
	})
}
