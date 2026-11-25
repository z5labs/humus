// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
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
// H3_INTERNAL_ERROR, by the panic quic-go answers with that code.
//
// http.ResponseController's SetReadDeadline moves the read deadline, and its
// SetWriteDeadline moves the timer, so a handler that streams can
// extend its own deadlines.
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
// has fired.
var errTimedOut = errors.New("http3server: the request's WriteTimeout has passed")

// timedWriter is the ResponseWriter of a request bounded by a write timer.
// It is what otelhttp wraps, so it implements every interface of quic-go's
// ResponseWriter that otelhttp passes on, and unwraps to it.
type timedWriter struct {
	http.ResponseWriter
	cancel context.CancelFunc

	mu    sync.Mutex
	timer *time.Timer
	gen   uint64 // counts the timers armed, so a stopped one that fires anyway does nothing
	fired bool   // the timer fired
	done  bool   // the handler returned
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
// it fired.
func (w *timedWriter) stop() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	return w.fired
}
