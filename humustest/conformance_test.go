// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

import (
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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
// on a machine whose ports are already in use. The TLS handshake serves a
// certificate for 127.0.0.1 alone, so a client dialling 127.0.0.2 fails to
// verify it.
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

	t.Run("TLS handshake with a NewCA certificate", func(t *testing.T) {
		h := newHost(t)
		ca := humustest.NewCA(t)
		every := conformanceListen(t, h, ":0")
		port := strconv.Itoa(portOf(t, every.Addr()))
		serveTLS(t, every, ca.Issue(t, "127.0.0.1").TLS())

		client := &http.Client{Transport: &http.Transport{
			DialContext:       h.Dial,
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{RootCAs: ca.Pool()},
		}}

		address := net.JoinHostPort("127.0.0.1", port)
		body, err := getHTTPS(t, client, "https://"+address+"/")
		require.NoError(t, err)
		require.Equal(t, address, body)

		_, err = getHTTPS(t, client, "https://"+net.JoinHostPort("127.0.0.2", port)+"/")
		requireHostnameError(t, err)
	})

	t.Run("TLS close while the peer does not read", func(t *testing.T) {
		took, ok := closeNotifyStandoff(t, newHost(t), false)
		require.True(t, ok, "Close hasn't returned after %v", took)
		require.Less(t, took, 5*time.Second, "Close waited out crypto/tls's write deadline")
	})

	t.Run("TLS close whose write deadline is reset while close_notify is written", func(t *testing.T) {
		took, ok := closeNotifyStandoff(t, newHost(t), true)
		require.True(t, ok, "Close hasn't returned after %v", took)
	})
}

// closeNotifyStandoff serves TLS on h with a server that never reads after
// its handshake, as kfake's reader doesn't while a response is unwritten.
// It closes the client's connection and, with reset, resets the
// connection's write deadline once crypto/tls is writing close_notify, as
// franz-go does after a write on the same connection. It returns how long
// Close took, or false if it hadn't returned after 10s.
func closeNotifyStandoff(t *testing.T, h humus.Host, reset bool) (time.Duration, bool) {
	t.Helper()
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "127.0.0.1").TLS()
	ln := conformanceListen(t, h, "127.0.0.1:0")
	defer func() { _ = ln.Close() }()

	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		srv := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, SessionTicketsDisabled: true})
		if err := srv.Handshake(); err != nil {
			return
		}
		<-release
	}()
	defer func() {
		unblock()
		_ = ln.Close()
		<-served
	}()

	conn, err := h.Dial(t.Context(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	w := &alertConn{Conn: conn, alert: make(chan struct{})}
	client := tls.Client(w, &tls.Config{RootCAs: ca.Pool(), ServerName: "127.0.0.1"})
	require.NoError(t, client.HandshakeContext(t.Context()))

	w.closing.Store(true)
	start := time.Now()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = client.Close()
	}()
	<-w.alert
	if reset {
		// franz-go discards this error too. Close may already have
		// finished, and the reset fails on a closed connection.
		_ = client.SetWriteDeadline(time.Time{})
	}

	select {
	case <-closed:
		return time.Since(start), true
	case <-time.After(10 * time.Second):
		unblock()
		<-closed
		return time.Since(start), false
	}
}

// alertConn is the connection beneath crypto/tls. Once closing is set, it
// closes alert as its first write, which is close_notify, begins.
type alertConn struct {
	net.Conn
	alert   chan struct{}
	closing atomic.Bool
	alerted atomic.Bool
}

func (c *alertConn) Write(b []byte) (int, error) {
	if c.closing.Load() && c.alerted.CompareAndSwap(false, true) {
		close(c.alert)
	}
	return c.Conn.Write(b)
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
