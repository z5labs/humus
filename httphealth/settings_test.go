// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth_test

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/humustest"
)

// fieldNames are words a check's error must not contain: a check reports
// what is wrong, and whoever calls it names the field or the variable.
var fieldNames = []string{"Settings", "Addr", "TLS", "address"}

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

	checks := []struct {
		name   string
		accept []func() error
		reject []func() error
	}{
		{
			name: "CheckAddr",
			accept: []func() error{
				func() error { return httphealth.CheckAddr(":8443") },
				func() error { return httphealth.CheckAddr(":0") },
				func() error { return httphealth.CheckAddr("localhost:8443") },
				func() error { return httphealth.CheckAddr("0.0.0.0:65535") },
				func() error { return httphealth.CheckAddr("[::1]:443") },
				func() error { return httphealth.CheckAddr("[::]:0") },
			},
			reject: []func() error{
				func() error { return httphealth.CheckAddr("") },
				func() error { return httphealth.CheckAddr("localhost") },
				func() error { return httphealth.CheckAddr("::1") },
				func() error { return httphealth.CheckAddr("localhost:") },
				func() error { return httphealth.CheckAddr(":65536") },
				func() error { return httphealth.CheckAddr(":-1") },
				func() error { return httphealth.CheckAddr(":https") },
				func() error { return httphealth.CheckAddr("https://localhost:8443") },
			},
		},
		{
			name: "CheckTLS",
			accept: []func() error{
				func() error { return httphealth.CheckTLS(&tls.Config{Certificates: certs}) },
				func() error { return httphealth.CheckTLS(&tls.Config{GetCertificate: getCertificate}) },
				func() error { return httphealth.CheckTLS(&tls.Config{GetConfigForClient: getConfigForClient}) },
				func() error {
					return httphealth.CheckTLS(&tls.Config{Certificates: certs, ClientAuth: tls.NoClientCert})
				},
				func() error {
					return httphealth.CheckTLS(&tls.Config{Certificates: certs, ClientAuth: tls.RequestClientCert})
				},
				func() error {
					return httphealth.CheckTLS(&tls.Config{Certificates: certs, ClientAuth: tls.VerifyClientCertIfGiven})
				},
			},
			reject: []func() error{
				func() error { return httphealth.CheckTLS(nil) },
				func() error { return httphealth.CheckTLS(&tls.Config{}) },
				func() error {
					return httphealth.CheckTLS(&tls.Config{Certificates: []tls.Certificate{}, MinVersion: tls.VersionTLS13})
				},
				func() error {
					return httphealth.CheckTLS(&tls.Config{Certificates: certs, ClientAuth: tls.RequireAnyClientCert})
				},
				func() error {
					return httphealth.CheckTLS(&tls.Config{Certificates: certs, ClientAuth: tls.RequireAndVerifyClientCert})
				},
				func() error {
					return httphealth.CheckTLS(&tls.Config{GetCertificate: getCertificate, ClientAuth: tls.RequireAndVerifyClientCert})
				},
			},
		},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for i, accept := range c.accept {
				require.NoError(t, accept(), "valid value %d", i)
			}
			for i, reject := range c.reject {
				err := reject()
				require.Error(t, err, "invalid value %d", i)
				for _, name := range fieldNames {
					require.NotContains(t, err.Error(), name, "the error of invalid value %d names a field", i)
				}
			}
		})
	}
}

func TestCheckTLS_says_the_kubelet_presents_no_client_certificate(t *testing.T) {
	t.Parallel()

	certs := certificate(t)
	for _, auth := range []tls.ClientAuthType{tls.RequireAnyClientCert, tls.RequireAndVerifyClientCert} {
		t.Run(auth.String(), func(t *testing.T) {
			t.Parallel()
			err := httphealth.CheckTLS(&tls.Config{Certificates: certs, ClientAuth: auth})
			require.ErrorContains(t, err, "kubelet presents no client certificate")
		})
	}
}

func TestSettings_zero_value_fails_both_checks(t *testing.T) {
	t.Parallel()

	var s httphealth.Settings
	require.Error(t, httphealth.CheckAddr(s.Addr))
	require.Error(t, httphealth.CheckTLS(s.TLS))
}
