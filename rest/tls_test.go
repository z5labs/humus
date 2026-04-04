// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	bedrockconfig "github.com/z5labs/bedrock/config"
	"github.com/stretchr/testify/require"
	"software.sslmate.com/src/go-pkcs12"
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

func TestBuildPKCS12TLSConfig(t *testing.T) {
	t.Run("loads cert and key from PKCS12 file", func(t *testing.T) {
		password := randomPassword(t)
		pkcs12File := writeTempPKCS12(t, password)

		val, err := buildPKCS12TLSConfig(
			bedrockconfig.ReaderOf(pkcs12File),
			bedrockconfig.ReaderOf(password),
		).Read(context.Background())
		require.NoError(t, err)

		cfg, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, cfg)
		require.Len(t, cfg.Certificates, 1)
	})

	t.Run("loads cert and key from PKCS12 file with empty password", func(t *testing.T) {
		pkcs12File := writeTempPKCS12(t, "")

		val, err := buildPKCS12TLSConfig(
			bedrockconfig.ReaderOf(pkcs12File),
			bedrockconfig.EmptyReader[string](),
		).Read(context.Background())
		require.NoError(t, err)

		cfg, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, cfg)
		require.Len(t, cfg.Certificates, 1)
	})

	t.Run("returns no value when file reader is empty", func(t *testing.T) {
		val, err := buildPKCS12TLSConfig(
			bedrockconfig.EmptyReader[string](),
			bedrockconfig.ReaderOf(randomPassword(t)),
		).Read(context.Background())
		require.NoError(t, err)

		_, ok := val.Value()
		require.False(t, ok)
	})

	t.Run("returns error when password is incorrect", func(t *testing.T) {
		correctPassword := randomPassword(t)
		wrongPassword := randomPassword(t)
		pkcs12File := writeTempPKCS12(t, correctPassword)

		_, err := buildPKCS12TLSConfig(
			bedrockconfig.ReaderOf(pkcs12File),
			bedrockconfig.ReaderOf(wrongPassword),
		).Read(context.Background())
		require.Error(t, err)
	})
}

func TestBuildTLSConfig(t *testing.T) {
	t.Run("falls back to self-signed when no file reader is set", func(t *testing.T) {
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

	t.Run("uses PKCS12 cert when file is provided", func(t *testing.T) {
		password := randomPassword(t)
		pkcs12File := writeTempPKCS12(t, password)

		val, err := buildTLSConfig(
			bedrockconfig.ReaderOf(pkcs12File),
			bedrockconfig.ReaderOf(password),
		).Read(context.Background())
		require.NoError(t, err)

		cfg, ok := val.Value()
		require.True(t, ok)
		require.NotNil(t, cfg)
		require.Len(t, cfg.Certificates, 1)
	})
}

// randomPassword generates a cryptographically random password for testing.
func randomPassword(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// writeTempPKCS12 generates a self-signed cert, encodes it as PKCS#12, writes
// it to a temp file, and returns the path.
func writeTempPKCS12(t *testing.T, password string) string {
	t.Helper()

	reader := buildSelfSignedTLSConfig()
	val, err := reader.Read(context.Background())
	require.NoError(t, err)
	cfg, _ := val.Value()
	tlsCert := cfg.Certificates[0]

	cert, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)

	key, ok := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	require.True(t, ok)

	pfxData, err := pkcs12.Modern.Encode(key, cert, nil, password)
	require.NoError(t, err)

	dir := t.TempDir()
	pkcs12File := filepath.Join(dir, "cert.p12")
	require.NoError(t, os.WriteFile(pkcs12File, pfxData, 0600))
	return pkcs12File
}
