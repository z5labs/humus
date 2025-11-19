// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// generateTestCertificates generates a test CA, server cert, and client cert
// for testing TLS functionality.
func generateTestCertificates(t *testing.T) (caCert, clientCert, clientKey []byte) {
	t.Helper()

	// Generate CA
	caPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test CA"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caPrivKey.PublicKey, caPrivKey)
	require.NoError(t, err)

	caCert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})

	// Generate client certificate
	clientPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"Test Client"},
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientCertDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientPrivKey.PublicKey, caPrivKey)
	require.NoError(t, err)

	clientCert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientCertDER})
	clientKey = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(clientPrivKey),
	})

	return caCert, clientCert, clientKey
}

func TestWithTLS_NilConfig(t *testing.T) {
	t.Run("will accept nil config", func(t *testing.T) {
		t.Run("for unencrypted connections", func(t *testing.T) {
			opts := &Options{}
			WithTLS(nil)(opts)

			require.Nil(t, opts.tlsConfig)
		})
	})
}

func TestWithTLS_ValidConfig(t *testing.T) {
	caCert, clientCert, clientKey := generateTestCertificates(t)

	t.Run("will set TLS config", func(t *testing.T) {
		t.Run("with client cert and CA", func(t *testing.T) {
			cert, err := tls.X509KeyPair(clientCert, clientKey)
			require.NoError(t, err)

			caCertPool := x509.NewCertPool()
			require.True(t, caCertPool.AppendCertsFromPEM(caCert))

			tlsConfig := &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caCertPool,
				MinVersion:   tls.VersionTLS12,
			}

			opts := &Options{}
			WithTLS(tlsConfig)(opts)

			require.NotNil(t, opts.tlsConfig)
			require.Equal(t, tlsConfig, opts.tlsConfig)
			require.Len(t, opts.tlsConfig.Certificates, 1)
			require.NotNil(t, opts.tlsConfig.RootCAs)
			require.Equal(t, uint16(tls.VersionTLS12), opts.tlsConfig.MinVersion)
		})
	})
}

func TestWithTLS_ServerName(t *testing.T) {
	t.Run("will set ServerName for SNI", func(t *testing.T) {
		t.Run("when specified in config", func(t *testing.T) {
			tlsConfig := &tls.Config{
				ServerName: "kafka.example.com",
				MinVersion: tls.VersionTLS12,
			}

			opts := &Options{}
			WithTLS(tlsConfig)(opts)

			require.NotNil(t, opts.tlsConfig)
			require.Equal(t, "kafka.example.com", opts.tlsConfig.ServerName)
		})
	})
}

func TestWithTLS_OnlyCA(t *testing.T) {
	caCert, _, _ := generateTestCertificates(t)

	t.Run("will create valid config", func(t *testing.T) {
		t.Run("with only CA certificate", func(t *testing.T) {
			caCertPool := x509.NewCertPool()
			require.True(t, caCertPool.AppendCertsFromPEM(caCert))

			tlsConfig := &tls.Config{
				RootCAs:    caCertPool,
				MinVersion: tls.VersionTLS12,
			}

			opts := &Options{}
			WithTLS(tlsConfig)(opts)

			require.NotNil(t, opts.tlsConfig)
			require.Empty(t, opts.tlsConfig.Certificates)
			require.NotNil(t, opts.tlsConfig.RootCAs)
		})
	})
}

func TestNewRuntime_WithTLS(t *testing.T) {
	caCert, clientCert, clientKey := generateTestCertificates(t)

	t.Run("will include TLS config in runtime", func(t *testing.T) {
		t.Run("when created with WithTLS option", func(t *testing.T) {
			cert, err := tls.X509KeyPair(clientCert, clientKey)
			require.NoError(t, err)

			caCertPool := x509.NewCertPool()
			require.True(t, caCertPool.AppendCertsFromPEM(caCert))

			tlsConfig := &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caCertPool,
				MinVersion:   tls.VersionTLS12,
			}

			// Create a simple processor for testing
			processor := testProcessor{}

			runtime := NewRuntime(
				[]string{"localhost:9092"},
				"test-group",
				WithTLS(tlsConfig),
				AtLeastOnce("test-topic", processor),
			)

			require.NotNil(t, runtime.tlsConfig)
			require.Equal(t, tlsConfig, runtime.tlsConfig)
			require.Len(t, runtime.tlsConfig.Certificates, 1)
			require.NotNil(t, runtime.tlsConfig.RootCAs)
		})
	})
}

// testProcessor is a simple test processor implementation
type testProcessor struct{}

func (testProcessor) Process(ctx context.Context, msg Message) error {
	return nil
}
