// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus/http/httpclient"
	"github.com/z5labs/humus/http3/http3client"
	"github.com/z5labs/humus/humustest"
)

// The package doc promises that each of http3client's fields means what
// httpclient's field of the same name means, has the same default, and has
// a check with the same rules, except where HTTP/3 differs. These tests hold
// it to that, so a change to httpclient that breaks a promise fails here.
// Only the tests import httpclient.

func TestSettings_has_httpclients_fields(t *testing.T) {
	t.Parallel()

	h3, h := reflect.TypeFor[http3client.Settings](), reflect.TypeFor[httpclient.Settings]()
	require.Equal(t, h.NumField(), h3.NumField(), "the number of fields")
	for i := range h.NumField() {
		hf, h3f := h.Field(i), h3.Field(i)
		require.Equal(t, hf.Name, h3f.Name, "field %d", i)
		if hf.Name == "Classify" {
			continue
		}
		require.Equal(t, hf.Type, h3f.Type, "Settings.%s", hf.Name)
	}

	c3, c := reflect.TypeFor[http3client.Classification](), reflect.TypeFor[httpclient.Classification]()
	require.Equal(t, c.NumField(), c3.NumField(), "the number of Classification's fields")
	for i := range c.NumField() {
		require.Equal(t, c.Field(i).Name, c3.Field(i).Name, "Classification's field %d", i)
		require.Equal(t, c.Field(i).Type, c3.Field(i).Type, "Classification.%s", c.Field(i).Name)
	}
}

func TestDefaultSettings_are_httpclients(t *testing.T) {
	t.Parallel()

	h3, h := http3client.DefaultSettings(), httpclient.DefaultSettings()
	require.Equal(t, h.BaseURL, h3.BaseURL, "BaseURL")
	require.Equal(t, h.TLS, h3.TLS, "TLS")
	require.Equal(t, h.Timeout, h3.Timeout, "Timeout")
	require.Equal(t, h.AttemptTimeout, h3.AttemptTimeout, "AttemptTimeout")
	require.Equal(t, h.MaxAttempts, h3.MaxAttempts, "MaxAttempts")
	require.Equal(t, h.BackoffBase, h3.BackoffBase, "BackoffBase")
	require.Equal(t, h.BackoffMax, h3.BackoffMax, "BackoffMax")
	require.Equal(t, h.RetryBudget, h3.RetryBudget, "RetryBudget")
	require.Equal(t, h.Breaker, h3.Breaker, "Breaker")
	require.Equal(t, h.ForwardScenario, h3.ForwardScenario, "ForwardScenario")

	for failures := range 25 {
		for successes := range 25 {
			for consecutive := range min(failures, successes) + 1 {
				counts := circuit.Counts{
					Successes:            successes,
					Failures:             failures,
					ConsecutiveFailures:  consecutive,
					ConsecutiveSuccesses: consecutive,
				}
				require.Equal(t, h.BreakerPolicy.Trip(counts), h3.BreakerPolicy.Trip(counts), "Trip(%+v)", counts)
				require.Equal(t, h.BreakerPolicy.Probe(counts), h3.BreakerPolicy.Probe(counts), "Probe(%+v)", counts)
			}
		}
	}
}

func TestDefaultSettings_Classify_agrees_with_httpclients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h3, h := http3client.DefaultSettings().Classify, httpclient.DefaultSettings().Classify
		now := time.Now()

		retryAfters := []string{
			"", "0", "7", " 2 ", "soon", "-5", "+5", "1.5", "99999999999999999999",
			now.Add(30 * time.Second).UTC().Format(http.TimeFormat),
			now.Add(-time.Hour).UTC().Format(http.TimeFormat),
			now.Add(time.Minute).UTC().Format(time.RFC850),
		}
		for code := 100; code < 600; code++ {
			for _, after := range retryAfters {
				respond := func() *http.Response {
					resp := &http.Response{StatusCode: code, Header: http.Header{}, Body: unreadBody{t: t}}
					if after != "" {
						resp.Header.Set("Retry-After", after)
					}
					return resp
				}
				got, want := h3(respond(), nil), h(respond(), nil)
				require.Equal(t, want.Retry, got.Retry, "%d, Retry-After %q", code, after)
				require.Equal(t, want.RetryAfter, got.RetryAfter, "%d, Retry-After %q", code, after)
				require.Equal(t, want.Breaker, got.Breaker, "%d, Retry-After %q", code, after)
			}
		}

		for _, err := range []error{
			&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			fmt.Errorf("handshake: %w", x509.HostnameError{Host: "payments"}),
			x509.CertificateInvalidError{Reason: x509.Expired},
			x509.UnknownAuthorityError{},
			errors.New("connection refused"),
			fmt.Errorf("the attempt had no response: %w", context.DeadlineExceeded),
			io.ErrUnexpectedEOF,
		} {
			got, want := h3(nil, err), h(nil, err)
			require.Equal(t, want.Retry, got.Retry, "%v", err)
			require.Equal(t, want.RetryAfter, got.RetryAfter, "%v", err)
			require.Equal(t, want.Breaker, got.Breaker, "%v", err)
		}
	})
}

func TestCheckBaseURL_rejects_what_httpclients_rejects_and_http(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"https://payments:8443", "https://payments", "https://payments:8443/v1/",
		"https://[::1]:8443", "http://payments:8080", "http://payments",
		"payments:8443", "/v1", "ftp://payments", "https://", "https://:8443",
		"https:payments", "https://user:secret@payments:8443",
		"https://payments:8443/?region=eu", "https://payments:8443/?",
		"https://payments:8443/#charges", "http://user@payments", "http://payments/?q",
	} {
		u := parse(t, raw)
		want := httpclient.CheckBaseURL(u) == nil && u.Scheme == "https"
		require.Equal(t, want, http3client.CheckBaseURL(u) == nil, "%q", raw)
	}
	require.Error(t, httpclient.CheckBaseURL(nil))
	require.Error(t, http3client.CheckBaseURL(nil))
}

func TestCheckTLS_passes_one_config_for_both_transports(t *testing.T) {
	t.Parallel()

	c := &tls.Config{RootCAs: humustest.NewCA(t).Pool(), MinVersion: tls.VersionTLS12}
	require.NoError(t, httpclient.CheckTLS(c))
	require.NoError(t, http3client.CheckTLS(c))
}

func TestChecks_agree_with_httpclients(t *testing.T) {
	t.Parallel()

	durations := []time.Duration{math.MinInt64, -time.Second, -time.Nanosecond, 0, time.Nanosecond, time.Second, time.Hour, math.MaxInt64}
	for _, c := range []struct {
		name  string
		h, h3 func(time.Duration) error
	}{
		{"CheckTimeout", httpclient.CheckTimeout, http3client.CheckTimeout},
		{"CheckAttemptTimeout", httpclient.CheckAttemptTimeout, http3client.CheckAttemptTimeout},
		{"CheckBackoffBase", httpclient.CheckBackoffBase, http3client.CheckBackoffBase},
		{"CheckBackoffMax", httpclient.CheckBackoffMax, http3client.CheckBackoffMax},
		{"CheckBreakerOpenFor", httpclient.CheckBreakerOpenFor, http3client.CheckBreakerOpenFor},
		{"CheckBreakerProbeTimeout", httpclient.CheckBreakerProbeTimeout, http3client.CheckBreakerProbeTimeout},
	} {
		for _, d := range durations {
			require.Equal(t, c.h(d) == nil, c.h3(d) == nil, "%s(%v)", c.name, d)
		}
	}

	ints := []int{math.MinInt, -1, 0, 1, 2, 3, 10, retry.TokenLimit, retry.TokenLimit + 1, circuit.MaxCount, circuit.MaxCount + 1, math.MaxInt}
	for _, c := range []struct {
		name  string
		h, h3 func(int) error
	}{
		{"CheckMaxAttempts", httpclient.CheckMaxAttempts, http3client.CheckMaxAttempts},
		{"CheckRetryBudgetMaxTokens", httpclient.CheckRetryBudgetMaxTokens, http3client.CheckRetryBudgetMaxTokens},
		{"CheckBreakerWindow", httpclient.CheckBreakerWindow, http3client.CheckBreakerWindow},
		{"CheckBreakerProbes", httpclient.CheckBreakerProbes, http3client.CheckBreakerProbes},
	} {
		for _, n := range ints {
			require.Equal(t, c.h(n) == nil, c.h3(n) == nil, "%s(%d)", c.name, n)
		}
	}

	for _, r := range []float64{math.Inf(-1), -0.1, 0, 0.0009, 0.001, 0.1, 1, retry.TokenLimit, retry.TokenLimit + 0.001, math.Inf(1), math.NaN()} {
		require.Equal(t, httpclient.CheckRetryBudgetTokenRatio(r) == nil, http3client.CheckRetryBudgetTokenRatio(r) == nil, "CheckRetryBudgetTokenRatio(%v)", r)
	}
}
