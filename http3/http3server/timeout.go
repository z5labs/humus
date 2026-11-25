// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// bound returns h with each request's stream bounded by settings'
// ReadTimeout and WriteTimeout, both measured from the end of the request's
// headers, when h begins.
//
// quic-go's http3.Server has neither timeout and no timer: a stream's
// deadline only fails a read or a write. So ReadTimeout is the stream's
// read deadline, which fails a read of the body once it passes. WriteTimeout
// is a timer, as net/http's HTTP/2 server has one per stream: when it
// fires, the request's context is cancelled and every read and write of the
// stream fails, and once h returns the stream is reset with
// H3_INTERNAL_ERROR, by the panic quic-go answers with that code. quic-go
// gives a handler no way to reset its stream but that panic, so a handler
// that ignores its context holds its stream until it returns.
//
// http.ResponseController's SetReadDeadline moves the read deadline, and its
// SetWriteDeadline moves the timer, so a handler that streams can extend its
// own deadlines. A handler that takes the stream over with
// http3.HTTPStreamer lifts both bounds: the stream is its own from then on.
func bound(settings Settings, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		begun := time.Now()
		if settings.ReadTimeout > 0 {
			_ = http.NewResponseController(w).SetReadDeadline(begun.Add(settings.ReadTimeout))
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		tw := &timedWriter{ResponseWriter: w, cancel: cancel}
		if settings.WriteTimeout > 0 {
			tw.mu.Lock()
			tw.armLocked(begun.Add(settings.WriteTimeout))
			tw.mu.Unlock()
		}
		defer tw.stop()

		h.ServeHTTP(tw, r.WithContext(ctx))
		if tw.stop() {
			panic(http.ErrAbortHandler)
		}
	})
}

// errTimedOut is what a timedWriter's deadline setters return once its timer
// has fired. It wraps http.ErrHandlerTimeout, as net/http's own timeout
// errors do.
var errTimedOut = fmt.Errorf("http3server: the request's WriteTimeout has passed: %w", http.ErrHandlerTimeout)

// timedWriter is the ResponseWriter of a request bounded by a write timer.
// It is what otelhttp wraps, so it implements every interface of quic-go's
// ResponseWriter that otelhttp passes on, and unwraps to it. It implements
// http3.HTTPStreamer too, so a handler that walks the Unwrap chain for one
// takes the stream over through it.
type timedWriter struct {
	http.ResponseWriter
	cancel context.CancelFunc

	mu       sync.Mutex
	timer    *time.Timer
	gen      uint64 // counts the timers armed, so a stopped one that fires anyway does nothing
	fired    bool   // the timer fired
	done     bool   // the handler returned, or took the stream over
	hijacked bool   // the handler took the stream over
}

// Unwrap returns quic-go's ResponseWriter, which http.ResponseController
// uses for every method timedWriter doesn't implement.
func (w *timedWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Flush flushes the response, as quic-go's Flush does.
func (w *timedWriter) Flush() {
	_ = w.FlushError()
}

// FlushError flushes the response, and returns quic-go's error.
func (w *timedWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// HTTPStream hands the stream to the handler, as quic-go's HTTPStream does,
// and lifts the stream's bounds: it stops the timer and, unless the timer
// has fired, clears the stream's deadlines, since quic-go neither resets
// nor finishes a stream it has handed over. A stream whose timer has fired
// keeps failing its reads and writes.
func (w *timedWriter) HTTPStream() *http3.Stream {
	w.mu.Lock()
	defer w.mu.Unlock()
	str := w.ResponseWriter.(http3.HTTPStreamer).HTTPStream()
	w.stopLocked()
	w.hijacked = true
	if !w.fired {
		_ = str.SetDeadline(time.Time{})
	}
	return str
}

// SetWriteDeadline moves the write timer to d, or stops it when d is zero,
// and sets the stream's write deadline to d. Once the timer has fired, it
// returns errTimedOut and moves nothing, since the stream is to
// be reset.
func (w *timedWriter) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired {
		return errTimedOut
	}
	w.armLocked(d)
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(d)
}

// SetReadDeadline sets the stream's read deadline to d. Once the write
// timer has fired, it returns errTimedOut and sets nothing, so
// the stream's reads go on failing.
func (w *timedWriter) SetReadDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired {
		return errTimedOut
	}
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(d)
}

// armLocked stops the timer, and arms a new one that fires at d, unless d is
// zero or the handler has returned. w.mu must be held.
func (w *timedWriter) armLocked(d time.Time) {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.gen++
	if d.IsZero() || w.done {
		return
	}
	gen := w.gen
	w.timer = time.AfterFunc(time.Until(d), func() { w.fire(gen) })
}

// fire cancels the request's context and fails every read and write of its
// stream, unless the timer gen was stopped or the handler has returned.
func (w *timedWriter) fire(gen uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen != w.gen || w.done {
		return
	}
	w.fired = true
	w.cancel()
	now := time.Now()
	rc := http.NewResponseController(w.ResponseWriter)
	_ = rc.SetWriteDeadline(now)
	_ = rc.SetReadDeadline(now)
}

// stop stops the timer, once the handler has returned, and reports whether
// the stream is to be reset: whether the timer fired, on a stream the
// handler didn't take over.
func (w *timedWriter) stop() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
	return w.fired && !w.hijacked
}

// stopLocked stops the timer for good. w.mu must be held.
func (w *timedWriter) stopLocked() {
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}
