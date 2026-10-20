// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package certfile serves and presents a certificate from its PEM file, read again when it changes.
package certfile

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"sync"

	"github.com/z5labs/humus"
)

func Config(fsys humus.FS, name string) (*tls.Config, error) {
	f := &file{fs: fsys, name: name}
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

type file struct {
	fs   humus.FS
	name string

	mu   sync.Mutex
	pem  []byte
	cert *tls.Certificate
}

func (f *file) certificate() (*tls.Certificate, error) {
	data, err := readFile(f.fs, f.name)
	if err != nil {
		return nil, err
	}
	if cert := f.cached(data); cert != nil {
		return cert, nil
	}

	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, fmt.Errorf("certificate and key %s: %w", f.name, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.pem, f.cert = data, &cert
	return &cert, nil
}

func (f *file) cached(data []byte) *tls.Certificate {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cert != nil && bytes.Equal(data, f.pem) {
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
