// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/credentials/insecure"
)

// frame returns an HTTP/2 frame of type typ with payload, on stream 0.
func frame(typ byte, payload []byte) []byte {
	n := len(payload)
	b := []byte{byte(n >> 16), byte(n >> 8), byte(n), typ, 0, 0, 0, 0, 0}
	return append(b, payload...)
}

var (
	settingsFrame = frame(0x4, make([]byte, 6))
	goAwayFrame   = frame(goAwayType, make([]byte, 8))
)

// split returns b in writes of n bytes, and the rest.
func split(b []byte, n int) [][]byte {
	var writes [][]byte
	for len(b) > n {
		writes = append(writes, b[:n])
		b = b[n:]
	}
	return append(writes, b)
}

func TestFrames_write(t *testing.T) {
	long := split(frame(0x0, bytes.Repeat(goAwayFrame, 10_000)), 16<<10)
	testCases := []struct {
		name   string
		writes [][]byte
		// at is the write that holds the end of the GOAWAY's header.
		at int
	}{
		{name: "a GOAWAY after the SETTINGS, in one write", writes: [][]byte{append(bytes.Clone(settingsFrame), goAwayFrame...)}, at: 0},
		{name: "a GOAWAY after the SETTINGS, in writes of their own", writes: [][]byte{settingsFrame, goAwayFrame}, at: 1},
		{name: "a GOAWAY's header split across writes", writes: [][]byte{settingsFrame, goAwayFrame[:4], goAwayFrame[4:]}, at: 2},
		{name: "every frame written a byte at a time", writes: split(append(bytes.Clone(settingsFrame), goAwayFrame...), 1), at: len(settingsFrame) + frameHeaderLen - 1},
		{name: "a DATA frame whose payload holds a GOAWAY", writes: [][]byte{frame(0x0, goAwayFrame), goAwayFrame}, at: 1},
		{name: "a DATA frame longer than 65535 bytes, split across writes", writes: append(long, goAwayFrame), at: len(long)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var f frames
			at := -1
			for i, w := range tc.writes {
				if f.write(w) {
					at = i
					break
				}
			}
			require.Equal(t, tc.at, at)
		})
	}
}

// recorder is a net.Conn that keeps what is written to it, and whether it
// has been closed.
type recorder struct {
	net.Conn

	written bytes.Buffer
	closed  bool
}

func (r *recorder) Write(b []byte) (int, error) { return r.written.Write(b) }

func (r *recorder) Close() error {
	r.closed = true
	return nil
}

func (r *recorder) SetDeadline(time.Time) error { return nil }

// accept tracks nc in cs as the listener does, and runs its handshake as
// grpc-go's server does: it sets a deadline, hands it to creds, and clears
// the deadline. It returns the connection grpc-go's transport writes
// frames to.
func accept(t *testing.T, cs *conns, nc net.Conn) net.Conn {
	t.Helper()
	c := cs.add(nc)
	require.NoError(t, c.SetDeadline(time.Now().Add(time.Minute)))
	fc, _, err := creds{TransportCredentials: insecure.NewCredentials()}.ServerHandshake(c)
	require.NoError(t, err)
	require.NoError(t, c.SetDeadline(time.Time{}))
	return fc
}

// requireOpen requires that ch isn't closed.
func requireOpen(t *testing.T, ch <-chan struct{}, msgAndArgs ...any) {
	t.Helper()
	select {
	case <-ch:
		require.Fail(t, "closed", msgAndArgs...)
	default:
	}
}

func TestConns_goingAway(t *testing.T) {
	t.Run("closes a connection in its handshake, and waits for every other to write a GOAWAY", func(t *testing.T) {
		cs := newConns()
		inHandshake := &recorder{}
		c := cs.add(inHandshake)
		require.NoError(t, c.SetDeadline(time.Now().Add(time.Minute)))
		first, second := &recorder{}, &recorder{}
		fc1, fc2 := accept(t, cs, first), accept(t, cs, second)

		wentAway := cs.goingAway()
		require.True(t, inHandshake.closed, "the connection in its handshake is closed")
		require.False(t, first.closed)
		require.False(t, second.closed)
		requireOpen(t, wentAway, "no connection has written a GOAWAY")

		_, err := fc1.Write(settingsFrame)
		require.NoError(t, err)
		_, err = fc1.Write(goAwayFrame)
		require.NoError(t, err)
		requireOpen(t, wentAway, "the second connection has written no GOAWAY")

		_, err = fc2.Write(append(bytes.Clone(settingsFrame), goAwayFrame...))
		require.NoError(t, err)
		<-wentAway
		require.Equal(t, append(bytes.Clone(settingsFrame), goAwayFrame...), second.written.Bytes(), "every byte reaches the connection")
	})

	t.Run("counts a connection that closes as gone", func(t *testing.T) {
		cs := newConns()
		fc := accept(t, cs, &recorder{})
		wentAway := cs.goingAway()
		requireOpen(t, wentAway)

		require.NoError(t, fc.Close())
		<-wentAway
	})

	t.Run("counts a connection that wrote its GOAWAY before", func(t *testing.T) {
		cs := newConns()
		fc := accept(t, cs, &recorder{})
		_, err := fc.Write(append(bytes.Clone(settingsFrame), goAwayFrame...))
		require.NoError(t, err)
		<-cs.goingAway()
	})

	t.Run("closes a connection accepted after it at once", func(t *testing.T) {
		cs := newConns()
		<-cs.goingAway()
		late := &recorder{}
		c := cs.add(late)
		require.True(t, late.closed)
		require.NoError(t, c.Close(), "grpc-go closes it again")
	})
}
