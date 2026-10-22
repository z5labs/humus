// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server

import (
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
)

// conns tracks every connection a Server's listener accepts, from Accept
// until it has written its client a GOAWAY, or closed. Shutdown closes the
// ones still in their handshake, and ends the streams only once every
// other one has written its GOAWAY, so that each stream's trailers follow
// the GOAWAY on its connection.
type conns struct {
	mu sync.Mutex
	// closing is set by goingAway: a connection accepted after it is
	// closed at once.
	closing bool
	// pending holds every connection that is open and has written no
	// GOAWAY.
	pending map[*conn]struct{}
	// wentAway is closed once closing is set and pending is empty.
	wentAway chan struct{}
}

func newConns() *conns {
	return &conns{pending: map[*conn]struct{}{}, wentAway: make(chan struct{})}
}

// add tracks nc, a connection the listener just accepted, as in its
// handshake. After goingAway, it closes nc instead, so that grpc-go's
// handshake of it fails at once.
func (cs *conns) add(nc net.Conn) *conn {
	c := &conn{Conn: nc, conns: cs}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.closing {
		_ = nc.Close()
		return c
	}
	cs.pending[c] = struct{}{}
	return c
}

// handshaken records that c's handshake is over.
func (cs *conns) handshaken(c *conn) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c.handshaken = true
}

// settle stops tracking c, which has written a GOAWAY, or closed.
func (cs *conns) settle(c *conn) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, ok := cs.pending[c]; !ok {
		return
	}
	delete(cs.pending, c)
	if cs.closing && len(cs.pending) == 0 {
		close(cs.wentAway)
	}
}

// goingAway closes every connection still in its handshake, and every one
// accepted after it. It returns a channel that is closed once every other
// connection has written a GOAWAY, or closed.
//
// grpc-go's GracefulStop sends no GOAWAY, and its Stop doesn't return,
// until every connection the server has accepted has finished its
// handshake, which may take up to its 120s ConnectionTimeout. A connection
// in its handshake has no RPC on it, and its client dials again.
func (cs *conns) goingAway() <-chan struct{} {
	cs.mu.Lock()
	cs.closing = true
	var handshaking []*conn
	for c := range cs.pending {
		if !c.handshaken {
			handshaking = append(handshaking, c)
		}
	}
	if len(cs.pending) == 0 {
		close(cs.wentAway)
	}
	cs.mu.Unlock()

	// Close settles each one, which takes cs.mu.
	for _, c := range handshaking {
		_ = c.Close()
	}
	return cs.wentAway
}

// conn is a connection the listener accepted. grpc-go sets a deadline on it
// for its handshake, TLS's and then HTTP/2's, and clears the deadline once
// the handshake is over, so a SetDeadline with the zero time ends it.
type conn struct {
	net.Conn
	conns *conns

	// handshaken is guarded by conns.mu.
	handshaken bool
}

func (c *conn) SetDeadline(t time.Time) error {
	err := c.Conn.SetDeadline(t)
	if t.IsZero() {
		c.conns.handshaken(c)
	}
	return err
}

func (c *conn) Close() error {
	err := c.Conn.Close()
	c.conns.settle(c)
	return err
}

// listener is a net.Listener whose connections its conns track.
type listener struct {
	net.Listener
	conns *conns
}

func (l *listener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.conns.add(nc), nil
}

// creds are the transport credentials a Server serves with. The connection
// their ServerHandshake returns, on which grpc-go's transport reads and
// writes HTTP/2 frames, watches the frames written to it for the first
// GOAWAY.
type creds struct {
	credentials.TransportCredentials
}

func (cr creds) ServerHandshake(rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	nc, info, err := cr.TransportCredentials.ServerHandshake(rawConn)
	if err != nil {
		return nc, info, err
	}
	raw, ok := rawConn.(*conn)
	if !ok {
		return nc, info, nil
	}
	return &framesConn{Conn: nc, raw: raw}, info, nil
}

func (cr creds) Clone() credentials.TransportCredentials {
	return creds{TransportCredentials: cr.TransportCredentials.Clone()}
}

// framesConn is the connection grpc-go's transport writes HTTP/2 frames
// to: the TLS connection over raw, or raw itself in plain text. Closing it
// closes raw. Once a GOAWAY's header is written to it, it settles raw:
// every frame written after, such as a stream's trailers, follows the
// GOAWAY to the client.
type framesConn struct {
	net.Conn
	raw *conn

	// frames and goneAway are not guarded: grpc-go's transport writes
	// one frame after another, from one goroutine at a time, as any
	// concurrent writes would interleave the frames.
	frames   frames
	goneAway bool
}

func (c *framesConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if !c.goneAway && c.frames.write(b[:n]) {
		c.goneAway = true
		c.raw.conns.settle(c.raw)
	}
	return n, err
}

const (
	// frameHeaderLen is the length of an HTTP/2 frame's header: a 24-bit
	// payload length, a type, flags, and a stream identifier (RFC 9113,
	// section 4.1).
	frameHeaderLen = 9
	// goAwayType is the type of a GOAWAY frame (RFC 9113, section 6.8).
	goAwayType = 0x7
)

// frames follows the HTTP/2 frames written to a connection, from its
// first byte: a server's first frame is its SETTINGS, with no preface
// before it.
type frames struct {
	header [frameHeaderLen]byte
	n      int // the bytes of header written so far
	skip   int // the bytes of the current frame's payload still to be written
}

// write follows b, the next bytes written, and reports whether they hold
// the whole header of a GOAWAY frame. It reads no further than that
// header.
func (f *frames) write(b []byte) bool {
	for len(b) > 0 {
		if f.skip > 0 {
			k := min(len(b), f.skip)
			b = b[k:]
			f.skip -= k
			continue
		}
		k := copy(f.header[f.n:], b)
		b = b[k:]
		f.n += k
		if f.n < frameHeaderLen {
			return false
		}
		f.n = 0
		if f.header[3] == goAwayType {
			return true
		}
		f.skip = int(f.header[0])<<16 | int(f.header[1])<<8 | int(f.header[2])
	}
	return false
}
