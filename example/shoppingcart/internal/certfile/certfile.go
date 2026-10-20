// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package certfile serves and presents a certificate from its files, read again when they change.
package certfile

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/z5labs/humus"
)

const interval = 30 * time.Second

func Config(fsys humus.FS, certFile, keyFile string) (*tls.Config, error) {
	f := &files{fs: fsys, certFile: certFile, keyFile: keyFile, checked: time.Now()}
	p, err := f.read(nil)
	if err != nil {
		return nil, err
	}
	f.last = p
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return f.certificate(), nil
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return f.certificate(), nil
		},
	}, nil
}

type files struct {
	fs       humus.FS
	certFile string
	keyFile  string

	mu      sync.Mutex
	checked time.Time
	last    *pair
}

type pair struct {
	certPEM []byte
	keyPEM  []byte
	cert    *tls.Certificate
}

func (f *files) certificate() *tls.Certificate {
	f.mu.Lock()
	last := f.last
	if time.Since(f.checked) < interval {
		f.mu.Unlock()
		return last.cert
	}
	f.checked = time.Now()
	f.mu.Unlock()

	p, err := f.read(last)
	if err != nil {
		return last.cert
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = p
	return p.cert
}

func (f *files) read(last *pair) (*pair, error) {
	certPEM, err := readFile(f.fs, f.certFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readFile(f.fs, f.keyFile)
	if err != nil {
		return nil, err
	}
	if last != nil && bytes.Equal(certPEM, last.certPEM) && bytes.Equal(keyPEM, last.keyPEM) {
		return last, nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate %s and key %s: %w", f.certFile, f.keyFile, err)
	}
	return &pair{certPEM: certPEM, keyPEM: keyPEM, cert: &cert}, nil
}

func readFile(fsys humus.FS, name string) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}
