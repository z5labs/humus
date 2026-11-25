// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server_test

import (
	"crypto/tls"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/humustest"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
// Settings.TLS is checked by tlsField, since "TLS 1.3" names a version.
var fieldNames = []string{
	"Settings", "Addr", "ReadHeaderTimeout", "ReadTimeout",
	"WriteTimeout", "IdleTimeout", "MaxHeaderBytes",
	"address", "timeout", "header",
}

// tlsField matches "TLS" where it is not the start of a version, such as
// "TLS 1.3".
var tlsField = regexp.MustCompile(`TLS(?:[^ ]|$)|TLS [^1]`)

// certificate is a server certificate from a CA generated for the test.
func certificate(t *testing.T) []tls.Certificate {
	t.Helper()
	return []tls.Certificate{humustest.NewCA(t).Issue(t, "localhost").TLS()}
}

func TestChecks(t *testing.T) {
	t.Parallel()

	certs := certificate(t)
	getCertificate := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &certs[0], nil }
	getConfigForClient := func(*tls.ClientHelloInfo) (*tls.Config, error) { return &tls.Config{Certificates: certs}, nil }

	timeout := func(check func(time.Duration) error) (accept, reject []func() error) {
		accept = []func() error{
			func() error { return check(0) },
			func() error { return check(time.Nanosecond) },
			func() error { return check(time.Hour) },
		}
		reject = []func() error{
			func() error { return check(-time.Nanosecond) },
			func() error { return check(-time.Second) },
		}
		return accept, reject
	}

	type check struct {
		name   string
		accept []func() error
		reject []func() error
	}
	checks := []check{
		{
			name: "CheckAddr",
			accept: []func() error{
				func() error { return http3server.CheckAddr(":8443") },
				func() error { return http3server.CheckAddr(":0") },
				func() error { return http3server.CheckAddr("localhost:8443") },
				func() error { return http3server.CheckAddr("0.0.0.0:65535") },
				func() error { return http3server.CheckAddr("[::1]:443") },
				func() error { return http3server.CheckAddr("[::]:0") },
			},
			reject: []func() error{
				func() error { return http3server.CheckAddr("") },
				func() error { return http3server.CheckAddr("localhost") },
				func() error { return http3server.CheckAddr("::1") },
				func() error { return http3server.CheckAddr("localhost:") },
				func() error { return http3server.CheckAddr(":65536") },
				func() error { return http3server.CheckAddr(":-1") },
				func() error { return http3server.CheckAddr(":https") },
				func() error { return http3server.CheckAddr("https://localhost:8443") },
			},
		},
		{
			name: "CheckTLS",
			accept: []func() error{
				func() error { return http3server.CheckTLS(&tls.Config{Certificates: certs}) },
				func() error { return http3server.CheckTLS(&tls.Config{GetCertificate: getCertificate}) },
				func() error { return http3server.CheckTLS(&tls.Config{GetConfigForClient: getConfigForClient}) },
				func() error {
					return http3server.CheckTLS(&tls.Config{Certificates: certs, MaxVersion: tls.VersionTLS13})
				},
				func() error {
					return http3server.CheckTLS(&tls.Config{Certificates: certs, MinVersion: tls.VersionTLS13})
				},
				func() error {
					return http3server.CheckTLS(&tls.Config{Certificates: certs, MinVersion: tls.VersionTLS12})
				},
			},
			reject: []func() error{
				func() error { return http3server.CheckTLS(nil) },
				func() error { return http3server.CheckTLS(&tls.Config{}) },
				func() error {
					return http3server.CheckTLS(&tls.Config{Certificates: []tls.Certificate{}, MinVersion: tls.VersionTLS13})
				},
				func() error {
					return http3server.CheckTLS(&tls.Config{Certificates: certs, MaxVersion: tls.VersionTLS12})
				},
				func() error {
					return http3server.CheckTLS(&tls.Config{GetCertificate: getCertificate, MaxVersion: tls.VersionTLS10})
				},
				func() error {
					return http3server.CheckTLS(&tls.Config{GetConfigForClient: getConfigForClient, MaxVersion: tls.VersionTLS11})
				},
			},
		},
		{
			name: "CheckMaxHeaderBytes",
			accept: []func() error{
				func() error { return http3server.CheckMaxHeaderBytes(1) },
				func() error { return http3server.CheckMaxHeaderBytes(1 << 20) },
			},
			reject: []func() error{
				func() error { return http3server.CheckMaxHeaderBytes(0) },
				func() error { return http3server.CheckMaxHeaderBytes(-1) },
			},
		},
	}
	for _, tc := range []struct {
		name  string
		check func(time.Duration) error
	}{
		{"CheckReadHeaderTimeout", http3server.CheckReadHeaderTimeout},
		{"CheckReadTimeout", http3server.CheckReadTimeout},
		{"CheckWriteTimeout", http3server.CheckWriteTimeout},
		{"CheckIdleTimeout", http3server.CheckIdleTimeout},
	} {
		accept, reject := timeout(tc.check)
		checks = append(checks, check{name: tc.name, accept: accept, reject: reject})
	}

	for _, c := range checks {
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

func TestCheckTLS_names_the_MaxVersion_it_rejects(t *testing.T) {
	t.Parallel()

	err := http3server.CheckTLS(&tls.Config{Certificates: certificate(t), MaxVersion: tls.VersionTLS12})
	require.EqualError(t, err, "has a MaxVersion of TLS 1.2, below the TLS 1.3 that QUIC requires")
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	d := http3server.DefaultSettings()
	require.Equal(t, http3server.Settings{
		Addr:              "",
		TLS:               nil,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TrustScenario:     nil,
	}, d)

	t.Run("passes the checks of the fields that have a default", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, http3server.CheckReadHeaderTimeout(d.ReadHeaderTimeout))
		require.NoError(t, http3server.CheckReadTimeout(d.ReadTimeout))
		require.NoError(t, http3server.CheckWriteTimeout(d.WriteTimeout))
		require.NoError(t, http3server.CheckIdleTimeout(d.IdleTimeout))
		require.NoError(t, http3server.CheckMaxHeaderBytes(d.MaxHeaderBytes))
	})

	t.Run("fails the checks of Addr and TLS, which have none", func(t *testing.T) {
		t.Parallel()
		require.Error(t, http3server.CheckAddr(d.Addr))
		require.Error(t, http3server.CheckTLS(d.TLS))
	})
}
