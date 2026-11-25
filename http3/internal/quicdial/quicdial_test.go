// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package quicdial

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/humustest"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts.
// The server is a bare quic-go listener on the Host's UDP, which echoes
// what each stream sends.

// alpn is the protocol the tests' QUIC connections negotiate.
const alpn = "echo"

// listen serves an echo over QUIC on localhost:4433 through h, with a
// certificate from ca, until t ends. It returns the channel each connection
// it accepts arrives on.
func listen(t *testing.T, h *humustest.Host, ca *humustest.CA) <-chan *quic.Conn {
	t.Helper()
	sock, err := h.ListenUDP(t.Context(), "udp", ":4433")
	require.NoError(t, err)
	ln, err := quic.Listen(sock, &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}, NextProtos: []string{alpn}}, nil)
	require.NoError(t, err)
	accepted := make(chan *quic.Conn, 8)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			accepted <- conn
			wg.Go(func() {
				for {
					str, err := conn.AcceptStream(context.Background())
					if err != nil {
						return
					}
					_, _ = io.Copy(str, str)
					_ = str.Close()
				}
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		for {
			select {
			case conn := <-accepted:
				_ = conn.CloseWithError(0, "")
				continue
			default:
			}
			break
		}
		_ = sock.Close()
		wg.Wait()
	})
	return accepted
}

// clientTLS returns a client's TLS config, trusting only ca.
func clientTLS(ca *humustest.CA) *tls.Config {
	return &tls.Config{RootCAs: ca.Pool(), ServerName: "localhost", NextProtos: []string{alpn}}
}

// sockets records each socket a Dialer dials, and how often it was closed.
type sockets struct {
	mu       sync.Mutex
	networks []string
	conns    []*countingConn
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
}

func (s *sockets) DialUDP(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := s.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	cc := &countingConn{Conn: c}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.networks = append(s.networks, network)
	s.conns = append(s.conns, cc)
	return cc, nil
}

// closed reports how many of the sockets were closed.
func (s *sockets) closed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.conns {
		if c.closes.Load() > 0 {
			n++
		}
	}
	return n
}

// countingConn is a socket that counts its closes.
type countingConn struct {
	net.Conn
	closes atomic.Int64
}

func (c *countingConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

// The socket's buffer methods, forwarded, as the Host's socket has them.
func (c *countingConn) SetReadBuffer(n int) error {
	return c.Conn.(interface{ SetReadBuffer(int) error }).SetReadBuffer(n)
}

func (c *countingConn) SetWriteBuffer(n int) error {
	return c.Conn.(interface{ SetWriteBuffer(int) error }).SetWriteBuffer(n)
}

// echo sends msg on a new stream of conn and returns what came back.
func echo(t *testing.T, conn *quic.Conn, msg string) string {
	t.Helper()
	str, err := conn.OpenStreamSync(t.Context())
	require.NoError(t, err)
	_, err = io.WriteString(str, msg)
	require.NoError(t, err)
	require.NoError(t, str.Close())
	b, err := io.ReadAll(str)
	require.NoError(t, err)
	return string(b)
}

func TestDialer_dials_QUIC_over_a_socket_of_its_own(t *testing.T) {
	for _, tc := range []struct {
		name string
		dial func(d *Dialer, ctx context.Context, address string, tlsConf *tls.Config, conf *quic.Config) (*quic.Conn, error)
	}{
		{"Dial", (*Dialer).Dial},
		{"DialEarly", (*Dialer).DialEarly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				listen(t, h, ca)
				synctest.Wait()
				s := &sockets{dial: h.DialUDP}
				d := New(s.DialUDP)
				defer d.Close()

				first, err := tc.dial(d, t.Context(), "localhost:4433", clientTLS(ca), nil)
				require.NoError(t, err)
				second, err := tc.dial(d, t.Context(), "localhost:4433", clientTLS(ca), nil)
				require.NoError(t, err)
				require.Equal(t, "hello", echo(t, first, "hello"))
				require.Equal(t, "again", echo(t, second, "again"))
				require.Equal(t, []string{"udp", "udp"}, s.networks, "each connection has a socket of its own, dialled over udp")
			})
		})
	}
}

func TestDialer_Close_closes_every_connection_it_dialled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		listen(t, h, ca)
		synctest.Wait()
		s := &sockets{dial: h.DialUDP}
		d := New(s.DialUDP)

		var conns []*quic.Conn
		for range 3 {
			conn, err := d.Dial(t.Context(), "localhost:4433", clientTLS(ca), nil)
			require.NoError(t, err)
			conns = append(conns, conn)
		}

		d.Close()
		for _, conn := range conns {
			require.Error(t, conn.Context().Err(), "a connection is still open")
		}
		require.Equal(t, 3, s.closed(), "every socket is closed")

		_, err := d.Dial(t.Context(), "localhost:4433", clientTLS(ca), nil)
		require.ErrorIs(t, err, errClosed)
		d.Close()
	})
}

func TestDialer_closes_the_socket_of_a_connection_its_peer_closed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		accepted := listen(t, h, ca)
		synctest.Wait()
		s := &sockets{dial: h.DialUDP}
		d := New(s.DialUDP)
		defer d.Close()

		conn, err := d.Dial(t.Context(), "localhost:4433", clientTLS(ca), nil)
		require.NoError(t, err)
		require.NoError(t, (<-accepted).CloseWithError(0, ""))
		<-conn.Context().Done()
		synctest.Wait()

		require.Equal(t, 1, s.closed())
		d.mu.Lock()
		require.Empty(t, d.conns, "the closed connection is no longer tracked")
		d.mu.Unlock()
	})
}

func TestDialer_fails_a_dial(t *testing.T) {
	t.Run("when the network does", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			errNoUDP := errors.New("no UDP here")
			d := New(func(context.Context, string, string) (net.Conn, error) { return nil, errNoUDP })
			_, err := d.Dial(t.Context(), "localhost:4433", &tls.Config{}, nil)
			require.ErrorIs(t, err, errNoUDP)
		})
	})

	t.Run("when nothing answers, and closes its socket", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			s := &sockets{dial: h.DialUDP}
			d := New(s.DialUDP)
			defer d.Close()

			_, err := d.Dial(t.Context(), "localhost:4433", &tls.Config{ServerName: "localhost", NextProtos: []string{alpn}}, nil)
			require.ErrorIs(t, err, syscall.ECONNREFUSED)
			require.Equal(t, 1, s.closed())
		})
	})

	t.Run("after Close", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			s := &sockets{dial: h.DialUDP}
			d := New(s.DialUDP)
			d.Close()
			_, err := d.DialEarly(t.Context(), "localhost:4433", &tls.Config{}, nil)
			require.ErrorIs(t, err, errClosed)
			require.Empty(t, s.networks, "no socket was dialled")
		})
	})
}

func TestNew_panics_on_a_nil_dial_function(t *testing.T) {
	require.PanicsWithValue(t, "quicdial: New called with a nil dial function", func() { New(nil) })
}

// buffers is a socket that records the buffer sizes it is set to.
type buffers struct {
	net.Conn
	read, write int
}

func (b *buffers) SetReadBuffer(n int) error  { b.read = n; return nil }
func (b *buffers) SetWriteBuffer(n int) error { b.write = n; return nil }

func TestPacketConn(t *testing.T) {
	t.Run("forwards the buffer sizes to the socket", func(t *testing.T) {
		b := &buffers{}
		pc := packetConn{Conn: b}
		require.NoError(t, pc.SetReadBuffer(7<<20))
		require.NoError(t, pc.SetWriteBuffer(5<<20))
		require.Equal(t, 7<<20, b.read)
		require.Equal(t, 5<<20, b.write)
	})

	t.Run("reports a socket that can't set them", func(t *testing.T) {
		a, b := net.Pipe()
		defer func() { _ = a.Close() }()
		defer func() { _ = b.Close() }()
		pc := packetConn{Conn: a}
		require.ErrorIs(t, pc.SetReadBuffer(1), errors.ErrUnsupported)
		require.ErrorIs(t, pc.SetWriteBuffer(1), errors.ErrUnsupported)
	})

	t.Run("reads from and writes to the address it is connected to", func(t *testing.T) {
		a, b := net.Pipe()
		defer func() { _ = a.Close() }()
		defer func() { _ = b.Close() }()
		pc := packetConn{Conn: a}

		go func() { _, _ = pc.WriteTo([]byte("ping"), &net.UDPAddr{Port: 9}) }()
		got := make([]byte, 4)
		_, err := io.ReadFull(b, got)
		require.NoError(t, err)
		require.Equal(t, "ping", string(got))

		go func() { _, _ = b.Write([]byte("pong")) }()
		n, from, err := pc.ReadFrom(got)
		require.NoError(t, err)
		require.Equal(t, "pong", string(got[:n]))
		require.Equal(t, a.RemoteAddr(), from)
	})
}
