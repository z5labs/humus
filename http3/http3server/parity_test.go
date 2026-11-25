// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server_test

import (
	"context"
	"crypto/tls"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http/httpserver"
	"github.com/z5labs/humus/http3/http3server"
)

// The package doc promises an end user serving both transports that the two
// packages agree: the same defaults, the same address rules, and one TLS
// config for both. These tests hold it to that, so a change to httpserver
// that breaks a promise fails here. Only the tests import httpserver.

func TestDefaultSettings_are_httpservers(t *testing.T) {
	t.Parallel()

	h3, h := http3server.DefaultSettings(), httpserver.DefaultSettings()
	require.Equal(t, h.Addr, h3.Addr, "Addr")
	require.Equal(t, h.TLS, h3.TLS, "TLS")
	require.Equal(t, h.ReadHeaderTimeout, h3.ReadHeaderTimeout, "ReadHeaderTimeout")
	require.Equal(t, h.ReadTimeout, h3.ReadTimeout, "ReadTimeout")
	require.Equal(t, h.WriteTimeout, h3.WriteTimeout, "WriteTimeout")
	require.Equal(t, h.IdleTimeout, h3.IdleTimeout, "IdleTimeout")
	require.Equal(t, h.MaxHeaderBytes, h3.MaxHeaderBytes, "MaxHeaderBytes")
	require.Nil(t, h.TrustScenario, "httpserver's TrustScenario")
	require.Nil(t, h3.TrustScenario, "http3server's TrustScenario")
}

func TestDefaults_are_httpservers(t *testing.T) {
	t.Parallel()

	var h3, h humus.App[humus.Host]
	require.NoError(t, h3.Apply(http3server.Defaults()...))
	require.NoError(t, h.Apply(httpserver.Defaults()...))
	require.Equal(t, h.ShutdownGracePeriod, h3.ShutdownGracePeriod, "ShutdownGracePeriod")
	require.Equal(t, drainDelay(t, h.Drain), drainDelay(t, h3.Drain), "the drain's delay")
}

func TestCheckAddr_agrees_with_httpservers(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{
		":8443", ":0", "localhost:8443", "0.0.0.0:65535", "[::1]:443", "[::]:0",
		"", "localhost", "::1", "localhost:", ":65536", ":-1", ":https", "https://localhost:8443",
	} {
		require.Equal(t, httpserver.CheckAddr(addr) == nil, http3server.CheckAddr(addr) == nil, "%q", addr)
	}
}

func TestCheckTLS_passes_one_config_for_both_transports(t *testing.T) {
	t.Parallel()

	c := &tls.Config{Certificates: certificate(t), MinVersion: tls.VersionTLS12}
	require.NoError(t, httpserver.CheckTLS(c))
	require.NoError(t, http3server.CheckTLS(c))
}

// drainDelay runs d inside a synctest bubble, with a context that is never
// done, and returns how long it waited.
func drainDelay(t *testing.T, d humus.Drain) time.Duration {
	t.Helper()
	require.NotNil(t, d)
	var took time.Duration
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		require.NoError(t, d(context.Background(), &humus.Setup{}, nil))
		took = time.Since(start)
	})
	return took
}
