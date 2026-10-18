// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// The tests in this file reach no broker: they run inside a testing/synctest
// bubble, on a humustest Host, so ConnectTimeout passes in fake time. What
// needs RabbitMQ itself is in connect_testcontainers_test.go.

// connection is DefaultSettings' Connection with addr and a login whose
// password is new for each call.
func connection(addr string) rabbitmq.Connection {
	c := rabbitmq.DefaultSettings().Connection
	c.Addr = addr
	c.SASL = &amqp.PlainAuth{Username: "orders", Password: rand.Text()}
	return c
}

// silent accepts one connection on l and reads it until it is closed,
// answering nothing. The returned channel is closed once the connection is,
// from either end.
func silent(t *testing.T, l net.Listener) <-chan struct{} {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		conn, err := l.Accept()
		if err != nil {
			t.Errorf("accepting: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(io.Discard, conn)
	}()
	return closed
}

func TestConnect_fails_with_the_Hosts_ECONNREFUSED_when_nothing_listens(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)

		conn, err := rabbitmq.Connect(t.Context(), h.Host(), connection("rabbit:5672"))
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
		require.Nil(t, conn)
	})
}

func TestConnect_fails_at_ConnectTimeout_when_the_broker_never_answers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		l, err := h.Listen("tcp", "rabbit:5672")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()
		closed := silent(t, l)

		c := connection("rabbit:5672")
		c.ConnectTimeout = 5 * time.Second
		start := time.Now()
		conn, err := rabbitmq.Connect(t.Context(), h.Host(), c)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Nil(t, conn)
		require.Equal(t, c.ConnectTimeout, time.Since(start))

		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("the connection is still open")
		}
	})
}

func TestConnect_names_Addrs_host_when_TLS_leaves_ServerName_empty(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		cert := ca.Issue(t, "rabbit")
		l, err := h.Listen("tcp", "rabbit:5671")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()

		serverName := make(chan string, 1)
		go func() {
			conn, err := l.Accept()
			if err != nil {
				t.Errorf("accepting: %v", err)
				return
			}
			tc := tls.Server(conn, &tls.Config{
				Certificates: []tls.Certificate{cert.TLS()},
				MinVersion:   tls.VersionTLS12,
			})
			if err := tc.HandshakeContext(t.Context()); err != nil {
				t.Errorf("handshake: %v", err)
			}
			serverName <- tc.ConnectionState().ServerName
			// The broker hangs up instead of speaking AMQP.
			_ = tc.Close()
		}()

		cfg := &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}
		c := connection("rabbit:5671")
		c.TLS = cfg
		start := time.Now()
		conn, err := rabbitmq.Connect(t.Context(), h.Host(), c)
		require.Error(t, err)
		require.NotErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(start), c.ConnectTimeout)
		require.Nil(t, conn)

		require.Equal(t, "rabbit", <-serverName)
		require.Empty(t, cfg.ServerName, "the end user's config was changed")
	})
}

func TestConnect_fails_the_TLS_handshake_at_ConnectTimeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		l, err := h.Listen("tcp", "rabbit:5671")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()
		closed := silent(t, l)

		c := connection("rabbit:5671")
		c.TLS = &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}
		c.ConnectTimeout = 5 * time.Second
		start := time.Now()
		conn, err := rabbitmq.Connect(t.Context(), h.Host(), c)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "TLS handshake")
		require.Nil(t, conn)
		require.Equal(t, c.ConnectTimeout, time.Since(start))

		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("the connection is still open")
		}
	})
}

func TestConnect_fails_before_dialling_when_TLS_needs_a_host_Addr_lacks(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		l, err := h.Listen("tcp", "rabbit:5671")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()

		c := connection("rabbit")
		c.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
		conn, err := rabbitmq.Connect(t.Context(), h.Host(), c)
		require.Error(t, err)
		require.Nil(t, conn)
	})
}
