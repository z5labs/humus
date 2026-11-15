// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// loggerName is the name of the logger the Site logs through.
const loggerName = "github.com/z5labs/humus/hypermedia"

// errPanicked is the error the ErrorPage answers a handler's panic with. It
// doesn't carry the panic's value, so the value never reaches the client.
var errPanicked = errors.New("hypermedia: the handler panicked")

// serve calls rt's handler with r, on a watcher of w, and answers or
// records its failure:
//   - before the response starts, an error or a panic drops the headers
//     the handler set, and the ErrorPage answers it;
//   - after it starts, an error ends the response as written, and a panic
//     aborts it.
//
// Every error but a *RequestError is recorded, and every panic but one
// with http.ErrAbortHandler, which passes through untouched.
func (s *Site) serve(w http.ResponseWriter, r *http.Request, rt *route) {
	above := w.Header().Clone()
	ww := &watcher{w: w}
	var err error
	p := guard(func() { err = rt.h(ww, r) })
	path := rt.path.String()
	switch {
	case p != nil && ww.started:
		s.recordPanic(r, path, "handler panicked", p)
		panic(http.ErrAbortHandler)
	case p != nil:
		s.recordPanic(r, path, "handler panicked", p)
		reset(w.Header(), above)
		s.answer(w, r, path, errPanicked)
	case err == nil:
	case ww.started:
		s.record(r, path, "response failed", err, ww.fault)
	default:
		s.record(r, path, "request failed", err, noFault)
		reset(w.Header(), above)
		s.answer(w, r, path, err)
	}
}

// answer answers err with the Site's ErrorPage, on a watcher of w. path
// is the pattern of r's route, or "" when no route serves r.
//
// The page's own error or panic is recorded as "error page failed". When
// the page wrote nothing, the headers it set are dropped and the Site
// answers a bare 500. Otherwise the response ends as a handler's would:
// as written after an error, and aborted after a panic.
func (s *Site) answer(w http.ResponseWriter, r *http.Request, path string, err error) {
	before := w.Header().Clone()
	pw := &watcher{w: w}
	var pageErr error
	p := guard(func() { pageErr = s.errorPage(pw, r, err) })
	switch {
	case p != nil:
		s.recordPanic(r, path, "error page failed", p)
		if pw.started {
			panic(http.ErrAbortHandler)
		}
	case pageErr == nil:
		return
	default:
		s.record(r, path, "error page failed", pageErr, pw.fault)
		if pw.started {
			return
		}
	}
	reset(w.Header(), before)
	w.WriteHeader(http.StatusInternalServerError)
}

// reset sets h back to was, which is a clone of h taken earlier.
func reset(h, was http.Header) {
	clear(h)
	maps.Copy(h, was)
}

// record records err, unless it is a *RequestError, as an exception event
// on the request's span, sets the span's status to Error, and emits one
// ERROR record, body, in the request's context. path is the pattern of r's
// route, or "".
//
// f says why a write of the response failed, if one did. When the client
// went away, err goes on the span alone: a write failed because the
// connection did, or, with no failed write, the request's context is
// done. A write the server refused is the server's failure, so it is
// logged whatever the request's context.
func (s *Site) record(r *http.Request, path, body string, err error, f fault) {
	if _, ok := errors.AsType[*RequestError](err); ok {
		return
	}
	span := trace.SpanFromContext(r.Context())
	span.RecordError(err)
	span.SetStatus(codes.Error, body)
	switch {
	case f == refused:
	case f == lost, r.Context().Err() != nil:
		return
	}
	s.emit(r, path, body,
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
	)
}

// recordPanic records p as an exception event on the request's span, with
// exception.type, exception.message and exception.stacktrace, sets the
// span's status to Error, and emits one ERROR record, body, in the
// request's context. A panic is a bug whatever the client did, so it is
// logged even when the client went away.
func (s *Site) recordPanic(r *http.Request, path, body string, p *panicked) {
	exception := []attribute.KeyValue{
		semconv.ExceptionType(fmt.Sprintf("%T", p.value)),
		semconv.ExceptionMessage(fmt.Sprint(p.value)),
		semconv.ExceptionStacktrace(string(p.stack)),
	}
	span := trace.SpanFromContext(r.Context())
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(exception...))
	span.SetStatus(codes.Error, body)
	s.emit(r, path, body, exception...)
}

// emit emits one ERROR record, body, with r's method, path as its route
// when it isn't "", and attrs. The record is emitted in
// context.WithoutCancel of r's context, so it still carries the span's
// trace and span IDs, and a processor or exporter that heeds the context
// doesn't drop it for a cancellation that has already happened.
func (s *Site) emit(r *http.Request, path, body string, attrs ...attribute.KeyValue) {
	var rec log.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(log.SeverityError)
	rec.SetSeverityText(log.SeverityError.String())
	rec.SetBody(attribute.StringValue(body))
	rec.AddAttributes(semconv.HTTPRequestMethodKey.String(r.Method))
	if path != "" {
		rec.AddAttributes(semconv.HTTPRoute(path))
	}
	rec.AddAttributes(attrs...)
	s.logger.Emit(context.WithoutCancel(r.Context()), rec)
}

// panicked is a panic guard recovered: its value, and the stack of the
// goroutine where it was recovered, which still holds the panic's frames.
type panicked struct {
	value any
	stack []byte
}

// guard calls f, and returns the panic it recovers, or nil when f returns.
// A panic with http.ErrAbortHandler is passed through untouched, as
// net/http and httpserver pass it, so the end user's code can still abort
// a response on purpose.
func guard(f func()) (p *panicked) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		// net/http compares the panic value with ErrAbortHandler, so
		// this does too.
		if v == http.ErrAbortHandler {
			panic(v)
		}
		// The stack is taken here, in the deferred call, where it still
		// holds the frames of the panic.
		p = &panicked{value: v, stack: debug.Stack()}
	}()
	f()
	return nil
}

// fault says why a write of a response failed.
type fault int

const (
	noFault fault = iota // no write failed
	lost                 // a write or flush failed because the connection did
	refused              // the server refused a write
)

// watcher is the http.ResponseWriter a handler or the ErrorPage is given.
// It passes every call straight to w, and holds nothing: it only watches
// whether the response has started, and why a write of it failed.
//
// The response starts at its first Write, Flush, ReadFrom or successful
// Hijack, or at a WriteHeader of 101 or of 200 and above. A WriteHeader
// of another 1xx, such as 103 Early Hints, is informational, and the
// response is still to come.
//
// It forwards Flush, FlushError, Hijack and ReadFrom, and Unwraps, so an
// http.ResponseController reaches w's deadlines.
type watcher struct {
	w       http.ResponseWriter
	started bool
	fault   fault
}

func (w *watcher) Header() http.Header {
	return w.w.Header()
}

func (w *watcher) WriteHeader(code int) {
	if code == http.StatusSwitchingProtocols || code >= http.StatusOK {
		w.started = true
	}
	w.w.WriteHeader(code)
}

func (w *watcher) Write(b []byte) (int, error) {
	w.started = true
	n, err := w.w.Write(b)
	if err != nil {
		w.note(err)
	}
	return n, err
}

// Flush flushes w, as http.Flusher's Flush does, and drops its error.
func (w *watcher) Flush() {
	_ = w.FlushError()
}

// FlushError flushes w, and returns its error, as http.ResponseController
// expects of a ResponseWriter.
func (w *watcher) FlushError() error {
	w.started = true
	err := http.NewResponseController(w.w).Flush()
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.fail(lost)
	}
	return err
}

// Hijack hijacks w's connection. The response starts only if it succeeds,
// so a handler that can't hijack, such as over HTTP/2, can still return an
// error the ErrorPage answers.
func (w *watcher) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.w).Hijack()
	if err == nil {
		w.started = true
	}
	return conn, rw, err
}

// ReadFrom copies src to w, with w's own ReadFrom when it has one, so a
// file served on HTTP/1.1 can still be sent with sendfile.
func (w *watcher) ReadFrom(src io.Reader) (int64, error) {
	w.started = true
	if rf, ok := w.w.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	// The struct hides w's ReadFrom from io.Copy, which would otherwise
	// call it again.
	return io.Copy(struct{ io.Writer }{w}, src)
}

// Unwrap returns w, for http.ResponseController.
func (w *watcher) Unwrap() http.ResponseWriter {
	return w.w
}

// note notes why a write failed. A write the server refuses fails before
// anything reaches the connection: a body for a status that has none, more
// bytes than the Content-Length, or a write after a Hijack. Any other
// failure is the connection's.
func (w *watcher) note(err error) {
	switch {
	case errors.Is(err, http.ErrBodyNotAllowed),
		errors.Is(err, http.ErrContentLength),
		errors.Is(err, http.ErrHijacked):
		w.fail(refused)
	default:
		w.fail(lost)
	}
}

// fail notes a failed write's fault, unless a write was already refused,
// which makes the response the server's failure whatever came after.
func (w *watcher) fail(why fault) {
	if w.fault != refused {
		w.fault = why
	}
}
