// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3servertest

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"testing"

	"github.com/quic-go/quic-go/http3"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/http3/internal/quicdial"
	"github.com/z5labs/humus/humustest"
)

// Start is humustest.Start with http3server.Defaults applied before opts,
// so a test runs its App with the drain and the grace period that
// http3server.Main gives it in production. Options given with
// humustest.WithApp come after the defaults, and win over them.
func Start[H humus.Network](t testing.TB, h H, init humus.Init[H], opts ...humustest.Option) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	return humustest.Start(t, h, init, append([]humustest.Option{humustest.WithApp(http3server.Defaults()...)}, opts...)...)
}

// NewClient returns an http.Client that dials h over UDP, trusts only the
// certificates in roots, and speaks only HTTP/3. It closes every connection
// it opened when t's test ends, so nothing it started outlives a
// testing/synctest bubble. It works with or without Start. It panics if h
// or roots is nil.
func NewClient(t testing.TB, h *humustest.Host, roots *x509.CertPool) *http.Client {
	if h == nil {
		panic("http3servertest: NewClient called with a nil *humustest.Host")
	}
	if roots == nil {
		panic("http3servertest: NewClient called with a nil *x509.CertPool")
	}
	d := quicdial.New(h.DialUDP)
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots},
		Dial:            d.Dial,
	}
	t.Cleanup(func() {
		_ = tr.Close()
		d.Close()
	})
	return &http.Client{Transport: tr}
}
