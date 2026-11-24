// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// recordPanics is the layer between nameRoute and the end user's handler. It
// records a panic of next on the request's span and as an ERROR record, then
// aborts the response by panicking again with http.ErrAbortHandler, which
// net/http handles without logging.
//
// It ends the request's span itself before it panics again, whatever the
// panic. otelhttp ends the span in a deferred call, and the OpenTelemetry
// SDK's End records a panic it finds in progress as an exception event: left
// to it, http.ErrAbortHandler would be recorded as an exception, and a
// recorded panic would be recorded twice. A span that has ended ignores the
// second End.
type recordPanics struct {
	next   http.Handler
	logger log.Logger
}

func (h recordPanics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		ctx := r.Context()
		span := trace.SpanFromContext(ctx)

		// net/http compares the panic value with ErrAbortHandler, so this
		// does too: a handler that aborts on purpose is passed through
		// untouched.
		if v == http.ErrAbortHandler {
			span.End()
			panic(v)
		}

		// The stack is taken here, in the deferred call, where it still
		// holds the frames of the panic.
		attrs := []attribute.KeyValue{
			semconv.ExceptionType(fmt.Sprintf("%T", v)),
			semconv.ExceptionMessage(fmt.Sprint(v)),
			semconv.ExceptionStacktrace(string(debug.Stack())),
		}

		span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(attrs...))
		span.SetStatus(codes.Error, "handler panicked")
		emit(ctx, h.logger, log.SeverityError, "handler panicked", attrs...)
		span.End()

		panic(http.ErrAbortHandler)
	}()
	h.next.ServeHTTP(w, r)
}
