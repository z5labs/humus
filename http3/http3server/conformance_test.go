// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build conformance

package http3server_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"

	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/http3/internal/quicdial"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// TestConformance_serves_HTTP3_on_the_OS_host is a smoke test on the real
// network: an App on humus.OSHost, whose Service is bound to a loopback
// port the OS picks, serves one request over HTTP/3, from a client that
// dials through humus.OSHost too, and stops on SIGTERM. It is behind the
// conformance tag because it opens real sockets.
func TestConformance_serves_HTTP3_on_the_OS_host(t *testing.T) {
	ca := humustest.NewCA(t)
	settings := http3server.DefaultSettings()
	settings.Addr = "127.0.0.1:0"
	settings.TLS = &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "127.0.0.1").TLS()}}

	bound := make(chan net.Addr, 1)
	init := func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
		svc, err := http3server.NewService(ctx, s, h, settings, hello)
		if err != nil {
			return nil, err
		}
		bound <- svc.Addr()
		return []humus.Service{svc}, nil
	}
	p, status := humustest.Start(t, humus.OSHost(), init,
		humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
	)
	var addr net.Addr
	select {
	case addr = <-bound:
	case st := <-status:
		t.Fatalf("the App exited before it bound its socket: %v", st.Cause())
	}

	host := humus.OSHost()
	d := quicdial.New(host.DialUDP)
	defer d.Close()
	tr := &http3.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}, Dial: d.Dial}
	defer func() { _ = tr.Close() }()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr.String()+"/", nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "HTTP/3.0", resp.Proto)
	require.Equal(t, "hello", string(body))

	<-p.Notified(syscall.SIGTERM)
	p.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.Empty(t, p.Stderr())
}
