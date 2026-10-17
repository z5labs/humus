// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpservertest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"testing"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
)

// Start is humustest.Start with httpserver.Defaults applied before opts, so
// a test runs its App with the drain and the grace period that
// httpserver.Main gives it in production. Options given with
// humustest.WithApp come after the defaults, and win over them.
func Start(t testing.TB, init func(ctx context.Context, s *humus.Setup) ([]humus.Service, error), opts ...humustest.Option) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	return humustest.Start(t, init, append([]humustest.Option{humustest.WithApp(httpserver.Defaults())}, opts...)...)
}

// NewClient returns an http.Client that dials h, trusts only the
// certificates in roots, and speaks only HTTP/2. It is built on
// h.HTTPClient, so it keeps no idle connection. It works with or without
// Start. It panics if h or roots is nil.
func NewClient(h *humustest.Host, roots *x509.CertPool) *http.Client {
	if h == nil {
		panic("httpservertest: NewClient called with a nil *humustest.Host")
	}
	if roots == nil {
		panic("httpservertest: NewClient called with a nil *x509.CertPool")
	}
	c := h.HTTPClient()
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: roots}
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	tr.Protocols = &protocols
	return c
}
