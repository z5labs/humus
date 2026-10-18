// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/telemetry"
)

// TestNewClient_the_transport checks the Client's http.Transport directly.
// net/http reads the proxy variables once per process, so
// TestClient_uses_no_proxy can't catch a transport that consulted them in a
// binary where something read them earlier; a nil Proxy is what guards that,
// whatever the order the tests run in.
func TestNewClient_the_transport(t *testing.T) {
	t.Parallel()

	dial := func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no network")
	}
	s := &humus.Setup{Host: humus.Host{Dial: dial}, Telemetry: telemetry.Noop()}
	c, err := NewClient(s, Connection{Server: "apiserver:6443", TLS: &tls.Config{}})
	require.NoError(t, err)

	tr := c.conns
	require.Nil(t, tr.Proxy, "the transport reads no proxy from the environment")
	require.NotNil(t, tr.DialContext)
	require.NotNil(t, tr.Protocols)
	require.True(t, tr.Protocols.HTTP2(), "the transport offers HTTP/2")
	require.True(t, tr.Protocols.HTTP1(), "the transport offers HTTP/1.1")
	require.NotNil(t, tr.HTTP2)
	require.Equal(t, 30*time.Second, tr.HTTP2.SendPingTimeout)
	require.Equal(t, 15*time.Second, tr.HTTP2.PingTimeout)
	require.Equal(t, "apiserver", tr.TLSClientConfig.ServerName)
}
