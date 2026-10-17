// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver_test

import (
	"errors"
	"io"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

// errBoom is what the panicking handlers in this file panic with.
var errBoom = errors.New("boom")

// panicking serves GET /panic by panicking with v, after calling before
// with the response writer, and answers GET /hello with "hello".
func panicking(v any, before func(http.ResponseWriter)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /panic", func(w http.ResponseWriter, _ *http.Request) {
		if before != nil {
			before(w)
		}
		panic(v)
	})
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	})
	return mux
}

// panicRun is what a client saw of an App whose handler panics, and what the
// App recorded.
type panicRun struct {
	panicked response
	hello    response
	spans    []sdktrace.ReadOnlySpan
	errors   []sdklog.Record
	stderr   string
}

// runPanic runs an App serving h, sends it GET /panic then GET /hello with
// one client offering p, then stops the App and returns what happened.
func runPanic(t *testing.T, h http.Handler, p protocols) panicRun {
	t.Helper()
	ca := humustest.NewCA(t)
	host := humustest.NewHost(t)
	tel := humustest.NewTelemetry()
	proc, status := humustest.Start(t, serves(settingsFor(t, ca), h),
		humustest.WithApp(humus.WithHost(host.Host()), humus.WithTelemetry(tel.Builder())),
	)
	<-host.Listening("localhost:8443")

	c := client(host, ca, p)
	var run panicRun
	run.panicked = <-get(c, "https://localhost:8443/panic")
	run.hello = <-get(c, "https://localhost:8443/hello")
	synctest.Wait()

	proc.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())

	run.spans = serverSpans(tel)
	for _, r := range records(tel) {
		if r.Severity() == log.SeverityError {
			run.errors = append(run.errors, r)
		}
	}
	run.stderr = proc.Stderr()
	return run
}

// exceptions returns the exception events of span.
func exceptions(span sdktrace.ReadOnlySpan) []sdktrace.Event {
	var evs []sdktrace.Event
	for _, ev := range span.Events() {
		if ev.Name == "exception" {
			evs = append(evs, ev)
		}
	}
	return evs
}

// eventAttr returns the value of ev's attribute key as a string, and whether
// it has one.
func eventAttr(ev sdktrace.Event, key string) (string, bool) {
	for _, kv := range ev.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// requireAborted fails t unless r is a request p's client saw aborted: over
// HTTP/2 its stream reset, and over HTTP/1.1 its connection closed.
func requireAborted(t *testing.T, r response, p protocols) {
	t.Helper()
	require.Error(t, r.err, "want the request aborted")
	if p.http2 {
		require.ErrorContains(t, r.err, "stream error", "want the stream reset")
		require.ErrorContains(t, r.err, "INTERNAL_ERROR", "want the stream reset")
		return
	}
	require.True(t,
		errors.Is(r.err, io.EOF) || errors.Is(r.err, io.ErrUnexpectedEOF) || errors.Is(r.err, io.ErrClosedPipe),
		"err = %v, want the connection closed", r.err)
}

func TestService_a_handler_panic(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    protocols
	}{
		{"over HTTP/2", offersH2},
		{"over HTTP/1.1", offersHTTP1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("aborts the request, is recorded, and the Service keeps serving", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					run := runPanic(t, panicking(errBoom, nil), tc.p)

					// No 500, and no response at all.
					requireAborted(t, run.panicked, tc.p)
					require.Zero(t, run.panicked.code, "want no response")

					require.NoError(t, run.hello.err)
					require.Equal(t, http.StatusOK, run.hello.code)
					require.Equal(t, "hello", run.hello.body)

					var failed []sdktrace.ReadOnlySpan
					for _, s := range run.spans {
						if s.Status().Code == codes.Error {
							failed = append(failed, s)
						}
					}
					require.Len(t, run.spans, 2)
					require.Len(t, failed, 1)
					span := failed[0]
					evs := exceptions(span)
					require.Len(t, evs, 1)
					typ, ok := eventAttr(evs[0], "exception.type")
					require.True(t, ok, "the exception has no exception.type")
					require.Equal(t, "*errors.errorString", typ)
					msg, ok := eventAttr(evs[0], "exception.message")
					require.True(t, ok, "the exception has no exception.message")
					require.Equal(t, "boom", msg)
					stack, ok := eventAttr(evs[0], "exception.stacktrace")
					require.True(t, ok, "the exception has no exception.stacktrace")
					require.Contains(t, stack, "panic_test.go", "the stack trace does not reach the handler")

					require.Len(t, run.errors, 1)
					rec := run.errors[0]
					require.Equal(t, "handler panicked", rec.Body().AsString())
					require.Equal(t, "ERROR", rec.SeverityText())
					require.Equal(t, span.SpanContext().TraceID(), rec.TraceID())
					require.Equal(t, span.SpanContext().SpanID(), rec.SpanID())
					recMsg, ok := attr(rec, "exception.message")
					require.True(t, ok, "the record has no exception.message")
					require.Equal(t, "boom", recMsg.AsString())
					recStack, ok := attr(rec, "exception.stacktrace")
					require.True(t, ok, "the record has no exception.stacktrace")
					require.Contains(t, recStack.AsString(), "panic_test.go")

					require.Empty(t, run.stderr)
				})
			})

			t.Run("with http.ErrAbortHandler passes through untouched", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					run := runPanic(t, panicking(http.ErrAbortHandler, nil), tc.p)

					requireAborted(t, run.panicked, tc.p)
					require.Zero(t, run.panicked.code, "want no response")
					require.NoError(t, run.hello.err)
					require.Equal(t, "hello", run.hello.body)

					require.Len(t, run.spans, 2)
					for _, s := range run.spans {
						require.Empty(t, exceptions(s), "span %q has an exception event", s.Name())
					}
					require.Empty(t, run.errors)
					require.Empty(t, run.stderr)
				})
			})

			t.Run("after a partial response fails the client's read", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					partial := func(w http.ResponseWriter) {
						w.Header().Set("Content-Type", "text/plain")
						w.WriteHeader(http.StatusOK)
						_, _ = io.WriteString(w, "partial")
						w.(http.Flusher).Flush()
					}
					run := runPanic(t, panicking(errBoom, partial), tc.p)

					// The client got the headers the handler wrote, and its
					// read of the body fails rather than ending cleanly.
					require.Equal(t, http.StatusOK, run.panicked.code)
					requireAborted(t, run.panicked, tc.p)
					require.NotErrorIs(t, run.panicked.err, io.EOF, "the body ended cleanly")

					require.NoError(t, run.hello.err)
					require.Equal(t, "hello", run.hello.body)
					require.Len(t, run.errors, 1)
					require.Empty(t, run.stderr)
				})
			})
		})
	}
}
