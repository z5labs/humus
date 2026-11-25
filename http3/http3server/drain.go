// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// closeDelay is how long the drain leaves a connection open after its last
// request finished, as net/http's HTTP/2 server waits after its GOAWAY. It
// lets quic-go finish writing the last response.
const closeDelay = time.Second

// closeCode is the code the drain closes a connection with.
const closeCode = quic.ApplicationErrorCode(http3.ErrCodeNoError)

// drain closes the Service's connections once Shutdown has begun.
//
// quic-go's Shutdown sends each connection a GOAWAY, then waits for its
// client to close it, which a client that keeps an idle connection never
// does (quic-go#6195). So the drain tracks each connection, and the requests
// in flight on it, and once it has started, closes a connection with
// H3_NO_ERROR closeDelay after its last request finished, or after the
// drain started for a connection with none in flight.
type drain struct {
	mu      sync.Mutex
	started bool
	conns   map[*quic.Conn]*tracked
}

// tracked is one connection the drain tracks.
type tracked struct {
	conn     *quic.Conn
	inFlight int
	timer    *time.Timer // the close, once scheduled
}

// trackedKey is the context key of a request's connection's tracked.
type trackedKey struct{}

func newDrain() *drain {
	return &drain{conns: make(map[*quic.Conn]*tracked)}
}

// connContext is the http3.Server's ConnContext. It tracks c until c
// closes, and puts its tracked in the context each of its requests'
// contexts derives from.
func (d *drain) connContext(ctx context.Context, c *quic.Conn) context.Context {
	t := &tracked{conn: c}
	d.mu.Lock()
	d.conns[c] = t
	if d.started {
		d.scheduleLocked(t)
	}
	d.mu.Unlock()

	context.AfterFunc(c.Context(), func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if t.timer != nil {
			t.timer.Stop()
		}
		delete(d.conns, c)
	})
	return context.WithValue(ctx, trackedKey{}, t)
}

// track returns h counting each request in flight on its connection, as
// the outermost layer, so a request is in flight until the end user's
// handler and every layer around it have returned.
func (d *drain) track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := r.Context().Value(trackedKey{}).(*tracked)
		if !ok {
			h.ServeHTTP(w, r)
			return
		}
		d.begin(t)
		defer d.end(t)
		h.ServeHTTP(w, r)
	})
}

// begin counts a request in flight on t, and cancels t's close.
func (d *drain) begin(t *tracked) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t.inFlight++
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// end counts a request on t finished, and schedules t's close once the
// drain has started and none is in flight.
func (d *drain) end(t *tracked) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t.inFlight--
	if d.started && t.inFlight == 0 {
		d.scheduleLocked(t)
	}
}

// start starts the drain: every connection with no request in flight is
// closed closeDelay from now, and every other one closeDelay after its last
// request finishes.
func (d *drain) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.started = true
	for _, t := range d.conns {
		if t.inFlight == 0 {
			d.scheduleLocked(t)
		}
	}
}

// scheduleLocked schedules t's close closeDelay from now, unless it is
// scheduled already. d.mu must be held.
func (d *drain) scheduleLocked(t *tracked) {
	if t.timer != nil {
		return
	}
	t.timer = time.AfterFunc(closeDelay, func() {
		_ = t.conn.CloseWithError(closeCode, "")
	})
}

// closeAll closes every connection the drain tracks, with H3_NO_ERROR, at
// once, and returns once each one has closed, which has cancelled the
// context of each of its requests.
func (d *drain) closeAll() {
	d.mu.Lock()
	conns := make([]*quic.Conn, 0, len(d.conns))
	for c := range d.conns {
		conns = append(conns, c)
	}
	d.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range conns {
		// CloseWithError returns once the connection has closed.
		wg.Go(func() { _ = c.CloseWithError(closeCode, "") })
	}
	wg.Wait()
}
