// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

import (
	"net"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

// testConformance runs the scenarios that the in-memory network must get
// right exactly as a real one does. TestConformance_in_memory runs them on
// a Host, and TestConformance_loopback, behind the conformance build tag,
// runs them on real loopback sockets through humus.OSHost.
//
// The two names are 127.0.0.1 and 127.0.0.2, both on the loopback
// interface, and every first listener takes port 0, so the scenarios work
// on a machine whose ports are already in use.
func testConformance(t *testing.T, newHost func(t *testing.T) humus.Host) {
	t.Run("two names on one port both serve", func(t *testing.T) {
		h := newHost(t)
		one := conformanceListen(t, h, "127.0.0.1:0")
		port := strconv.Itoa(portOf(t, one.Addr()))
		two := conformanceListen(t, h, net.JoinHostPort("127.0.0.2", port))
		serve(t, one, "one")
		serve(t, two, "two")

		require.Equal(t, "one", dialRead(t, h.Dial, net.JoinHostPort("127.0.0.1", port)))
		require.Equal(t, "two", dialRead(t, h.Dial, net.JoinHostPort("127.0.0.2", port)))
	})

	t.Run("EADDRINUSE for a name on a port every name holds", func(t *testing.T) {
		h := newHost(t)
		every := conformanceListen(t, h, ":0")
		defer func() { require.NoError(t, every.Close()) }()

		_, err := h.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(portOf(t, every.Addr()))))
		require.ErrorIs(t, err, syscall.EADDRINUSE)
	})

	t.Run("EADDRINUSE for every name on a port a name holds", func(t *testing.T) {
		h := newHost(t)
		one := conformanceListen(t, h, "127.0.0.1:0")
		defer func() { require.NoError(t, one.Close()) }()

		_, err := h.Listen("tcp", ":"+strconv.Itoa(portOf(t, one.Addr())))
		require.ErrorIs(t, err, syscall.EADDRINUSE)
	})

	t.Run("ECONNREFUSED for a name no listener holds", func(t *testing.T) {
		h := newHost(t)
		one := conformanceListen(t, h, "127.0.0.1:0")
		serve(t, one, "one")

		_, err := h.Dial(t.Context(), "tcp", net.JoinHostPort("127.0.0.2", strconv.Itoa(portOf(t, one.Addr()))))
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
	})

	t.Run("ECONNREFUSED after the listener closes", func(t *testing.T) {
		h := newHost(t)
		one := conformanceListen(t, h, "127.0.0.1:0")
		address := one.Addr().String()
		require.NoError(t, one.Close())

		_, err := h.Dial(t.Context(), "tcp", address)
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
	})
}

func conformanceListen(t *testing.T, h humus.Host, address string) net.Listener {
	t.Helper()
	ln, err := h.Listen("tcp", address)
	require.NoError(t, err)
	return ln
}

func TestConformance_in_memory(t *testing.T) {
	testConformance(t, func(t *testing.T) humus.Host {
		return humustest.NewHost(t).Host()
	})
}
