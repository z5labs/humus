// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

// A certificate is valid from notBefore before the time it is made to
// notAfter after it.
const (
	notBefore = time.Hour
	notAfter  = 24 * time.Hour
)

// CA is a certificate authority generated for one test. Its key, serial
// number and validity come from crypto/rand and the clock when NewCA is
// called. Nothing about it is fixed.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// NewCA generates a CA with a new random ECDSA P-256 key and a random
// 128-bit serial number. It is valid from an hour before time.Now() to a
// day after. Inside a synctest bubble that is the bubble's clock, so build
// the CA inside the bubble that uses it. NewCA fails the test through t
// if generation fails.
func NewCA(t testing.TB) *CA {
	t.Helper()
	ca, err := newCA()
	if err != nil {
		t.Fatalf("humustest: NewCA: %v", err)
	}
	return ca
}

func newCA() (*CA, error) {
	key, serial, err := newKeyAndSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "humustest CA " + serial.Text(16)},
		NotBefore:             now.Add(-notBefore),
		NotAfter:              now.Add(notAfter),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	cert, err := create(tmpl, tmpl, key, key)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key}, nil
}

// Pool returns a CertPool holding only the CA's certificate, for a
// client's tls.Config.RootCAs.
func (ca *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// Issue returns a certificate for names, which are DNS names or IP
// literals, signed by the CA, with a new random key and serial number.
func (ca *CA) Issue(t testing.TB, names ...string) *Certificate {
	t.Helper()
	c, err := ca.issue(names)
	if err != nil {
		t.Fatalf("humustest: Issue(%q): %v", names, err)
	}
	return c
}

func (ca *CA) issue(names []string) (*Certificate, error) {
	key, serial, err := newKeyAndSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    now.Add(-notBefore),
		NotAfter:     now.Add(notAfter),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	if len(names) > 0 {
		tmpl.Subject.CommonName = names[0]
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, name)
	}
	leaf, err := create(tmpl, ca.cert, key, ca.key)
	if err != nil {
		return nil, err
	}
	return &Certificate{leaf: leaf, key: key, ca: ca}, nil
}

// Certificate is a leaf certificate issued by a CA.
type Certificate struct {
	leaf *x509.Certificate
	key  *ecdsa.PrivateKey
	ca   *CA
}

// TLS returns the certificate and its chain, for a server's
// tls.Config.Certificates.
func (c *Certificate) TLS() tls.Certificate {
	return tls.Certificate{
		Certificate: [][]byte{c.leaf.Raw, c.ca.cert.Raw},
		PrivateKey:  c.key,
		Leaf:        c.leaf,
	}
}

// PKCS12 encodes the certificate, its key and the CA's certificate as a
// PKCS#12 bundle, encrypted with a new password from crypto/rand.Text.
// It returns the bundle and the password. Each call makes a new password.
func (c *Certificate) PKCS12(t testing.TB) (bundle []byte, password string) {
	t.Helper()
	password = rand.Text()
	bundle, err := pkcs12.Modern.Encode(c.key, c.leaf, []*x509.Certificate{c.ca.cert}, password)
	if err != nil {
		t.Fatalf("humustest: PKCS12: %v", err)
	}
	return bundle, password
}

// newKeyAndSerial generates an ECDSA P-256 key and a positive 128-bit
// serial number from crypto/rand.
func newKeyAndSerial() (*ecdsa.PrivateKey, *big.Int, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		serial, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, nil, err
		}
		// RFC 5280 requires a positive serial number.
		if serial.Sign() > 0 {
			return key, serial, nil
		}
	}
}

// create signs tmpl for key's public key with signer, as parent, and parses
// the result.
func create(tmpl, parent *x509.Certificate, key, signer *ecdsa.PrivateKey) (*x509.Certificate, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}
