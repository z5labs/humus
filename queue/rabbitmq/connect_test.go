// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
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

		c := connection("rabbit")
		c.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
		conn, err := rabbitmq.Connect(t.Context(), h.Host(), c)
		require.Nil(t, conn)
		// The Host's Dial would fail with a *net.OpError.
		var addrErr *net.AddrError
		require.ErrorAs(t, err, &addrErr)
		var opErr *net.OpError
		require.False(t, errors.As(err, &opErr), "it dialled: %v", err)
		require.ErrorContains(t, err, "TLS server name")
	})
}

// startOnly accepts one connection on l, reads AMQP's protocol header and
// answers it with a Connection.Start offering PLAIN, then reads until the
// connection is closed, answering nothing more. The returned channel is
// closed once the connection is.
func startOnly(t *testing.T, l net.Listener) <-chan struct{} {
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
		header := make([]byte, 8)
		if _, err := io.ReadFull(conn, header); err != nil {
			t.Errorf("reading the protocol header: %v", err)
			return
		}
		if _, err := conn.Write(connectionStart()); err != nil {
			t.Errorf("writing Connection.Start: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	}()
	return closed
}

// connectionStart is a Connection.Start frame for AMQP 0-9-1, with no
// server properties, the PLAIN mechanism and the en_US locale.
func connectionStart() []byte {
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, 10) // class: connection
	payload = binary.BigEndian.AppendUint16(payload, 10) // method: start
	payload = append(payload, 0, 9)                      // version 0-9
	payload = binary.BigEndian.AppendUint32(payload, 0)  // server properties: an empty table
	for _, s := range []string{"PLAIN", "en_US"} {       // mechanisms, locales
		payload = binary.BigEndian.AppendUint32(payload, uint32(len(s)))
		payload = append(payload, s...)
	}
	frame := []byte{1, 0, 0} // a method frame, on channel 0
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)
	return append(frame, 0xCE) // frame end
}

func TestConnect_reports_a_timeout_after_Connection_Start_as_a_timeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		l, err := h.Listen("tcp", "rabbit:5672")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()
		closed := startOnly(t, l)

		c := connection("rabbit:5672")
		c.ConnectTimeout = 5 * time.Second
		start := time.Now()
		conn, err := rabbitmq.Connect(t.Context(), h.Host(), c)
		require.Nil(t, conn)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		// amqp091 reports any failure after Start as ErrCredentials.
		require.NotErrorIs(t, err, amqp.ErrCredentials)
		require.Equal(t, c.ConnectTimeout, time.Since(start))

		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("the connection is still open")
		}
	})
}

func TestConnect_stops_the_AMQP_handshake_when_ctx_is_cancelled(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		l, err := h.Listen("tcp", "rabbit:5672")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()
		closed := silent(t, l)

		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(time.Second, cancel)
		start := time.Now()
		conn, err := rabbitmq.Connect(ctx, h.Host(), connection("rabbit:5672"))
		require.Nil(t, conn)
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, time.Second, time.Since(start))

		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("the connection is still open")
		}
	})
}

func TestConnect_stops_the_AMQP_handshake_at_ctxs_earlier_deadline(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		l, err := h.Listen("tcp", "rabbit:5672")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()
		closed := silent(t, l)

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		start := time.Now()
		conn, err := rabbitmq.Connect(ctx, h.Host(), connection("rabbit:5672"))
		require.Nil(t, conn)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, 2*time.Second, time.Since(start))

		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("the connection is still open")
		}
	})
}
