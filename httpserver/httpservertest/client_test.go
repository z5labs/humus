// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpservertest_test

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// hello answers every request with "hello".
var hello = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "hello")
})

func TestNewClient_speaks_only_HTTP2(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, status := httpservertest.Start(t, serves(ca.Issue(t, "localhost"), hello),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening("localhost:8443")

		r := <-get(httpservertest.NewClient(h, ca.Pool()), "https://localhost:8443/")
		require.NoError(t, r.err)
		require.Equal(t, 2, r.proto)
		require.Equal(t, "hello", r.body)

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})

	t.Run("and fails against a server that offers only HTTP/1.1", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			l, err := h.Listen("tcp", ":8443")
			require.NoError(t, err)

			var http1 http.Protocols
			http1.SetHTTP1(true)
			srv := &http.Server{
				Handler:   hello,
				Protocols: &http1,
				TLSConfig: &tls.Config{
					Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()},
					NextProtos:   []string{"http/1.1"},
				},
				ErrorLog: log.New(io.Discard, "", 0),
			}
			served := make(chan error, 1)
			go func() { served <- srv.ServeTLS(l, "", "") }()

			r := <-get(httpservertest.NewClient(h, ca.Pool()), "https://localhost:8443/")
			require.ErrorContains(t, r.err, "no application protocol", "the client offers only h2, which the server does not")

			require.NoError(t, srv.Close())
			require.ErrorIs(t, <-served, http.ErrServerClosed)
		})
	})

	t.Run("and keeps no idle connection", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := httpservertest.NewClient(h, x509.NewCertPool())
			tr, ok := c.Transport.(*http.Transport)
			require.True(t, ok, "Transport = %T, want *http.Transport", c.Transport)
			require.True(t, tr.DisableKeepAlives)
			require.NotNil(t, tr.Protocols)
			require.True(t, tr.Protocols.HTTP2(), "HTTP/2")
			require.False(t, tr.Protocols.HTTP1(), "HTTP/1.1")
			require.False(t, tr.Protocols.UnencryptedHTTP2(), "h2c")

			other, ok := h.HTTPClient().Transport.(*http.Transport)
			require.True(t, ok)
			require.Nil(t, other.TLSClientConfig, "NewClient leaves the Host's other clients unchanged")
			require.Nil(t, other.Protocols, "NewClient leaves the Host's other clients unchanged")
		})
	})
}

func TestNewClient_fails_the_handshake_against_another_CA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		other := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, status := httpservertest.Start(t, serves(ca.Issue(t, "localhost"), hello),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening("localhost:8443")

		r := <-get(httpservertest.NewClient(h, other.Pool()), "https://localhost:8443/")
		require.Error(t, r.err)
		_, ok := errors.AsType[x509.UnknownAuthorityError](r.err)
		require.True(t, ok, "err = %v, want an x509.UnknownAuthorityError", r.err)

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func TestNewClient_works_next_to_humustest_Start(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, status := humustest.Start(t, serves(ca.Issue(t, "localhost"), hello),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening("localhost:8443")

		r := <-get(httpservertest.NewClient(h, ca.Pool()), "https://localhost:8443/")
		require.NoError(t, r.err)
		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, 2, r.proto)
		require.Equal(t, "hello", r.body)

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func TestNewClient_panics_on_nil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		require.PanicsWithValue(t, "httpservertest: NewClient called with a nil *humustest.Host", func() {
			httpservertest.NewClient(nil, x509.NewCertPool())
		})
		require.PanicsWithValue(t, "httpservertest: NewClient called with a nil *x509.CertPool", func() {
			httpservertest.NewClient(h, nil)
		})
	})
}
