// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/telemetry"
)

// errDialled is what the Host's DialTCP returns in these tests, which send
// nothing.
var errDialled = errors.New("dialled")

// newSetup returns a Setup with telemetry that discards everything, and no
// Host, and a network on a test host whose DialTCP records that it was
// called.
func newSetup(t *testing.T, dialled *bool) (*humus.Setup, Network) {
	n := hosttest.Seams{Network: hosttest.New(t)}
	n.OnDialTCP = func(context.Context, string, string) (net.Conn, error) {
		*dialled = true
		return nil, errDialled
	}
	return &humus.Setup{Telemetry: telemetry.Noop()}, n
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestNewClient_the_client(t *testing.T) {
	t.Parallel()

	roots := x509.NewCertPool()
	settings := DefaultSettings()
	settings.BaseURL = mustParse(t, "https://payments:8443")
	settings.TLS = &tls.Config{RootCAs: roots, ServerName: "payments"}
	settings.Timeout = 7 * time.Second

	var dialled bool
	s, n := newSetup(t, &dialled)
	c, err := NewClient(s, n, settings)
	require.NoError(t, err)
	require.False(t, dialled, "NewClient dials nothing")
	_, err = c.Get("https://payments:8443/")
	require.ErrorIs(t, err, errDialled)
	require.True(t, dialled, "the client NewClient returned dials through the Network")
	dialled = false

	require.Equal(t, 7*time.Second, c.Timeout)
	require.Nil(t, c.Jar)
	require.NotNil(t, c.CheckRedirect)

	tr := newTransport(n, settings)
	require.NotNil(t, tr.DialContext)
	_, err = tr.DialContext(t.Context(), "tcp", "payments:8443")
	require.ErrorIs(t, err, errDialled)
	require.True(t, dialled, "the transport dials with the Network's DialTCP")

	require.Nil(t, tr.Proxy, "the transport reads no proxy variable")
	require.Equal(t, 100, tr.MaxIdleConns)
	require.Equal(t, 100, tr.MaxIdleConnsPerHost)
	require.Equal(t, 90*time.Second, tr.IdleConnTimeout)

	require.NotSame(t, settings.TLS, tr.TLSClientConfig, "the transport dials with a clone")
	require.Same(t, roots, tr.TLSClientConfig.RootCAs)
	require.Equal(t, "payments", tr.TLSClientConfig.ServerName)

	require.NotNil(t, tr.Protocols)
	require.True(t, tr.Protocols.HTTP2())
	require.True(t, tr.Protocols.HTTP1())
	require.False(t, tr.Protocols.UnencryptedHTTP2())
}

func TestNewClient_an_http_BaseURL_is_HTTP1_alone(t *testing.T) {
	t.Parallel()

	settings := DefaultSettings()
	settings.BaseURL = mustParse(t, "http://payments:8080")
	var dialled bool
	s, n := newSetup(t, &dialled)
	_, err := NewClient(s, n, settings)
	require.NoError(t, err)

	tr := newTransport(n, settings)
	require.Nil(t, tr.TLSClientConfig)
	require.True(t, tr.Protocols.HTTP1())
	require.False(t, tr.Protocols.HTTP2())
	require.False(t, tr.Protocols.UnencryptedHTTP2())
	require.False(t, dialled)
}

// closeRecorder is a request body that records being closed.
type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

func TestRetryTransport_refuses_another_origin(t *testing.T) {
	t.Parallel()

	settings := DefaultSettings()
	settings.BaseURL = mustParse(t, "https://payments:8443/v1")
	settings.TLS = &tls.Config{}
	var dialled bool
	s, n := newSetup(t, &dialled)
	c, err := NewClient(s, n, settings)
	require.NoError(t, err)

	body := &closeRecorder{Reader: strings.NewReader("{}")}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://payments/v1/charges", body)
	require.NoError(t, err)
	resp, err := c.Transport.RoundTrip(req)
	require.Nil(t, resp)
	require.EqualError(t, err, "httpclient: request to https://payments:443 on a client for https://payments:8443")
	require.True(t, body.closed, "the refused request's body is closed")
	require.False(t, dialled)
}
