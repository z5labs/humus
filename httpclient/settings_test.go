// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"crypto/tls"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
// Settings.TLS is checked by tlsField, since "TLS 1.2" names a version.
var fieldNames = []string{
	"Settings", "BaseURL", "Timeout",
	"base", "timeout",
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
	require.Equal(t, httpclient.Settings{
		BaseURL: nil,
		TLS:     nil,
		Timeout: 10 * time.Second,
	}, d)

	t.Run("passes the checks of the fields that have a default", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, httpclient.CheckTLS(d.TLS))
		require.NoError(t, httpclient.CheckTimeout(d.Timeout))
	})

	t.Run("fails the check of BaseURL, which has none", func(t *testing.T) {
		t.Parallel()
		require.Error(t, httpclient.CheckBaseURL(d.BaseURL))
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
		c, err := httpclient.NewClient(&humus.Setup{}, httpclient.DefaultSettings())
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: Settings.BaseURL must not be nil")
	})

	t.Run("every bad field is named in one error", func(t *testing.T) {
		t.Parallel()
		settings := https(t)
		settings.TLS.InsecureSkipVerify = true
		settings.Timeout = 0

		_, err := httpclient.NewClient(&humus.Setup{}, settings)
		require.ErrorContains(t, err, "httpclient: NewClient: ")
		require.ErrorContains(t, err, "Settings.TLS sets InsecureSkipVerify")
		require.ErrorContains(t, err, "Settings.Timeout must be positive")
		require.NotContains(t, err.Error(), "Settings.BaseURL")
	})

	t.Run("each field is named", func(t *testing.T) {
		t.Parallel()
		for field, change := range map[string]func(*httpclient.Settings){
			"Settings.BaseURL": func(s *httpclient.Settings) { s.BaseURL = mustParse(t, "https://payments:8443/?q=1") },
			"Settings.TLS":     func(s *httpclient.Settings) { s.TLS.MaxVersion = tls.VersionTLS11 },
			"Settings.Timeout": func(s *httpclient.Settings) { s.Timeout = -time.Second },
		} {
			settings := https(t)
			change(&settings)
			_, err := httpclient.NewClient(&humus.Setup{}, settings)
			require.ErrorContains(t, err, field)
		}
	})

	t.Run("an https BaseURL with a nil TLS is rejected", func(t *testing.T) {
		t.Parallel()
		settings := https(t)
		settings.TLS = nil
		c, err := httpclient.NewClient(&humus.Setup{}, settings)
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: Settings.TLS must be set for an https Settings.BaseURL")
	})

	t.Run("an http BaseURL with a TLS config is rejected", func(t *testing.T) {
		t.Parallel()
		settings := https(t)
		settings.BaseURL = mustParse(t, "http://payments:8080")
		c, err := httpclient.NewClient(&humus.Setup{}, settings)
		require.Nil(t, c)
		require.EqualError(t, err, "httpclient: NewClient: Settings.TLS must be nil for an http Settings.BaseURL, which is plain text")
	})

	t.Run("DefaultSettings with an http BaseURL passes", func(t *testing.T) {
		t.Parallel()
		settings := httpclient.DefaultSettings()
		settings.BaseURL = mustParse(t, "http://payments:8080")
		c, err := httpclient.NewClient(&humus.Setup{}, settings)
		require.NoError(t, err)
		require.NotNil(t, c)
		require.Equal(t, 10*time.Second, c.Timeout)
		require.Nil(t, c.Jar)
	})

	t.Run("DefaultSettings with an https BaseURL and a TLS config passes", func(t *testing.T) {
		t.Parallel()
		c, err := httpclient.NewClient(&humus.Setup{}, https(t))
		require.NoError(t, err)
		require.NotNil(t, c)
	})
}
