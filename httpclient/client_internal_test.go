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

	"github.com/z5labs/humus"
)

// errDialled is what the Host's Dial returns in these tests, which send
// nothing.
var errDialled = errors.New("dialled")

// newSetup returns a Setup whose Host's Dial records that it was called.
func newSetup(dialled *bool) *humus.Setup {
	return &humus.Setup{Host: humus.Host{
		Dial: func(context.Context, string, string) (net.Conn, error) {
			*dialled = true
			return nil, errDialled
		},
	}}
}

// transportOf returns c's http.Transport.
func transportOf(t *testing.T, c *http.Client) *http.Transport {
	t.Helper()
	ot, ok := c.Transport.(*originTransport)
	require.True(t, ok, "the client's Transport is %T", c.Transport)
	return ot.next
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
	s := newSetup(&dialled)
	c, err := NewClient(s, settings)
	require.NoError(t, err)
	require.False(t, dialled, "NewClient dials nothing")

	require.Equal(t, 7*time.Second, c.Timeout)
	require.Nil(t, c.Jar)
	require.NotNil(t, c.CheckRedirect)

	tr := transportOf(t, c)
	require.NotNil(t, tr.DialContext)
	_, err = tr.DialContext(t.Context(), "tcp", "payments:8443")
	require.ErrorIs(t, err, errDialled)
	require.True(t, dialled, "the transport dials with the Host's Dial")

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
	c, err := NewClient(newSetup(&dialled), settings)
	require.NoError(t, err)

	tr := transportOf(t, c)
	require.Nil(t, tr.TLSClientConfig)
	require.True(t, tr.Protocols.HTTP1())
	require.False(t, tr.Protocols.HTTP2())
	require.False(t, tr.Protocols.UnencryptedHTTP2())
	require.False(t, dialled)
}

func TestOriginOf(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"https://payments":            "https://payments:443",
		"https://payments:443/v1":     "https://payments:443",
		"HTTPS://Payments:8443/a?b=c": "https://payments:8443",
		"http://payments":             "http://payments:80",
		"http://payments:8080":        "http://payments:8080",
		"https://[::1]:8443":          "https://[::1]:8443",
		"https://user:pw@payments":    "https://payments:443",
	} {
		require.Equal(t, want, originOf(mustParse(t, raw)).String(), raw)
	}
	require.Equal(t, originOf(mustParse(t, "https://payments")), originOf(mustParse(t, "https://PAYMENTS:443/x")))
	require.NotEqual(t, originOf(mustParse(t, "https://payments")), originOf(mustParse(t, "http://payments:443")))
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

func TestOriginTransport_refuses_another_origin(t *testing.T) {
	t.Parallel()

	settings := DefaultSettings()
	settings.BaseURL = mustParse(t, "https://payments:8443/v1")
	settings.TLS = &tls.Config{}
	var dialled bool
	c, err := NewClient(newSetup(&dialled), settings)
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
