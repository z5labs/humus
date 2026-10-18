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
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
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
	body, err := rt.codecs.decode(r)
	if err != nil {
		status := http.StatusBadRequest
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
		}
		rt.reject(w, r, &RequestError{Status: status, Err: err})
		return
	}
	resp, err := rt.codecs.handle(r.Context(), st, body)
	if err != nil {
		rt.answerError(w, r, err)
		return
	}
	wt, err := rt.codecs.encode(resp)
	if err != nil {
		rt.fail(w, r, fmt.Errorf("encoding the response: %w", err))
		return
	}
	write(w, r, rt.status, rt.codecs.encMediaType, wt)
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
	write(w, r, rerr.Status, rt.answers.requestErrors.mediaType, wt)
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
		write(w, r, e.status, e.mediaType, wt)
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
	write(w, r, ca.status, ca.mediaType, wt)
}

// record records err as an exception event on the request's span, and as
// one ERROR record, "request failed", in the request's context, so the
// record carries the span's trace and span IDs. When the request's context
// is done, because the client went away, err goes on the span alone.
func (rt *route) record(r *http.Request, err error) {
	ctx := r.Context()
	trace.SpanFromContext(ctx).RecordError(err)
	if ctx.Err() != nil {
		return
	}
	emit(ctx, rt.logger, log.SeverityError, "request failed",
		semconv.HTTPRequestMethodKey.String(r.Method),
		semconv.HTTPRoute(rt.path.String()),
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
	)
}

// write writes status, then the WriterTo's body, with the Content-Type
// mediaType, which isn't set when it is "". Once the status is written, a
// failed write can't change the response: it is recorded on the request's
// span.
func write(w http.ResponseWriter, r *http.Request, status int, mediaType string, wt io.WriterTo) {
	if mediaType != "" {
		w.Header().Set("Content-Type", mediaType)
	}
	w.WriteHeader(status)
	if wt == nil {
		return
	}
	if _, err := wt.WriteTo(w); err != nil {
		trace.SpanFromContext(r.Context()).RecordError(err)
	}
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
