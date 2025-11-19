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
	"os"
	"path/filepath"
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

func TestBuildTLSConfig_NilConfig(t *testing.T) {
	t.Run("will return nil", func(t *testing.T) {
		t.Run("when config is nil", func(t *testing.T) {
			cfg, err := buildTLSConfig(nil)
			require.NoError(t, err)
			require.Nil(t, cfg)
		})
	})
}

func TestBuildTLSConfig_WithInMemoryData(t *testing.T) {
	caCert, clientCert, clientKey := generateTestCertificates(t)

	t.Run("will create valid tls.Config", func(t *testing.T) {
		t.Run("with client cert and CA from memory", func(t *testing.T) {
			tlsCfg := &TLSConfig{
				CertData: clientCert,
				KeyData:  clientKey,
				CAData:   caCert,
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.NoError(t, err)
			require.NotNil(t, cfg)
			require.Len(t, cfg.Certificates, 1)
			require.NotNil(t, cfg.RootCAs)
		})
	})
}

func TestBuildTLSConfig_WithFiles(t *testing.T) {
	caCert, clientCert, clientKey := generateTestCertificates(t)

	// Create temporary directory for test certificates
	tmpDir := t.TempDir()
	caCertFile := filepath.Join(tmpDir, "ca.pem")
	clientCertFile := filepath.Join(tmpDir, "client-cert.pem")
	clientKeyFile := filepath.Join(tmpDir, "client-key.pem")

	require.NoError(t, os.WriteFile(caCertFile, caCert, 0600))
	require.NoError(t, os.WriteFile(clientCertFile, clientCert, 0600))
	require.NoError(t, os.WriteFile(clientKeyFile, clientKey, 0600))

	t.Run("will create valid tls.Config", func(t *testing.T) {
		t.Run("with client cert and CA from files", func(t *testing.T) {
			tlsCfg := &TLSConfig{
				CertFile: clientCertFile,
				KeyFile:  clientKeyFile,
				CAFile:   caCertFile,
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.NoError(t, err)
			require.NotNil(t, cfg)
			require.Len(t, cfg.Certificates, 1)
			require.NotNil(t, cfg.RootCAs)
		})
	})
}

func TestBuildTLSConfig_WithTLSVersions(t *testing.T) {
	caCert, clientCert, clientKey := generateTestCertificates(t)

	t.Run("will set MinVersion and MaxVersion", func(t *testing.T) {
		t.Run("when specified in config", func(t *testing.T) {
			tlsCfg := &TLSConfig{
				CertData:   clientCert,
				KeyData:    clientKey,
				CAData:     caCert,
				MinVersion: tls.VersionTLS12,
				MaxVersion: tls.VersionTLS13,
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.NoError(t, err)
			require.NotNil(t, cfg)
			require.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
			require.Equal(t, uint16(tls.VersionTLS13), cfg.MaxVersion)
		})
	})
}

func TestBuildTLSConfig_WithServerName(t *testing.T) {
	caCert, clientCert, clientKey := generateTestCertificates(t)

	t.Run("will set ServerName", func(t *testing.T) {
		t.Run("when specified in config", func(t *testing.T) {
			tlsCfg := &TLSConfig{
				CertData:   clientCert,
				KeyData:    clientKey,
				CAData:     caCert,
				ServerName: "kafka.example.com",
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.NoError(t, err)
			require.NotNil(t, cfg)
			require.Equal(t, "kafka.example.com", cfg.ServerName)
		})
	})
}

func TestBuildTLSConfig_OnlyCA(t *testing.T) {
	caCert, _, _ := generateTestCertificates(t)

	t.Run("will create valid tls.Config", func(t *testing.T) {
		t.Run("with only CA certificate", func(t *testing.T) {
			tlsCfg := &TLSConfig{
				CAData: caCert,
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.NoError(t, err)
			require.NotNil(t, cfg)
			require.Empty(t, cfg.Certificates)
			require.NotNil(t, cfg.RootCAs)
		})
	})
}

func TestBuildTLSConfig_ErrorCases(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("will return error", func(t *testing.T) {
		t.Run("when cert file does not exist", func(t *testing.T) {
			tlsCfg := &TLSConfig{
				CertFile: filepath.Join(tmpDir, "nonexistent-cert.pem"),
				KeyFile:  filepath.Join(tmpDir, "nonexistent-key.pem"),
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.Error(t, err)
			require.Nil(t, cfg)
			require.Contains(t, err.Error(), "failed to read client certificate file")
		})

		t.Run("when key file does not exist", func(t *testing.T) {
			_, clientCert, _ := generateTestCertificates(t)
			certFile := filepath.Join(tmpDir, "cert.pem")
			require.NoError(t, os.WriteFile(certFile, clientCert, 0600))

			tlsCfg := &TLSConfig{
				CertFile: certFile,
				KeyFile:  filepath.Join(tmpDir, "nonexistent-key.pem"),
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.Error(t, err)
			require.Nil(t, cfg)
			require.Contains(t, err.Error(), "failed to read client key file")
		})

		t.Run("when CA file does not exist", func(t *testing.T) {
			tlsCfg := &TLSConfig{
				CAFile: filepath.Join(tmpDir, "nonexistent-ca.pem"),
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.Error(t, err)
			require.Nil(t, cfg)
			require.Contains(t, err.Error(), "failed to read CA certificate file")
		})

		t.Run("when certificate and key do not match", func(t *testing.T) {
			_, cert1, _ := generateTestCertificates(t)
			_, _, key2 := generateTestCertificates(t)

			tlsCfg := &TLSConfig{
				CertData: cert1,
				KeyData:  key2,
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.Error(t, err)
			require.Nil(t, cfg)
			require.Contains(t, err.Error(), "failed to load client certificate and key")
		})

		t.Run("when CA certificate is invalid", func(t *testing.T) {
			tlsCfg := &TLSConfig{
				CAData: []byte("invalid ca certificate"),
			}

			cfg, err := buildTLSConfig(tlsCfg)
			require.Error(t, err)
			require.Nil(t, cfg)
			require.Contains(t, err.Error(), "failed to parse CA certificate")
		})
	})
}

func TestWithTLS_Option(t *testing.T) {
	caCert, clientCert, clientKey := generateTestCertificates(t)

	t.Run("will set TLS config in options", func(t *testing.T) {
		t.Run("when WithTLS option is used", func(t *testing.T) {
			tlsCfg := TLSConfig{
				CertData: clientCert,
				KeyData:  clientKey,
				CAData:   caCert,
			}

			opts := &Options{}
			WithTLS(tlsCfg)(opts)

			require.NotNil(t, opts.tlsConfig)
			require.Equal(t, clientCert, opts.tlsConfig.CertData)
			require.Equal(t, clientKey, opts.tlsConfig.KeyData)
			require.Equal(t, caCert, opts.tlsConfig.CAData)
		})
	})
}

func TestNewRuntime_WithTLS(t *testing.T) {
	caCert, clientCert, clientKey := generateTestCertificates(t)

	t.Run("will include TLS config in runtime", func(t *testing.T) {
		t.Run("when created with WithTLS option", func(t *testing.T) {
			tlsCfg := TLSConfig{
				CertData: clientCert,
				KeyData:  clientKey,
				CAData:   caCert,
			}

			// Create a simple processor for testing
			processor := testProcessor{}

			runtime := NewRuntime(
				[]string{"localhost:9092"},
				"test-group",
				WithTLS(tlsCfg),
				AtLeastOnce("test-topic", processor),
			)

			require.NotNil(t, runtime.tlsConfig)
			require.Equal(t, clientCert, runtime.tlsConfig.CertData)
			require.Equal(t, clientKey, runtime.tlsConfig.KeyData)
			require.Equal(t, caCert, runtime.tlsConfig.CAData)
		})
	})
}

// testProcessor is a simple test processor implementation
type testProcessor struct{}

func (testProcessor) Process(ctx context.Context, msg Message) error {
	return nil
}
