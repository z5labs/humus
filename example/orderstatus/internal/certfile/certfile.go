// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package certfile serves a certificate from its files, read again when they change.
package certfile

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"sync"

	"github.com/z5labs/humus"
)

func Config(fsys humus.FS, certFile, keyFile string) (*tls.Config, error) {
	f := &files{fs: fsys, certFile: certFile, keyFile: keyFile}
	if _, err := f.certificate(); err != nil {
		return nil, err
	}
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return f.certificate()
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return f.certificate()
		},
	}, nil
}

type files struct {
	fs       humus.FS
	certFile string
	keyFile  string

	mu      sync.Mutex
	certPEM []byte
	keyPEM  []byte
	cert    *tls.Certificate
}

func (f *files) certificate() (*tls.Certificate, error) {
	certPEM, err := readFile(f.fs, f.certFile)
	if err != nil {
		return nil, err
	}
	keyPEM := certPEM
	if f.keyFile != f.certFile {
		keyPEM, err = readFile(f.fs, f.keyFile)
		if err != nil {
			return nil, err
		}
	}
	if cert := f.cached(certPEM, keyPEM); cert != nil {
		return cert, nil
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate %s and key %s: %w", f.certFile, f.keyFile, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.certPEM, f.keyPEM, f.cert = certPEM, keyPEM, &cert
	return &cert, nil
}

func (f *files) cached(certPEM, keyPEM []byte) *tls.Certificate {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cert != nil && bytes.Equal(certPEM, f.certPEM) && bytes.Equal(keyPEM, f.keyPEM) {
		return f.cert
	}
	return nil
}

func readFile(fsys humus.FS, name string) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}
