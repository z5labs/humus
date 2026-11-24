// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
)

// The real machine's network and filesystem, and the test host's, are a
// humus.IPNetwork and a humus.Filesystem: this fails to compile if either
// interface lists a method one of them lacks.
var (
	_ humus.IPNetwork  = host.IPNetwork{}
	_ humus.IPNetwork  = (*hosttest.Host)(nil)
	_ humus.Filesystem = host.OSFilesystem{}
	_ humus.Filesystem = (*hosttest.Filesystem)(nil)
)

func TestOSHost_is_the_real_network_and_filesystem(t *testing.T) {
	h := humus.OSHost()

	require.Equal(t, host.IPNetwork{}, h.IPNetwork, "IPNetwork is host.IPNetwork{}")
	require.Equal(t, host.OSFilesystem{}, h.Filesystem, "Filesystem is host.OSFilesystem{}")
}

func TestOSHost_listens_and_dials_TCP_on_loopback(t *testing.T) {
	h := humus.OSHost()

	ln, err := h.ListenTCP(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- err.Error()
			return
		}
		defer func() { _ = c.Close() }()
		b, _ := io.ReadAll(c)
		accepted <- string(b)
	}()

	c, err := h.DialTCP(t.Context(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	_, err = io.WriteString(c, "hello over tcp")
	require.NoError(t, err)
	require.NoError(t, c.Close())

	require.Equal(t, "hello over tcp", <-accepted)
}

func TestOSHost_listens_and_dials_UDP_on_loopback(t *testing.T) {
	h := humus.OSHost()

	pc, err := h.ListenUDP(t.Context(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })

	c, err := h.DialUDP(t.Context(), "udp", pc.LocalAddr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	_, err = io.WriteString(c, "hello over udp")
	require.NoError(t, err)

	buf := make([]byte, 64)
	n, from, err := pc.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "hello over udp", string(buf[:n]))
	require.Equal(t, c.LocalAddr().String(), from.String(), "the datagram came from the dialled socket")
}

func TestOSHost_refuses_the_other_protocol(t *testing.T) {
	h := humus.OSHost()
	ctx := context.Background()

	_, err := h.ListenTCP(ctx, "udp", "127.0.0.1:0")
	requireUnknownNetwork(t, err)
	_, err = h.DialTCP(ctx, "udp", "127.0.0.1:1")
	requireUnknownNetwork(t, err)
	_, err = h.ListenUDP(ctx, "tcp", "127.0.0.1:0")
	requireUnknownNetwork(t, err)
	_, err = h.DialUDP(ctx, "tcp", "127.0.0.1:1")
	requireUnknownNetwork(t, err)
}

func requireUnknownNetwork(t *testing.T, err error) {
	t.Helper()
	var unknown net.UnknownNetworkError
	require.ErrorAs(t, err, &unknown)
}
