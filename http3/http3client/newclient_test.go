// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/http3client"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

func TestNewClient_the_Setup(t *testing.T) {
	t.Parallel()

	settings := http3client.DefaultSettings()
	settings.BaseURL = mustParse(t, "https://payments:8443")

	t.Run("a nil Setup is rejected", func(t *testing.T) {
		t.Parallel()
		c, err := http3client.NewClient(nil, humustest.NewHost(t), settings)
		require.Nil(t, c)
		require.EqualError(t, err, "http3client: NewClient: the Setup must not be nil")
	})

	t.Run("a Telemetry with no LoggerProvider is rejected, rather than panic", func(t *testing.T) {
		t.Parallel()
		tel := telemetry.Noop()
		tel.LoggerProvider = nil
		c, err := http3client.NewClient(&humus.Setup{Telemetry: tel}, humustest.NewHost(t), settings)
		require.Nil(t, c)
		require.EqualError(t, err, "http3client: NewClient: the Setup's Telemetry.LoggerProvider must not be nil")
	})

	t.Run("a Telemetry with no MeterProvider is rejected, rather than panic", func(t *testing.T) {
		t.Parallel()
		tel := telemetry.Noop()
		tel.MeterProvider = nil
		c, err := http3client.NewClient(&humus.Setup{Telemetry: tel}, humustest.NewHost(t), settings)
		require.Nil(t, c)
		require.EqualError(t, err, "http3client: NewClient: the Setup's Telemetry.MeterProvider must not be nil")
	})

	t.Run("alongside the bad fields", func(t *testing.T) {
		t.Parallel()
		_, err := http3client.NewClient(&humus.Setup{}, humustest.NewHost(t), http3client.DefaultSettings())
		require.ErrorContains(t, err, "the Setup's Telemetry.LoggerProvider must not be nil")
		require.ErrorContains(t, err, "the Setup's Telemetry.MeterProvider must not be nil")
		require.ErrorContains(t, err, "Settings.BaseURL must not be nil")
	})
}

func TestNewClient_settings(t *testing.T) {
	t.Parallel()

	newSetup := func() *humus.Setup { return &humus.Setup{Telemetry: telemetry.Noop()} }

	t.Run("DefaultSettings alone fails, naming BaseURL", func(t *testing.T) {
		t.Parallel()
		c, err := http3client.NewClient(newSetup(), humustest.NewHost(t), http3client.DefaultSettings())
		require.Nil(t, c)
		require.EqualError(t, err, "http3client: NewClient: Settings.BaseURL must not be nil")
	})

	t.Run("DefaultSettings with a BaseURL pass, and dial nothing", func(t *testing.T) {
		t.Parallel()
		settings := http3client.DefaultSettings()
		settings.BaseURL = mustParse(t, "https://payments:8443")
		var dialled bool
		n := networkFunc(func() { dialled = true })
		c, err := http3client.NewClient(newSetup(), n, settings)
		require.NoError(t, err)
		require.Equal(t, 10*time.Second, c.Timeout)
		require.Nil(t, c.Jar)
		require.False(t, dialled)
	})

	t.Run("every bad field is named in one error", func(t *testing.T) {
		t.Parallel()
		settings := http3client.DefaultSettings()
		settings.BaseURL = mustParse(t, "http://payments:8080")
		settings.TLS = &tls.Config{MaxVersion: tls.VersionTLS12}
		settings.Timeout = 0
		settings.AttemptTimeout = 0
		settings.MaxAttempts = 0
		settings.BackoffBase = 0
		settings.BackoffMax = -time.Second
		settings.RetryBudget = retry.BudgetConfig{MaxTokens: 2, TokenRatio: 0}
		settings.Classify = nil
		settings.Breaker = circuit.Config{Window: 0, Probes: circuit.MaxCount + 1, OpenFor: 0, ProbeTimeout: -time.Second}
		settings.BreakerPolicy = circuit.Policy{}

		c, err := http3client.NewClient(newSetup(), humustest.NewHost(t), settings)
		require.Nil(t, c)
		require.ErrorContains(t, err, "http3client: NewClient: ")
		for _, want := range []string{
			"Settings.BaseURL has the scheme http, not https",
			"Settings.TLS has a MaxVersion of TLS 1.2",
			"Settings.Timeout must be positive",
			"Settings.AttemptTimeout must be positive",
			"Settings.MaxAttempts must be at least 1",
			"Settings.BackoffBase must be positive",
			"Settings.BackoffMax must be positive",
			"Settings.RetryBudget.MaxTokens must be from 3",
			"Settings.RetryBudget.TokenRatio must be from 0.001",
			"Settings.Classify must not be nil",
			"Settings.Breaker.Window must be from 1 to 65536, got 0",
			"Settings.Breaker.Probes must be from 1 to 65536, got 65537",
			"Settings.Breaker.OpenFor must be positive",
			"Settings.Breaker.ProbeTimeout must be positive",
			"Settings.BreakerPolicy.Trip must not be nil",
			"Settings.BreakerPolicy.Probe must not be nil",
		} {
			require.ErrorContains(t, err, want)
		}
	})

	t.Run("a BackoffMax below BackoffBase is rejected, next to the other bad fields", func(t *testing.T) {
		t.Parallel()
		settings := http3client.DefaultSettings()
		settings.BaseURL = mustParse(t, "https://payments:8443")
		settings.BackoffBase = 2 * time.Second
		settings.BackoffMax = time.Second
		settings.Classify = nil

		c, err := http3client.NewClient(newSetup(), humustest.NewHost(t), settings)
		require.Nil(t, c)
		require.EqualError(t, err, "http3client: NewClient: Settings.BackoffMax must be at least Settings.BackoffBase, 2s, got 1s\n"+
			"Settings.Classify must not be nil")
	})
}

// networkFunc is a Network whose DialUDP calls f, and fails.
type networkFunc func()

func (f networkFunc) DialUDP(context.Context, string, string) (net.Conn, error) {
	f()
	return nil, errors.New("the test's network dials nothing")
}
