// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/hypermedia"
	"github.com/z5labs/humus/telemetry"
)

// recorded is a Site whose spans and records a test reads, and which
// notes each error its ErrorPage answers.
type recorded struct {
	t     *testing.T
	tel   *humustest.Telemetry
	sdk   telemetry.SDK
	h     *hypermedia.Site
	pages []error
}

// newRecorded returns the Site that serves routes, with page as its
// ErrorPage, or, when page is nil, one that writes the error's Status, or
// 500, and "error page".
func newRecorded(t *testing.T, page hypermedia.ErrorPage, routes ...hypermedia.Route) *recorded {
	t.Helper()
	rs := &recorded{t: t, tel: humustest.NewTelemetry()}
	sdk, err := rs.tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	rs.sdk = sdk
	if page == nil {
		page = func(w http.ResponseWriter, _ *http.Request, err error) error {
			status := http.StatusInternalServerError
			if re, ok := errors.AsType[*hypermedia.RequestError](err); ok {
				status = re.Status
			}
			w.WriteHeader(status)
			_, werr := io.WriteString(w, "error page")
			return werr
		}
	}
	h, err := hypermedia.NewSite(&humus.Setup{Telemetry: sdk}, func(w http.ResponseWriter, r *http.Request, err error) error {
		rs.pages = append(rs.pages, err)
		return page(w, r, err)
	}, routes)
	require.NoError(t, err)
	rs.h = h
	return rs
}

// serve serves r on w within a span of its own, which it ends even when
// the Site panics. It ends the span before it panics again, as httpserver
// does, since the SDK's End records a panic in progress as an exception.
func (rs *recorded) serve(w http.ResponseWriter, r *http.Request) {
	ctx, span := rs.sdk.TracerProvider.Tracer("test").Start(r.Context(), "request")
	defer func() {
		v := recover()
		span.End()
		if v != nil {
			panic(v)
		}
	}()
	rs.h.ServeHTTP(w, r.WithContext(ctx))
}

// do serves a request of method for target on a ResponseRecorder.
func (rs *recorded) do(method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	rs.serve(w, httptest.NewRequest(method, target, nil))
	return w
}

// span returns the one span that has ended.
func (rs *recorded) span() sdktrace.ReadOnlySpan {
	rs.t.Helper()
	spans := rs.tel.Spans()
	require.Len(rs.t, spans, 1)
	return spans[0]
}

// record is a log record, as a test compares it.
type record struct {
	scope    string
	severity log.Severity
	body     string
	attrs    map[string]string
}

// records returns every log record emitted.
func (rs *recorded) records() []record {
	return recordsOf(rs.tel)
}

// recordsOf returns every log record tel recorded.
func recordsOf(tel *humustest.Telemetry) []record {
	var out []record
	for _, r := range tel.Logs() {
		rec := record{
			scope:    r.InstrumentationScope().Name,
			severity: r.Severity(),
			body:     r.Body().AsString(),
			attrs:    make(map[string]string),
		}
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			rec.attrs[string(kv.Key)] = kv.Value.String()
			return true
		})
		out = append(out, rec)
	}
	return out
}

// bodies returns the body of each log record emitted.
func (rs *recorded) bodies() []string {
	var out []string
	for _, r := range rs.records() {
		out = append(out, r.body)
	}
	return out
}

// events returns each event of the span, as its name and attributes.
func events(s sdktrace.ReadOnlySpan) []map[string]string {
	var out []map[string]string
	for _, e := range s.Events() {
		m := map[string]string{"name": e.Name}
		for _, kv := range e.Attributes {
			m[string(kv.Key)] = kv.Value.String()
		}
		out = append(out, m)
	}
	return out
}

// fails returns a Handler that returns err.
func fails(err error) hypermedia.Handler {
	return func(http.ResponseWriter, *http.Request) error { return err }
}

func TestSite_records_a_failure_by_its_error_not_by_its_status(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request, _ error) error {
		_, err := io.WriteString(w, "all is well")
		return err
	}
	failed := errors.New("the database is down")

	t.Run("an error the page answers with a 200 is recorded", func(t *testing.T) {
		rs := newRecorded(t, ok, hypermedia.NewRoute(http.MethodPut, One, fails(failed)))
		w := rs.do(http.MethodPut, "/contacts/7")
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "all is well", w.Body.String())
		require.Equal(t, []error{failed}, rs.pages)

		span := rs.span()
		require.Equal(t, sdktrace.Status{Code: codes.Error, Description: "request failed"}, span.Status())
		require.Equal(t, []map[string]string{{
			"name":              "exception",
			"exception.type":    "*errors.errorString",
			"exception.message": "the database is down",
		}}, events(span))
		require.Equal(t, []record{{
			scope:    "github.com/z5labs/humus/hypermedia",
			severity: log.SeverityError,
			body:     "request failed",
			attrs: map[string]string{
				"http.request.method": "PUT",
				"http.route":          "/contacts/{contactId}",
				"error.type":          "*errors.errorString",
				"exception.message":   "the database is down",
			},
		}}, rs.records())
		require.Equal(t, span.SpanContext().TraceID(), rs.tel.Logs()[0].TraceID(), "the record is in the request's context")
	})

	for name, err := range map[string]error{
		"a *RequestError of 500":         &hypermedia.RequestError{Status: http.StatusInternalServerError, Err: failed},
		"a *RequestError of 404":         &hypermedia.RequestError{Status: http.StatusNotFound},
		"a wrapped *RequestError of 410": fmt.Errorf("contact 7: %w", &hypermedia.RequestError{Status: http.StatusGone}),
	} {
		t.Run(name+" isn't recorded", func(t *testing.T) {
			rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, fails(err)))
			w := rs.do(http.MethodGet, "/contacts/7")
			require.Equal(t, "error page", w.Body.String())
			require.Equal(t, []error{err}, rs.pages)
			require.Equal(t, sdktrace.Status{Code: codes.Unset}, rs.span().Status())
			require.Empty(t, rs.span().Events())
			require.Empty(t, rs.records())
		})
	}

	t.Run("a path or a method the Site answers isn't recorded", func(t *testing.T) {
		rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, says("one")))
		require.Equal(t, http.StatusNotFound, rs.do(http.MethodGet, "/orders").Code)
		require.Equal(t, http.StatusNotFound, rs.do(http.MethodGet, "/contacts/0").Code)
		require.Equal(t, http.StatusMethodNotAllowed, rs.do(http.MethodPut, "/contacts/7").Code)
		require.Len(t, rs.pages, 3)
		for _, s := range rs.tel.Spans() {
			require.Empty(t, s.Events())
		}
		require.Empty(t, rs.records())
	})
}

func TestSite_answers_a_handlers_panic_before_the_response_starts_with_the_ErrorPage(t *testing.T) {
	rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
		w.Header().Set("X-Event", "saved")
		panic("the contact's template is broken")
	}))
	w := rs.do(http.MethodGet, "/contacts/7")
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Equal(t, "error page", w.Body.String())
	require.Empty(t, w.Header().Get("X-Event"))
	require.Len(t, rs.pages, 1)
	require.NotContains(t, rs.pages[0].Error(), "template", "the page doesn't get the panic's value")

	span := rs.span()
	require.Equal(t, sdktrace.Status{Code: codes.Error, Description: "handler panicked"}, span.Status())
	evs := events(span)
	require.Len(t, evs, 1)
	require.Equal(t, "exception", evs[0]["name"])
	require.Equal(t, "string", evs[0]["exception.type"])
	require.Equal(t, "the contact's template is broken", evs[0]["exception.message"])
	require.Contains(t, evs[0]["exception.stacktrace"], "TestSite_answers_a_handlers_panic_before_the_response_starts_with_the_ErrorPage")

	recs := rs.records()
	require.Len(t, recs, 1)
	require.Equal(t, "handler panicked", recs[0].body)
	require.Equal(t, log.SeverityError, recs[0].severity)
	require.Equal(t, "GET", recs[0].attrs["http.request.method"])
	require.Equal(t, "/contacts/{contactId}", recs[0].attrs["http.route"])
	require.Equal(t, "string", recs[0].attrs["exception.type"])
	require.Equal(t, "the contact's template is broken", recs[0].attrs["exception.message"])
	require.Equal(t, evs[0]["exception.stacktrace"], recs[0].attrs["exception.stacktrace"])
}

func TestSite_keeps_the_headers_set_above_it_and_drops_a_failed_handlers_own(t *testing.T) {
	// above is a handler above the Site, which sets a Vary and a cookie on
	// every response.
	above := func(rs *recorded) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Vary", "X-Target")
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1"})
			rs.serve(w, r)
		})
	}
	// handler sets a header, a cookie and a Vary of its own, then ends as
	// end does.
	handler := func(end func(w http.ResponseWriter) error) hypermedia.Handler {
		return func(w http.ResponseWriter, _ *http.Request) error {
			w.Header().Set("X-Event", "saved")
			w.Header().Add("Vary", "Accept")
			http.SetCookie(w, &http.Cookie{Name: "flash", Value: "saved"})
			return end(w)
		}
	}
	page := func(w http.ResponseWriter, _ *http.Request, _ error) error {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, err := io.WriteString(w, "<p>Sorry</p>")
		return err
	}

	t.Run("a handler that succeeds adds to them", func(t *testing.T) {
		rs := newRecorded(t, page, hypermedia.NewRoute(http.MethodGet, One, handler(func(w http.ResponseWriter) error {
			_, err := io.WriteString(w, "<p>Saved</p>")
			return err
		})))
		w := httptest.NewRecorder()
		above(rs).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/contacts/7", nil))
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, []string{"X-Target", "Accept"}, w.Header().Values("Vary"))
		require.Equal(t, []string{"session=s1", "flash=saved"}, w.Header().Values("Set-Cookie"))
		require.Equal(t, "saved", w.Header().Get("X-Event"))
	})

	for name, end := range map[string]func(w http.ResponseWriter) error{
		"an error":  func(http.ResponseWriter) error { return errors.New("the database is down") },
		"a panic":   func(http.ResponseWriter) error { panic("the template is broken") },
		"a refusal": func(http.ResponseWriter) error { return &hypermedia.RequestError{Status: http.StatusConflict} },
	} {
		t.Run("a handler that ends in "+name+" before it writes gets the error page with only theirs", func(t *testing.T) {
			rs := newRecorded(t, page, hypermedia.NewRoute(http.MethodGet, One, handler(end)))
			w := httptest.NewRecorder()
			above(rs).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/contacts/7", nil))
			require.Equal(t, http.StatusInternalServerError, w.Code)
			require.Equal(t, "<p>Sorry</p>", w.Body.String())
			require.Equal(t, []string{"X-Target"}, w.Header().Values("Vary"))
			require.Equal(t, []string{"session=s1"}, w.Header().Values("Set-Cookie"))
			require.Empty(t, w.Header().Values("X-Event"))
			require.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
		})
	}
}

func TestSite_after_the_response_starts_the_response_is_the_handlers(t *testing.T) {
	failed := errors.New("the template is broken")
	starts := map[string]func(w http.ResponseWriter){
		"a Write":            func(w http.ResponseWriter) { _, _ = io.WriteString(w, "partial") },
		"a WriteHeader(200)": func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) },
		"a WriteHeader(204)": func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
		"a Flush":            func(w http.ResponseWriter) { w.(http.Flusher).Flush() },
		"a FlushError": func(w http.ResponseWriter) {
			_ = w.(interface{ FlushError() error }).FlushError()
		},
		"a ReadFrom": func(w http.ResponseWriter) {
			_, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader("partial"))
		},
	}
	for name, start := range starts {
		t.Run("an error after "+name+" ends the response as written, and is recorded", func(t *testing.T) {
			rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
				start(w)
				return failed
			}))
			w := rs.do(http.MethodGet, "/contacts/7")
			require.NotEqual(t, "error page", w.Body.String())
			require.Empty(t, rs.pages)
			require.Equal(t, sdktrace.Status{Code: codes.Error, Description: "response failed"}, rs.span().Status())
			require.Len(t, rs.span().Events(), 1)
			recs := rs.records()
			require.Len(t, recs, 1)
			require.Equal(t, "response failed", recs[0].body)
			require.Equal(t, map[string]string{
				"http.request.method": "GET",
				"http.route":          "/contacts/{contactId}",
				"error.type":          "*errors.errorString",
				"exception.message":   "the template is broken",
			}, recs[0].attrs)
		})
	}

	t.Run("a *RequestError after a Write ends the response as written, and isn't recorded", func(t *testing.T) {
		rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
			_, _ = io.WriteString(w, "partial")
			return &hypermedia.RequestError{Status: http.StatusNotFound}
		}))
		w := rs.do(http.MethodGet, "/contacts/7")
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "partial", w.Body.String())
		require.Empty(t, rs.pages)
		require.Empty(t, rs.span().Events())
		require.Empty(t, rs.records())
	})

	t.Run("a panic after a Write is recorded, and aborts the response", func(t *testing.T) {
		rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
			_, _ = io.WriteString(w, "partial")
			panic("the template is broken")
		}))
		require.PanicsWithValue(t, http.ErrAbortHandler, func() { rs.do(http.MethodGet, "/contacts/7") })
		require.Empty(t, rs.pages)
		require.Equal(t, sdktrace.Status{Code: codes.Error, Description: "handler panicked"}, rs.span().Status())
		recs := rs.records()
		require.Len(t, recs, 1)
		require.Equal(t, "handler panicked", recs[0].body)
		require.Equal(t, "the template is broken", recs[0].attrs["exception.message"])
		require.Contains(t, recs[0].attrs["exception.stacktrace"], "TestSite_after_the_response_starts_the_response_is_the_handlers")
	})

	t.Run("an informational WriteHeader doesn't start the response", func(t *testing.T) {
		rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
			w.Header().Set("Link", "</static/app.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			return failed
		}))
		fw := newFakeWriter()
		rs.serve(fw, httptest.NewRequest(http.MethodGet, "/contacts/7", nil))
		require.Equal(t, []int{http.StatusEarlyHints, http.StatusInternalServerError}, fw.codes)
		require.Equal(t, "error page", fw.body.String())
		require.Equal(t, []error{failed}, rs.pages)
	})

	t.Run("a WriteHeader(101) starts the response", func(t *testing.T) {
		rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusSwitchingProtocols)
			return failed
		}))
		fw := newFakeWriter()
		rs.serve(fw, httptest.NewRequest(http.MethodGet, "/contacts/7", nil))
		require.Equal(t, []int{http.StatusSwitchingProtocols}, fw.codes)
		require.Empty(t, rs.pages)
		require.Equal(t, []string{"response failed"}, rs.bodies())
	})

	t.Run("a Hijack that fails doesn't start the response", func(t *testing.T) {
		var hijackErr error
		rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
			_, _, hijackErr = w.(http.Hijacker).Hijack()
			return hijackErr
		}))
		w := rs.do(http.MethodGet, "/contacts/7")
		require.ErrorIs(t, hijackErr, http.ErrNotSupported)
		require.Equal(t, "error page", w.Body.String())
		require.Equal(t, []string{"request failed"}, rs.bodies())
	})
}

func TestSite_passes_a_panic_with_ErrAbortHandler_through(t *testing.T) {
	for name, before := range map[string]func(w http.ResponseWriter){
		"before the first write": func(http.ResponseWriter) {},
		"after the first write":  func(w http.ResponseWriter) { _, _ = io.WriteString(w, "partial") },
	} {
		t.Run(name, func(t *testing.T) {
			rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
				before(w)
				panic(http.ErrAbortHandler)
			}))
			require.PanicsWithValue(t, http.ErrAbortHandler, func() { rs.do(http.MethodGet, "/contacts/7") })
			require.Empty(t, rs.pages, "the ErrorPage doesn't answer")
			require.Equal(t, sdktrace.Status{Code: codes.Unset}, rs.span().Status())
			require.Empty(t, rs.span().Events())
			require.Empty(t, rs.records())
		})
	}

	t.Run("from the ErrorPage", func(t *testing.T) {
		rs := newRecorded(t, func(http.ResponseWriter, *http.Request, error) error {
			panic(http.ErrAbortHandler)
		}, hypermedia.NewRoute(http.MethodGet, One, says("one")))
		require.PanicsWithValue(t, http.ErrAbortHandler, func() { rs.do(http.MethodGet, "/orders") })
		require.Empty(t, rs.span().Events())
		require.Empty(t, rs.records())
	})
}

func TestSite_records_an_ErrorPage_that_fails_or_panics(t *testing.T) {
	pageErr := errors.New("the error template is broken")
	type answer struct {
		code   int
		header string // the page's X-Page header
		body   string
	}
	cases := []struct {
		name  string
		page  hypermedia.ErrorPage
		want  answer
		abort bool // the response is aborted
		panic bool // the page panicked
	}{
		{
			name: "an error before it writes gets a bare 500",
			page: func(w http.ResponseWriter, _ *http.Request, _ error) error {
				w.Header().Set("X-Page", "sorry")
				return pageErr
			},
			want: answer{code: http.StatusInternalServerError},
		},
		{
			name: "a panic before it writes gets a bare 500",
			page: func(w http.ResponseWriter, _ *http.Request, _ error) error {
				w.Header().Set("X-Page", "sorry")
				panic("the error template is broken")
			},
			want:  answer{code: http.StatusInternalServerError},
			panic: true,
		},
		{
			name: "an error after it writes ends the response as written",
			page: func(w http.ResponseWriter, _ *http.Request, _ error) error {
				w.Header().Set("X-Page", "sorry")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, "<p>Sor")
				return pageErr
			},
			want: answer{code: http.StatusServiceUnavailable, header: "sorry", body: "<p>Sor"},
		},
		{
			name: "a panic after it writes aborts the response",
			page: func(w http.ResponseWriter, _ *http.Request, _ error) error {
				_, _ = io.WriteString(w, "<p>Sor")
				panic("the error template is broken")
			},
			abort: true,
			panic: true,
		},
	}
	for _, tc := range cases {
		for _, req := range []struct {
			name   string
			target string
			route  string   // the http.route of the records, or ""
			before []string // what is recorded before the page
		}{
			{name: "for a handler's error", target: "/contacts/7", route: "/contacts/{contactId}", before: []string{"request failed"}},
			{name: "for a path no route serves", target: "/orders"},
		} {
			t.Run(tc.name+" "+req.name, func(t *testing.T) {
				rs := newRecorded(t, tc.page, hypermedia.NewRoute(http.MethodGet, One, fails(errors.New("the database is down"))))
				w := httptest.NewRecorder()
				if tc.abort {
					require.PanicsWithValue(t, http.ErrAbortHandler, func() {
						rs.serve(w, httptest.NewRequest(http.MethodGet, req.target, nil))
					})
				} else {
					rs.serve(w, httptest.NewRequest(http.MethodGet, req.target, nil))
					require.Equal(t, tc.want, answer{code: w.Code, header: w.Header().Get("X-Page"), body: w.Body.String()})
				}

				require.Equal(t, append(req.before, "error page failed"), rs.bodies())
				rec := rs.records()[len(req.before)]
				require.Equal(t, req.route, rec.attrs["http.route"])
				require.Equal(t, "GET", rec.attrs["http.request.method"])
				require.Equal(t, "the error template is broken", rec.attrs["exception.message"])
				if tc.panic {
					require.Equal(t, "string", rec.attrs["exception.type"])
					require.Contains(t, rec.attrs["exception.stacktrace"], "TestSite_records_an_ErrorPage_that_fails_or_panics")
				} else {
					require.Equal(t, "*errors.errorString", rec.attrs["error.type"])
				}
				require.Equal(t, sdktrace.Status{Code: codes.Error, Description: "error page failed"}, rs.span().Status())
				require.Len(t, rs.span().Events(), len(req.before)+1)
			})
		}
	}

	t.Run("a *RequestError it returns before it writes gets a bare 500, and isn't recorded", func(t *testing.T) {
		rs := newRecorded(t, func(http.ResponseWriter, *http.Request, error) error {
			return &hypermedia.RequestError{Status: http.StatusNotFound}
		}, hypermedia.NewRoute(http.MethodGet, One, says("one")))
		w := rs.do(http.MethodGet, "/orders")
		require.Equal(t, http.StatusInternalServerError, w.Code)
		require.Empty(t, w.Body.String())
		require.Empty(t, rs.records())
	})
}

func TestSite_records_on_the_span_alone_when_the_client_went_away(t *testing.T) {
	failed := errors.New("the database is down")
	gone := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	cases := []struct {
		name    string
		ctx     func() context.Context
		writeTo error // the error of a write to the connection, or nil
		// readFromTo is the error of the writer's own ReadFrom, or nil.
		readFromTo error
		handler    hypermedia.Handler
		want       []string // the records emitted
		status     string   // the span's status
	}{
		{
			name:    "an error before the response starts",
			ctx:     gone,
			handler: fails(failed),
			status:  "request failed",
		},
		{
			name: "an error after the response starts",
			ctx:  gone,
			handler: func(w http.ResponseWriter, _ *http.Request) error {
				w.WriteHeader(http.StatusOK)
				return failed
			},
			status: "response failed",
		},
		{
			name:    "a write that failed because the connection did",
			ctx:     context.Background,
			writeTo: errors.New("connection reset by peer"),
			handler: func(w http.ResponseWriter, _ *http.Request) error {
				_, err := io.WriteString(w, "partial")
				return err
			},
			status: "response failed",
		},
		{
			name:    "a write the server refused is logged whatever the context",
			ctx:     gone,
			writeTo: http.ErrBodyNotAllowed,
			handler: func(w http.ResponseWriter, _ *http.Request) error {
				_, err := io.WriteString(w, "partial")
				return err
			},
			want:   []string{"response failed"},
			status: "response failed",
		},
		{
			name:       "a ReadFrom the server refused is logged whatever the context",
			ctx:        gone,
			readFromTo: http.ErrContentLength,
			handler: func(w http.ResponseWriter, _ *http.Request) error {
				_, err := w.(io.ReaderFrom).ReadFrom(strings.NewReader("a file"))
				return err
			},
			want:   []string{"response failed"},
			status: "response failed",
		},
		{
			name:       "a ReadFrom that failed otherwise is the connection's when the context is done",
			ctx:        gone,
			readFromTo: errors.New("connection reset by peer"),
			handler: func(w http.ResponseWriter, _ *http.Request) error {
				_, err := w.(io.ReaderFrom).ReadFrom(strings.NewReader("a file"))
				return err
			},
			status: "response failed",
		},
		{
			name:       "a ReadFrom that failed otherwise is logged while the context is live",
			ctx:        context.Background,
			readFromTo: errors.New("the file is unreadable"),
			handler: func(w http.ResponseWriter, _ *http.Request) error {
				_, err := w.(io.ReaderFrom).ReadFrom(strings.NewReader("a file"))
				return err
			},
			want:   []string{"response failed"},
			status: "response failed",
		},
		{
			name: "a panic is logged whatever the context",
			ctx:  gone,
			handler: func(http.ResponseWriter, *http.Request) error {
				panic("the template is broken")
			},
			want:   []string{"handler panicked"},
			status: "handler panicked",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRecorded(t, func(http.ResponseWriter, *http.Request, error) error { return nil },
				hypermedia.NewRoute(http.MethodGet, One, tc.handler))
			fw := newFakeWriter()
			fw.writeErr = tc.writeTo
			fw.readFromErr = tc.readFromTo
			rs.serve(fw, httptest.NewRequestWithContext(tc.ctx(), http.MethodGet, "/contacts/7", nil))
			require.Equal(t, tc.want, rs.bodies())
			require.Equal(t, sdktrace.Status{Code: codes.Error, Description: tc.status}, rs.span().Status())
			require.Len(t, rs.span().Events(), 1)
		})
	}
}

// fakeWriter is an http.ResponseWriter with every optional method, which
// notes each call.
type fakeWriter struct {
	header   http.Header
	codes    []int
	body     strings.Builder
	writeErr error // the error of each Write, or nil
	// readFromErr is the error of each ReadFrom, or nil.
	readFromErr error

	flushes  int
	hijacked bool
	readFrom string
	deadline time.Time
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{header: make(http.Header)}
}

func (f *fakeWriter) Header() http.Header { return f.header }

func (f *fakeWriter) WriteHeader(code int) { f.codes = append(f.codes, code) }

func (f *fakeWriter) Write(b []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.body.Write(b)
}

func (f *fakeWriter) FlushError() error {
	f.flushes++
	return nil
}

func (f *fakeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijacked = true
	server, client := net.Pipe()
	_ = client.Close()
	return server, bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)), nil
}

func (f *fakeWriter) ReadFrom(r io.Reader) (int64, error) {
	if f.readFromErr != nil {
		return 0, f.readFromErr
	}
	b, err := io.ReadAll(r)
	f.readFrom += string(b)
	return int64(len(b)), err
}

func (f *fakeWriter) SetWriteDeadline(t time.Time) error {
	f.deadline = t
	return nil
}

func TestSite_forwards_the_optional_methods_of_its_ResponseWriter(t *testing.T) {
	fw := newFakeWriter()
	deadline := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var unwrapped http.ResponseWriter
	var calls []error
	rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
		w.(http.Flusher).Flush()
		calls = append(calls, w.(interface{ FlushError() error }).FlushError())
		conn, _, err := w.(http.Hijacker).Hijack()
		calls = append(calls, err)
		if conn != nil {
			calls = append(calls, conn.Close())
		}
		_, err = w.(io.ReaderFrom).ReadFrom(strings.NewReader("a file"))
		calls = append(calls, err)
		unwrapped = w.(interface{ Unwrap() http.ResponseWriter }).Unwrap()
		calls = append(calls, http.NewResponseController(w).SetWriteDeadline(deadline))
		return nil
	}))
	rs.serve(fw, httptest.NewRequest(http.MethodGet, "/contacts/7", nil))
	require.Equal(t, []error{nil, nil, nil, nil, nil}, calls)
	require.Equal(t, 2, fw.flushes)
	require.True(t, fw.hijacked)
	require.Equal(t, "a file", fw.readFrom)
	require.Same(t, fw, unwrapped)
	require.Equal(t, deadline, fw.deadline)
	require.Empty(t, rs.pages)
	require.Empty(t, rs.records())

	t.Run("a ReadFrom of a writer without one copies with its Write", func(t *testing.T) {
		rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
			_, err := w.(io.ReaderFrom).ReadFrom(strings.NewReader("a file"))
			return err
		}))
		w := rs.do(http.MethodGet, "/contacts/7")
		require.Equal(t, "a file", w.Body.String())
		require.Empty(t, rs.pages)
	})

	t.Run("a Flush of a writer without one is ErrNotSupported", func(t *testing.T) {
		var flushErr error
		rs := newRecorded(t, nil, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
			flushErr = w.(interface{ FlushError() error }).FlushError()
			return nil
		}))
		rs.serve(struct{ http.ResponseWriter }{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/contacts/7", nil))
		require.ErrorIs(t, flushErr, http.ErrNotSupported)
	})
}
