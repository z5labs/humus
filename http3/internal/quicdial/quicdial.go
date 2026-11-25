// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package quicdial dials QUIC connections over the connected UDP sockets a
// network's DialUDP returns, and tracks each one so that it can be closed.
//
// quic-go's quic.Transport reads and writes a net.PacketConn, and a
// connected socket is a net.Conn, whose WriteTo fails with
// net.ErrWriteToConnected. The adapter here reads with Read and writes with
// Write, so a socket the network dialled carries a QUIC connection, and the
// network resolves the name as it does for TCP. It forwards SetReadBuffer
// and SetWriteBuffer to the socket: without them, quic-go writes its UDP
// buffer warning to the real standard error with log.Printf.
//
// A Dialer gives each connection a socket and a quic.Transport of its own,
// and closes both once the connection has closed. Close closes every
// connection still open, so nothing it dialled outlives it, whatever quic-go
// does with a connection it stops using. http3servertest's client dials
// through it, and http3client will.
package quicdial

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"

	"github.com/quic-go/quic-go"
)

// Dialer dials QUIC connections through a network's DialUDP, and tracks
// each connection until it closes. It is safe for concurrent use.
type Dialer struct {
	dialUDP func(ctx context.Context, network, address string) (net.Conn, error)

	mu     sync.Mutex
	conns  map[*dialled]struct{}
	closed bool
}

// New returns a Dialer that dials each connection's socket with dialUDP,
// over "udp". It panics if dialUDP is nil.
func New(dialUDP func(ctx context.Context, network, address string) (net.Conn, error)) *Dialer {
	if dialUDP == nil {
		panic("quicdial: New called with a nil dial function")
	}
	return &Dialer{dialUDP: dialUDP, conns: make(map[*dialled]struct{})}
}

// errClosed is the error of a dial after Close.
var errClosed = errors.New("quicdial: the Dialer is closed")

// Dial dials a QUIC connection to address with a full handshake, over a
// socket of its own. Its signature is that of http3.Transport's Dial
// field, so it can be one.
func (d *Dialer) Dial(ctx context.Context, address string, tlsConf *tls.Config, conf *quic.Config) (*quic.Conn, error) {
	return d.dial(ctx, address, func(tr *quic.Transport, addr net.Addr) (*quic.Conn, error) {
		return tr.Dial(ctx, addr, tlsConf, conf)
	})
}

// DialEarly dials a QUIC connection to address as Dial does, and returns
// it before its handshake completes, so that it may send 0-RTT data if the
// TLS config holds a session the server allowed it for.
func (d *Dialer) DialEarly(ctx context.Context, address string, tlsConf *tls.Config, conf *quic.Config) (*quic.Conn, error) {
	return d.dial(ctx, address, func(tr *quic.Transport, addr net.Addr) (*quic.Conn, error) {
		return tr.DialEarly(ctx, addr, tlsConf, conf)
	})
}

func (d *Dialer) dial(ctx context.Context, address string, dial func(*quic.Transport, net.Addr) (*quic.Conn, error)) (*quic.Conn, error) {
	if d.isClosed() {
		return nil, errClosed
	}
	sock, err := d.dialUDP(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	tr := &quic.Transport{Conn: packetConn{Conn: sock}}
	conn, err := dial(tr, sock.RemoteAddr())
	if err != nil {
		_ = tr.Close()
		_ = sock.Close()
		return nil, err
	}

	dc := &dialled{conn: conn, tr: tr, sock: sock}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		dc.close()
		return nil, errClosed
	}
	d.conns[dc] = struct{}{}
	d.mu.Unlock()

	// Once the connection has closed, whoever closed it, its transport and
	// socket are closed and it is no longer tracked.
	context.AfterFunc(conn.Context(), func() {
		dc.close()
		d.mu.Lock()
		delete(d.conns, dc)
		d.mu.Unlock()
	})
	return conn, nil
}

func (d *Dialer) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// Close closes every connection the Dialer dialled that is still open,
// with its transport and socket, and makes every later dial fail. Calling
// it again does nothing.
func (d *Dialer) Close() {
	d.mu.Lock()
	d.closed = true
	conns := make([]*dialled, 0, len(d.conns))
	for dc := range d.conns {
		conns = append(conns, dc)
	}
	clear(d.conns)
	d.mu.Unlock()

	for _, dc := range conns {
		dc.close()
	}
}

// dialled is one connection the Dialer dialled, with the transport and the
// socket that carry it.
type dialled struct {
	conn *quic.Conn
	tr   *quic.Transport
	sock net.Conn
	once sync.Once
}

// close closes the connection with no error, then its transport and its
// socket, once.
func (dc *dialled) close() {
	dc.once.Do(func() {
		_ = dc.conn.CloseWithError(0, "")
		_ = dc.tr.Close()
		_ = dc.sock.Close()
	})
}

// packetConn is a connected socket as a net.PacketConn, for quic.Transport.
// It reads only from the address it is connected to, and writes only to it,
// whatever address it is given.
type packetConn struct {
	net.Conn
}

func (c packetConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.Read(p)
	return n, c.RemoteAddr(), err
}

func (c packetConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return c.Write(p)
}

// SetReadBuffer sets the socket's receive buffer, so quic-go doesn't warn
// that it can't. A socket that can't set it reports so.
func (c packetConn) SetReadBuffer(bytes int) error {
	s, ok := c.Conn.(interface{ SetReadBuffer(int) error })
	if !ok {
		return errors.ErrUnsupported
	}
	return s.SetReadBuffer(bytes)
}

// SetWriteBuffer sets the socket's send buffer, so quic-go doesn't warn
// that it can't. A socket that can't set it reports so.
func (c packetConn) SetWriteBuffer(bytes int) error {
	s, ok := c.Conn.(interface{ SetWriteBuffer(int) error })
	if !ok {
		return errors.ErrUnsupported
	}
	return s.SetWriteBuffer(bytes)
}
