// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/http3/internal/quicdial"
)

func TestNewTransport(t *testing.T) {
	t.Parallel()

	errDialled := errors.New("dialled")
	var network, address string
	d := quicdial.New(func(_ context.Context, n, a string) (net.Conn, error) {
		network, address = n, a
		return nil, errDialled
	})
	defer d.Close()

	roots := x509.NewCertPool()
	settings := DefaultSettings()
	settings.BaseURL = &url.URL{Scheme: "https", Host: "payments:8443"}
	settings.TLS = &tls.Config{RootCAs: roots, ServerName: "payments"}

	tr := newTransport(d, settings)

	require.NotNil(t, tr.Dial, "quic-go would open a socket of its own and resolve names with net.DefaultResolver")
	_, err := tr.Dial(t.Context(), "payments:8443", &tls.Config{}, nil)
	require.ErrorIs(t, err, errDialled)
	require.Equal(t, "udp", network, "each connection is dialled through the Network's DialUDP, over udp")
	require.Equal(t, "payments:8443", address)

	require.NotNil(t, tr.QUICConfig)
	require.Equal(t, 5*time.Second, tr.QUICConfig.HandshakeIdleTimeout)
	require.Equal(t, 30*time.Second, tr.QUICConfig.MaxIdleTimeout)
	require.Equal(t, 10*time.Second, tr.QUICConfig.KeepAlivePeriod)

	require.NotSame(t, settings.TLS, tr.TLSClientConfig, "the transport dials with a clone")
	require.Same(t, roots, tr.TLSClientConfig.RootCAs)
	require.Equal(t, "payments", tr.TLSClientConfig.ServerName)

	settings.TLS = nil
	require.Nil(t, newTransport(d, settings).TLSClientConfig, "nil means the system's roots")
}

// recordingTransport records the request it is given.
type recordingTransport struct {
	got    *http.Request
	closed bool
}

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.got = r
	return nil, errors.New("sent nothing")
}

func (t *recordingTransport) CloseIdleConnections() { t.closed = true }

func TestProtoTransport(t *testing.T) {
	t.Parallel()

	next := &recordingTransport{}
	tr := protoTransport{next: next}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://payments:8443/", nil)
	require.NoError(t, err)

	_, err = tr.RoundTrip(req)
	require.Error(t, err)
	require.Equal(t, "HTTP/3.0", next.got.Proto)
	require.Equal(t, 3, next.got.ProtoMajor)
	require.Equal(t, 0, next.got.ProtoMinor)
	require.Equal(t, "HTTP/1.1", req.Proto, "the caller's request is not modified")
	require.Same(t, req.Context(), next.got.Context())

	tr.CloseIdleConnections()
	require.True(t, next.closed, "CloseIdleConnections reaches the layers below")
	protoTransport{next: roundTripFunc(nil)}.CloseIdleConnections() // a layer without it is passed over
}

// roundTripFunc is an http.RoundTripper without CloseIdleConnections.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
