// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// route is an operation, checked and bound, as the API serves it.
type route struct {
	name     string // such as "GET /carts/{cartId}"
	method   string
	path     Path
	opParams []operationParam // its query and header parameters
	status   int
	codecs   *codecs
	answers  resolved
	logger   log.Logger
	bodyType string // the decoder's media type, parsed and lowercased
	meta     parts  // its own parts, for its metadata in the document
}

// parseMediaType returns mt's type and subtype, lowercased, without
// parameters.
func parseMediaType(mt string) string {
	if mt == "" {
		return ""
	}
	parsed, _, err := mime.ParseMediaType(mt)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(mt))
	}
	return parsed
}

// ServeHTTP decodes r's parameters and body, calls the handler, and writes
// its response, or the answer to its error. r is the copy of the request
// chi routed, carrying the path's values.
//
// A panic of the decoder's Decode, of the handler, or of the encoder's
// Encode is recovered, recorded, and answered by the catch-all. A panic of
// the encoder's WriterTo is recovered and recorded, and cuts the response
// short.
func (rt *route) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st, rerr := rt.params(r)
	if rerr != nil {
		rt.reject(w, r, rerr)
		return
	}
	if rerr := rt.checkContentType(r); rerr != nil {
		rt.reject(w, r, rerr)
		return
	}
	var body any
	var err error
	if p := guard(func() { body, err = rt.codecs.decode(r) }); p != nil {
		rt.answerPanic(w, r, p)
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		rt.reject(w, r, &RequestError{Status: status, Err: err})
		return
	}
	var resp any
	if p := guard(func() { resp, err = rt.codecs.handle(r.Context(), st, body) }); p != nil {
		rt.answerPanic(w, r, p)
		return
	}
	if err != nil {
		rt.answerError(w, r, err)
		return
	}
	var wt io.WriterTo
	if p := guard(func() { wt, err = rt.codecs.encode(resp) }); p != nil {
		rt.answerPanic(w, r, p)
		return
	}
	if err != nil {
		rt.fail(w, r, fmt.Errorf("encoding the response: %w", err))
		return
	}
	rt.respond(w, r, wt)
}

// params decodes each of the path's parameters from r, then each query and
// header parameter the operation reads. chi matches a request's escaped
// path when it has one, so its values are unescaped here.
func (rt *route) params(r *http.Request) (*requestState, *RequestError) {
	st := &requestState{op: rt.name, params: make(map[string]any)}
	for _, seg := range rt.path.segs {
		if seg.param == nil {
			continue
		}
		raw := r.PathValue(seg.param.name)
		if raw == "" {
			// chi matches an empty segment, as in /carts//items, but a
			// path parameter is always required.
			return nil, &RequestError{
				Status: http.StatusBadRequest,
				Param:  seg.param.name,
				Err:    fmt.Errorf("path parameter %q is empty", seg.param.name),
			}
		}
		if r.URL.RawPath != "" {
			unescaped, err := url.PathUnescape(raw)
			if err != nil {
				return nil, &RequestError{
					Status: http.StatusBadRequest,
					Param:  seg.param.name,
					Err:    fmt.Errorf("path parameter %q: %w", seg.param.name, err),
				}
			}
			raw = unescaped
		}
		v, err := seg.param.parse(raw)
		if err != nil {
			return nil, &RequestError{Status: http.StatusBadRequest, Param: seg.param.name, Err: err}
		}
		st.params[pathKey(seg.param.name)] = v
	}
	if len(rt.opParams) == 0 {
		return st, nil
	}
	query := r.URL.Query()
	for _, p := range rt.opParams {
		v, err := p.read(r, query)
		if err != nil {
			return nil, &RequestError{Status: http.StatusBadRequest, Param: p.paramName(), Err: err}
		}
		st.params[p.paramKey()] = v
	}
	return st, nil
}

// checkContentType rejects a request whose Content-Type names another media
// type than the decoder's, or none, unless the decoder reads no body.
func (rt *route) checkContentType(r *http.Request) *RequestError {
	if rt.bodyType == "" {
		return nil
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return &RequestError{
			Status: http.StatusUnsupportedMediaType,
			Err:    fmt.Errorf("the request has no Content-Type, but the operation reads %s", rt.codecs.decMediaType),
		}
	}
	if got, _, err := mime.ParseMediaType(ct); err != nil || got != rt.bodyType {
		return &RequestError{
			Status: http.StatusUnsupportedMediaType,
			Err:    fmt.Errorf("the request's Content-Type is %q, but the operation reads %s", ct, rt.codecs.decMediaType),
		}
	}
	return nil
}

// reject answers rerr with the operation's RequestErrors. It isn't logged:
// the request was the client's mistake.
func (rt *route) reject(w http.ResponseWriter, r *http.Request, rerr *RequestError) {
	wt, _, err := rt.answers.requestErrors.encode(rerr)
	if err != nil {
		rt.fail(w, r, fmt.Errorf("encoding %s's answer to %w: %w", rt.answers.requestErrors.name, rerr, err))
		return
	}
	rt.write(w, r, rerr.Status, rt.answers.requestErrors.mediaType, wt)
}

// answerError answers the handler's error with the first Error that
// matches it, the operation's own first, and otherwise with the catch-all.
func (rt *route) answerError(w http.ResponseWriter, r *http.Request, herr error) {
	for _, e := range rt.answers.errors {
		wt, ok, err := e.encode(herr)
		if !ok {
			continue
		}
		if err != nil {
			rt.fail(w, r, fmt.Errorf("encoding %s's answer to %w: %w", e.name, herr, err))
			return
		}
		rt.write(w, r, e.status, e.mediaType, wt)
		return
	}
	rt.fail(w, r, herr)
}

// fail records err, and answers it with the catch-all. When the catch-all's
// own encoder fails, its status is sent alone.
func (rt *route) fail(w http.ResponseWriter, r *http.Request, err error) {
	rt.record(r, err)
	ca := rt.answers.catchAll
	wt, _, encErr := ca.encode(err)
	if encErr != nil {
		rt.record(r, fmt.Errorf("encoding %s's answer: %w", ca.name, encErr))
		w.WriteHeader(ca.status)
		return
	}
	rt.write(w, r, ca.status, ca.mediaType, wt)
}

// record records err as an exception event on the request's span, and as
// one ERROR record, "request failed", in the request's context, so the
// record carries the span's trace and span IDs. When the request's context
// is done, because the client went away, err goes on the span alone.
func (rt *route) record(r *http.Request, err error) {
	ctx := r.Context()
	trace.SpanFromContext(ctx).RecordError(err)
	rt.log(r, "request failed", err)
}

// log emits one ERROR record, body, for err, in the request's context,
// unless the context is done because the client went away.
func (rt *route) log(r *http.Request, body string, err error) {
	ctx := r.Context()
	if ctx.Err() != nil {
		return
	}
	emit(ctx, rt.logger, log.SeverityError, body,
		semconv.HTTPRequestMethodKey.String(r.Method),
		semconv.HTTPRoute(rt.path.String()),
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
	)
}

// write writes an answer: status, then the WriterTo's body, with rt's rule
// for a WriterTo that fails once the status is written. A panic of the
// WriterTo isn't recovered: it is the end user's answer that panicked, so
// it reaches httpserver, which records it and aborts the response.
func (rt *route) write(w http.ResponseWriter, r *http.Request, status int, mediaType string, wt io.WriterTo) {
	if err := write(w, status, mediaType, wt); err != nil {
		rt.abort(r, err)
	}
}

// respond writes the operation's response: its status, then the body of
// the WriterTo its encoder returned, with rt's rule for a WriterTo that
// fails once the status is written. A panic of the WriterTo's Len comes
// before the status, so it is answered by answerPanic. A panic of its
// WriteTo is recorded as answerPanic records one, and cuts the response
// short, as a failure does.
func (rt *route) respond(w http.ResponseWriter, r *http.Request, wt io.WriterTo) {
	n := -1
	if p := guard(func() { n = length(wt) }); p != nil {
		rt.answerPanic(w, r, p)
		return
	}
	var err error
	if p := guard(func() { err = writeSized(w, rt.status, rt.codecs.encMediaType, n, wt) }); p != nil {
		rt.recordPanic(r, p)
		panic(http.ErrAbortHandler)
	}
	if err != nil {
		rt.abort(r, err)
	}
}

// abort cuts short a response whose WriterTo failed with err once the
// status was written.
//
// That failure can't change the response, so it is cut short: the error is
// recorded on the request's span, with the span's status set to Error, and
// as one ERROR record, "response failed", unless the client went away.
// Then abort panics with http.ErrAbortHandler, which net/http and
// httpserver pass through without logging. On HTTP/2 the stream is reset,
// and on HTTP/1.1 the connection is closed, so the client sees a failed
// response rather than a complete one.
func (rt *route) abort(r *http.Request, err error) {
	span := trace.SpanFromContext(r.Context())
	span.RecordError(err)
	span.SetStatus(codes.Error, "response failed")
	rt.log(r, "response failed", err)
	panic(http.ErrAbortHandler)
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

// answerPanic records p, and answers it with the catch-all, with its status
// and wrap of an error that doesn't carry the panic's value, so the value
// never reaches the client.
// When the catch-all's own encoder fails, its status is sent alone, as fail
// sends it. A panic of the catch-all's wrap or encoder isn't recovered: it
// reaches httpserver, which records it and aborts the response.
func (rt *route) answerPanic(w http.ResponseWriter, r *http.Request, p *panicked) {
	rt.recordPanic(r, p)
	ca := rt.answers.catchAll
	wt, _, err := ca.encode(errors.New("the operation panicked"))
	if err != nil {
		rt.record(r, fmt.Errorf("encoding %s's answer: %w", ca.name, err))
		w.WriteHeader(ca.status)
		return
	}
	rt.write(w, r, ca.status, ca.mediaType, wt)
}

// recordPanic records p as an exception event on the request's span, with
// exception.type, exception.message and exception.stacktrace, sets the
// span's status to Error, and emits one ERROR record, "operation
// panicked", in the request's context, so the record carries the span's
// trace and span IDs. A panic is a bug whatever the client did, so it is
// logged even when the client went away.
func (rt *route) recordPanic(r *http.Request, p *panicked) {
	ctx := r.Context()
	exception := []attribute.KeyValue{
		semconv.ExceptionType(fmt.Sprintf("%T", p.value)),
		semconv.ExceptionMessage(fmt.Sprint(p.value)),
		semconv.ExceptionStacktrace(string(p.stack)),
	}
	span := trace.SpanFromContext(ctx)
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(exception...))
	span.SetStatus(codes.Error, "operation panicked")
	attrs := append([]attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(r.Method),
		semconv.HTTPRoute(rt.path.String()),
	}, exception...)
	emit(ctx, rt.logger, log.SeverityError, "operation panicked", attrs...)
}

// write writes status, then the WriterTo's body, with the Content-Type
// mediaType, which isn't set when it is "". When the WriterTo has a Len,
// it is the Content-Length, so it must count the bytes WriteTo writes. The
// WriterTo is handed a writer that flushes each write, so a body that
// streams reaches the client as it is written. It returns the WriterTo's
// error.
func write(w http.ResponseWriter, status int, mediaType string, wt io.WriterTo) error {
	return writeSized(w, status, mediaType, length(wt), wt)
}

// length returns the WriterTo's Len, or -1 when it has none.
func length(wt io.WriterTo) int {
	if l, ok := wt.(interface{ Len() int }); ok {
		return l.Len()
	}
	return -1
}

// writeSized is write, given the WriterTo's length, or -1 for none.
func writeSized(w http.ResponseWriter, status int, mediaType string, n int, wt io.WriterTo) error {
	if mediaType != "" {
		w.Header().Set("Content-Type", mediaType)
	}
	if n >= 0 && bodyAllowed(status) {
		w.Header().Set("Content-Length", strconv.Itoa(n))
	}
	w.WriteHeader(status)
	if wt == nil {
		return nil
	}
	_, err := wt.WriteTo(flushWriter{w: w, rc: http.NewResponseController(w)})
	return err
}

// bodyAllowed reports whether a response with status has a body, and so a
// Content-Length.
func bodyAllowed(status int) bool {
	return status >= http.StatusOK && status != http.StatusNoContent && status != http.StatusNotModified
}

// flushWriter is the writer a WriterTo is handed: it flushes each write.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	if err := f.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return n, err
	}
	return n, nil
}

// emit emits one record at sev, with body and attrs, through logger.
func emit(ctx context.Context, logger log.Logger, sev log.Severity, body string, attrs ...attribute.KeyValue) {
	var rec log.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(sev)
	rec.SetSeverityText(sev.String())
	rec.SetBody(attribute.StringValue(body))
	rec.AddAttributes(attrs...)
	logger.Emit(ctx, rec)
}
