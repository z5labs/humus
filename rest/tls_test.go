// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	bedrockconfig "github.com/z5labs/bedrock/config"
	"github.com/stretchr/testify/require"
)

func TestBuildSelfSignedTLSConfig(t *testing.T) {
	t.Run("returns a valid tls.Config", func(t *testing.T) {
		reader := buildSelfSignedTLSConfig()
		val, err := reader.Read(context.Background())
		require.NoError(t, err)

		cfg, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, cfg)
		require.Len(t, cfg.Certificates, 1)
	})

	t.Run("generates a fresh certificate on each read", func(t *testing.T) {
		reader := buildSelfSignedTLSConfig()

		val1, err := reader.Read(context.Background())
		require.NoError(t, err)
		cfg1, _ := val1.Value()

		val2, err := reader.Read(context.Background())
		require.NoError(t, err)
		cfg2, _ := val2.Value()

		require.NotSame(t, cfg1, cfg2)
		require.NotEqual(t, cfg1.Certificates[0].Certificate, cfg2.Certificates[0].Certificate)
	})
}

func TestBuildFileTLSConfig(t *testing.T) {
	t.Run("loads cert and key from files", func(t *testing.T) {
		certFile, keyFile := writeTempCert(t)

		val, err := buildFileTLSConfig(
			bedrockconfig.ReaderOf(certFile),
			bedrockconfig.ReaderOf(keyFile),
		).Read(context.Background())
		require.NoError(t, err)

		cfg, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, cfg)
		require.Len(t, cfg.Certificates, 1)
	})

	t.Run("returns no value when cert file reader is empty", func(t *testing.T) {
		val, err := buildFileTLSConfig(
			bedrockconfig.EmptyReader[string](),
			bedrockconfig.ReaderOf("/some/key.pem"),
		).Read(context.Background())
		require.NoError(t, err)

		_, ok := val.Value()
		require.False(t, ok)
	})

	t.Run("returns no value when key file reader is empty", func(t *testing.T) {
		val, err := buildFileTLSConfig(
			bedrockconfig.ReaderOf("/some/cert.pem"),
			bedrockconfig.EmptyReader[string](),
		).Read(context.Background())
		require.NoError(t, err)

		_, ok := val.Value()
		require.False(t, ok)
	})
}

func TestBuildTLSConfig(t *testing.T) {
	t.Run("falls back to self-signed when no file readers are set", func(t *testing.T) {
		val, err := buildTLSConfig(
			bedrockconfig.EmptyReader[string](),
			bedrockconfig.EmptyReader[string](),
		).Read(context.Background())
		require.NoError(t, err)

		cfg, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, cfg)
		require.Len(t, cfg.Certificates, 1)
	})

	t.Run("uses file cert when both paths are provided", func(t *testing.T) {
		certFile, keyFile := writeTempCert(t)

		val, err := buildTLSConfig(
			bedrockconfig.ReaderOf(certFile),
			bedrockconfig.ReaderOf(keyFile),
		).Read(context.Background())
		require.NoError(t, err)

		cfg, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, cfg)
		require.Len(t, cfg.Certificates, 1)
	})
}

// writeTempCert generates a self-signed cert, writes it to temp files,
// and returns the paths.
func writeTempCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	reader := buildSelfSignedTLSConfig()
	val, err := reader.Read(context.Background())
	require.NoError(t, err)
	cfg, _ := val.Value()
	cert := cfg.Certificates[0]

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Certificate[0],
	})

	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	require.True(t, ok)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0600))
	return certFile, keyFile
}
