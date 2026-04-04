// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"time"

	"github.com/z5labs/bedrock/config"
)

// buildTLSConfig returns a config.Reader[*tls.Config] that loads a TLS config
// from the cert and key files when both readers return a value, or falls back
// to generating a self-signed certificate.
func buildTLSConfig(certFile, keyFile config.Reader[string]) config.Reader[*tls.Config] {
	fileBased := buildFileTLSConfig(certFile, keyFile)
	selfSigned := buildSelfSignedTLSConfig()
	return config.Or(fileBased, selfSigned)
}

// buildFileTLSConfig returns a config.Reader[*tls.Config] that reads cert and key
// from the provided file path readers. It returns no value if either path is unset.
func buildFileTLSConfig(certFile, keyFile config.Reader[string]) config.Reader[*tls.Config] {
	return config.ReaderFunc[*tls.Config](func(ctx context.Context) (config.Value[*tls.Config], error) {
		cert, err := config.Read(ctx, certFile)
		if err != nil {
			return config.Value[*tls.Config]{}, nil
		}

		key, err := config.Read(ctx, keyFile)
		if err != nil {
			return config.Value[*tls.Config]{}, nil
		}

		tlsCert, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return config.Value[*tls.Config]{}, err
		}

		return config.ValueOf(&tls.Config{
			Certificates: []tls.Certificate{tlsCert},
		}), nil
	})
}

// buildSelfSignedTLSConfig returns a config.Reader[*tls.Config] that generates
// a self-signed ECDSA P-256 certificate for localhost at read time.
// The certificate is valid for 24 hours and includes SANs for localhost,
// 127.0.0.1, and ::1.
func buildSelfSignedTLSConfig() config.Reader[*tls.Config] {
	return config.ReaderFunc[*tls.Config](func(ctx context.Context) (config.Value[*tls.Config], error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return config.Value[*tls.Config]{}, err
		}

		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return config.Value[*tls.Config]{}, err
		}

		now := time.Now()
		template := x509.Certificate{
			SerialNumber: serial,
			Subject: pkix.Name{
				CommonName: "localhost",
			},
			NotBefore:             now,
			NotAfter:              now.Add(24 * time.Hour),
			KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
			DNSNames:              []string{"localhost"},
			IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		}

		certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
		if err != nil {
			return config.Value[*tls.Config]{}, err
		}

		tlsCert := tls.Certificate{
			Certificate: [][]byte{certDER},
			PrivateKey:  key,
		}

		return config.ValueOf(&tls.Config{
			Certificates: []tls.Certificate{tlsCert},
		}), nil
	})
}
