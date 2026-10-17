// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

import (
	"crypto/tls"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/humustest"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
// Settings.TLS is checked by tlsField, since "TLS 1.2" names a version.
var fieldNames = []string{"Settings", "Addr", "address"}

// tlsField matches "TLS" where it is not the start of a version, such as
// "TLS 1.2".
var tlsField = regexp.MustCompile(`TLS(?:[^ _]|$)|TLS [^1]`)

// certificate is a server certificate from a CA generated for the test.
func certificate(t *testing.T) []tls.Certificate {
	t.Helper()
	return []tls.Certificate{humustest.NewCA(t).Issue(t, "localhost").TLS()}
}

// requireUnnamed fails the test if err names a field of Settings.
func requireUnnamed(t *testing.T, err error, msgAndArgs ...any) {
	t.Helper()
	for _, name := range fieldNames {
		require.NotContains(t, err.Error(), name, msgAndArgs...)
	}
	require.NotRegexp(t, tlsField, err.Error(), msgAndArgs...)
}

func TestCheckAddr(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		addr  string
		valid bool
	}{
		{name: "an empty host", addr: ":9090", valid: true},
		{name: "port 0", addr: ":0", valid: true},
		{name: "a host name", addr: "localhost:9090", valid: true},
		{name: "the highest port", addr: "0.0.0.0:65535", valid: true},
		{name: "an IPv6 host", addr: "[::1]:443", valid: true},
		{name: "every IPv6 address on port 0", addr: "[::]:0", valid: true},
		{name: "the empty string", addr: ""},
		{name: "a host with no port", addr: "localhost"},
		{name: "a bare IPv6 address", addr: "::1"},
		{name: "an empty port", addr: "localhost:"},
		{name: "a port above 65535", addr: ":65536"},
		{name: "a negative port", addr: ":-1"},
		{name: "a named port", addr: ":grpc"},
		{name: "a URL", addr: "https://localhost:9090"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := grpcserver.CheckAddr(tc.addr)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			requireUnnamed(t, err, "the error names a field")
		})
	}
}

func TestCheckTLS(t *testing.T) {
	t.Parallel()

	certs := certificate(t)
	getCertificate := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &certs[0], nil }
	getConfigForClient := func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return &tls.Config{Certificates: certs}, nil
	}
	// Neither of the two suites HTTP/2 requires below TLS 1.3, which
	// CheckTLS doesn't look at.
	nonHTTP2Suites := []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384}

	testCases := []struct {
		name  string
		c     *tls.Config
		valid bool
	}{
		{name: "Certificates", c: &tls.Config{Certificates: certs}, valid: true},
		{name: "only GetCertificate", c: &tls.Config{GetCertificate: getCertificate}, valid: true},
		{name: "only GetConfigForClient", c: &tls.Config{GetConfigForClient: getConfigForClient}, valid: true},
		{name: "a MaxVersion of TLS 1.2", c: &tls.Config{Certificates: certs, MaxVersion: tls.VersionTLS12}, valid: true},
		{name: "a MaxVersion of TLS 1.3", c: &tls.Config{Certificates: certs, MaxVersion: tls.VersionTLS13}, valid: true},
		{name: "a client certificate required", c: &tls.Config{
			Certificates: certs,
			ClientAuth:   tls.RequireAndVerifyClientCert,
		}, valid: true},
		{name: "any CipherSuites", c: &tls.Config{Certificates: certs, CipherSuites: nonHTTP2Suites}, valid: true},
		{name: "any CipherSuites at TLS 1.2", c: &tls.Config{
			Certificates: certs,
			MinVersion:   tls.VersionTLS12,
			MaxVersion:   tls.VersionTLS12,
			CipherSuites: nonHTTP2Suites,
		}, valid: true},
		{name: "nil", c: nil},
		{name: "an empty config", c: &tls.Config{}},
		{name: "an empty Certificates", c: &tls.Config{Certificates: []tls.Certificate{}, MinVersion: tls.VersionTLS13}},
		{name: "a MaxVersion of TLS 1.1", c: &tls.Config{Certificates: certs, MaxVersion: tls.VersionTLS11}},
		{name: "a MaxVersion of TLS 1.0", c: &tls.Config{GetCertificate: getCertificate, MaxVersion: tls.VersionTLS10}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := grpcserver.CheckTLS(tc.c)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			requireUnnamed(t, err, "the error names a field")
		})
	}
}

func TestSettings_zero_value_fails_both_checks(t *testing.T) {
	t.Parallel()

	var s grpcserver.Settings

	err := grpcserver.CheckAddr(s.Addr)
	require.Error(t, err)
	requireUnnamed(t, err, "the error names a field")

	err = grpcserver.CheckTLS(s.TLS)
	require.Error(t, err)
	requireUnnamed(t, err, "the error names a field")
}
