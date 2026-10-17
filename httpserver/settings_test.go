// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver_test

import (
	"crypto/tls"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
// Settings.TLS is checked by tlsField, since "TLS 1.2" names a version.
var fieldNames = []string{
	"Settings", "Addr", "HTTP2Only", "ReadHeaderTimeout", "ReadTimeout",
	"WriteTimeout", "IdleTimeout", "MaxHeaderBytes",
	"address", "timeout", "header",
}

// tlsField matches "TLS" where it is not the start of a version, such as
// "TLS 1.2", or of a cipher suite's name.
var tlsField = regexp.MustCompile(`TLS(?:[^ _]|$)|TLS [^1]`)

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
	nonHTTP2Suites := []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384}

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
				func() error { return httpserver.CheckAddr(":8443") },
				func() error { return httpserver.CheckAddr(":0") },
				func() error { return httpserver.CheckAddr("localhost:8443") },
				func() error { return httpserver.CheckAddr("0.0.0.0:65535") },
				func() error { return httpserver.CheckAddr("[::1]:443") },
				func() error { return httpserver.CheckAddr("[::]:0") },
			},
			reject: []func() error{
				func() error { return httpserver.CheckAddr("") },
				func() error { return httpserver.CheckAddr("localhost") },
				func() error { return httpserver.CheckAddr("::1") },
				func() error { return httpserver.CheckAddr("localhost:") },
				func() error { return httpserver.CheckAddr(":65536") },
				func() error { return httpserver.CheckAddr(":-1") },
				func() error { return httpserver.CheckAddr(":https") },
				func() error { return httpserver.CheckAddr("https://localhost:8443") },
			},
		},
		{
			name: "CheckTLS",
			accept: []func() error{
				func() error { return httpserver.CheckTLS(&tls.Config{Certificates: certs}) },
				func() error { return httpserver.CheckTLS(&tls.Config{GetCertificate: getCertificate}) },
				func() error { return httpserver.CheckTLS(&tls.Config{GetConfigForClient: getConfigForClient}) },
				func() error {
					return httpserver.CheckTLS(&tls.Config{Certificates: certs, MaxVersion: tls.VersionTLS12})
				},
				func() error {
					return httpserver.CheckTLS(&tls.Config{Certificates: certs, MaxVersion: tls.VersionTLS13})
				},
				func() error {
					return httpserver.CheckTLS(&tls.Config{
						Certificates: certs,
						CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
					})
				},
				func() error {
					return httpserver.CheckTLS(&tls.Config{
						Certificates: certs,
						MinVersion:   tls.VersionTLS12,
						CipherSuites: append(nonHTTP2Suites, tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
					})
				},
				func() error {
					return httpserver.CheckTLS(&tls.Config{
						Certificates: certs,
						MinVersion:   tls.VersionTLS13,
						CipherSuites: nonHTTP2Suites,
					})
				},
			},
			reject: []func() error{
				func() error { return httpserver.CheckTLS(nil) },
				func() error { return httpserver.CheckTLS(&tls.Config{}) },
				func() error {
					return httpserver.CheckTLS(&tls.Config{Certificates: []tls.Certificate{}, MinVersion: tls.VersionTLS13})
				},
				func() error {
					return httpserver.CheckTLS(&tls.Config{Certificates: certs, MaxVersion: tls.VersionTLS11})
				},
				func() error {
					return httpserver.CheckTLS(&tls.Config{GetCertificate: getCertificate, MaxVersion: tls.VersionTLS10})
				},
				func() error {
					return httpserver.CheckTLS(&tls.Config{Certificates: certs, CipherSuites: nonHTTP2Suites})
				},
				func() error {
					return httpserver.CheckTLS(&tls.Config{
						Certificates: certs,
						MinVersion:   tls.VersionTLS12,
						CipherSuites: nonHTTP2Suites,
					})
				},
			},
		},
		{
			name: "CheckMaxHeaderBytes",
			accept: []func() error{
				func() error { return httpserver.CheckMaxHeaderBytes(1) },
				func() error { return httpserver.CheckMaxHeaderBytes(1 << 20) },
			},
			reject: []func() error{
				func() error { return httpserver.CheckMaxHeaderBytes(0) },
				func() error { return httpserver.CheckMaxHeaderBytes(-1) },
			},
		},
	}
	for _, tc := range []struct {
		name  string
		check func(time.Duration) error
	}{
		{"CheckReadHeaderTimeout", httpserver.CheckReadHeaderTimeout},
		{"CheckReadTimeout", httpserver.CheckReadTimeout},
		{"CheckWriteTimeout", httpserver.CheckWriteTimeout},
		{"CheckIdleTimeout", httpserver.CheckIdleTimeout},
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

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	d := httpserver.DefaultSettings()
	require.Equal(t, httpserver.Settings{
		Addr:              "",
		TLS:               nil,
		HTTP2Only:         false,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}, d)

	t.Run("passes the checks of the fields that have a default", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, httpserver.CheckReadHeaderTimeout(d.ReadHeaderTimeout))
		require.NoError(t, httpserver.CheckReadTimeout(d.ReadTimeout))
		require.NoError(t, httpserver.CheckWriteTimeout(d.WriteTimeout))
		require.NoError(t, httpserver.CheckIdleTimeout(d.IdleTimeout))
		require.NoError(t, httpserver.CheckMaxHeaderBytes(d.MaxHeaderBytes))
	})

	t.Run("fails the checks of Addr and TLS, which have none", func(t *testing.T) {
		t.Parallel()
		require.Error(t, httpserver.CheckAddr(d.Addr))
		require.Error(t, httpserver.CheckTLS(d.TLS))
	})
}
