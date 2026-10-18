// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	openapi "github.com/z5labs/humus/openapi/v3"
)

// secret is what the panicking code in this file panics with. The client
// must never see it.
const secret = "secret: the end user's bug"

// panicCodec is a codec of item whose Decode, Encode or WriterTo panics
// with secret, as its fields say.
type panicCodec struct {
	decode, encode, writeTo bool
}

func (panicCodec) MediaType() string { return "application/json" }

func (panicCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (c panicCodec) Decode(r *http.Request) (item, error) {
	if c.decode {
		panic(secret)
	}
	var v item
	err := json.NewDecoder(r.Body).Decode(&v)
	return v, err
}

func (c panicCodec) Encode(v item) (io.WriterTo, error) {
	if c.encode {
		panic(secret)
	}
	return writerFunc(func(w io.Writer) (int64, error) {
		n, err := io.WriteString(w, `{"name":"`+v.Name+`",`)
		if err != nil {
			return int64(n), err
		}
		if c.writeTo {
			panic(secret)
		}
		m, err := io.WriteString(w, `"quantity":1}`)
		return int64(n + m), err
	}), nil
}

// decoderOf and encoderOf return codec functions for c, as Post takes
// them.
func decoderOf(c panicCodec) func() openapi.Decoder[item] {
	return func() openapi.Decoder[item] { return c }
}

func encoderOf(c panicCodec) func() openapi.Encoder[item] {
	return func() openapi.Encoder[item] { return c }
}

// detailedError is an operation's own catch-all wrap, which puts the error
// it is given in the body, so a test can see what wrap was handed.
func detailedError(err error) problem {
	return problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable", Detail: err.Error()}
}

// exceptions returns the attributes of each exception event on span.
func exceptions(span sdktrace.ReadOnlySpan) []map[string]string {
	var evs []map[string]string
	for _, e := range span.Events() {
		if e.Name != "exception" {
			continue
		}
		m := make(map[string]string)
		for _, kv := range e.Attributes {
			m[string(kv.Key)] = kv.Value.String()
		}
		evs = append(evs, m)
	}
	return evs
}

// logged returns the records with body, whoever emitted them.
func logged(s *server, body string) []sdklog.Record {
	var recs []sdklog.Record
	for _, r := range s.tel.Logs() {
		if r.Body().AsString() == body {
			recs = append(recs, r)
		}
	}
	return recs
}

// recordAttr returns the value of r's attribute key.
func recordAttr(r sdklog.Record, key string) string {
	var v string
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == key {
			v = kv.Value.String()
			return false
		}
		return true
	})
	return v
}

// requirePanicRecorded checks that the one server span ending in a panic of
// the end user's code, and the one "operation panicked" record, both hold
// the panic with its stack, and that httpserver recorded no panic.
func requirePanicRecorded(t *testing.T, s *server, method, route string) {
	t.Helper()
	var panicked []sdktrace.ReadOnlySpan
	for _, span := range serverSpans(s.tel) {
		if len(exceptions(span)) > 0 {
			panicked = append(panicked, span)
		}
	}
	require.Len(t, panicked, 1, "want one span with an exception event")
	span := panicked[0]
	evs := exceptions(span)
	require.Len(t, evs, 1)
	require.Equal(t, "string", evs[0]["exception.type"])
	require.Equal(t, secret, evs[0]["exception.message"])
	require.Contains(t, evs[0]["exception.stacktrace"], "panic_test.go", "the stack is the panic's")
	require.Equal(t, codes.Error, span.Status().Code)

	recs := records(s.tel)
	require.Len(t, recs, 1)
	rec := recs[0]
	require.Equal(t, "operation panicked", rec.Body().AsString())
	require.Equal(t, log.SeverityError, rec.Severity())
	require.Equal(t, span.SpanContext().TraceID(), rec.TraceID())
	require.Equal(t, span.SpanContext().SpanID(), rec.SpanID())
	got := attrs(rec)
	require.Contains(t, got["exception.stacktrace"], "panic_test.go", "the stack is the panic's")
	delete(got, "exception.stacktrace")
	require.Equal(t, map[string]string{
		"http.request.method": method,
		"http.route":          route,
		"exception.type":      "string",
		"exception.message":   secret,
	}, got)

	require.Empty(t, logged(s, "handler panicked"), "httpserver recorded the panic too")
}

func TestAPI_answers_a_panic_with_the_catch_all(t *testing.T) {
	handler := func(context.Context, openapi.Request[item]) (item, error) { panic(secret) }
	for _, tc := range []struct {
		name string
		op   func(...openapi.Option) openapi.Operation
	}{
		{"in the handler", func(opts ...openapi.Option) openapi.Operation {
			return openapi.Post(items, handler, decoderOf(panicCodec{}), encoderOf(panicCodec{}), opts...)
		}},
		{"in Decode", func(opts ...openapi.Option) openapi.Operation {
			return openapi.Post(items, addItem, decoderOf(panicCodec{decode: true}), encoderOf(panicCodec{}), opts...)
		}},
		{"in Encode", func(opts ...openapi.Option) openapi.Operation {
			return openapi.Post(items, addItem, decoderOf(panicCodec{}), encoderOf(panicCodec{encode: true}), opts...)
		}},
	} {
		t.Run(tc.name+", by the API's catch-all", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serveAPI(t, []openapi.Operation{
					tc.op(),
					openapi.Get(cart, getCart, jsonEncoder),
				})

				r := s.do(http.MethodPost, "/carts/c-1/items", "application/json", `{"name":"apple","quantity":1}`)
				require.Equal(t, http.StatusInternalServerError, r.status)
				require.Equal(t, "application/problem+json", r.contentType)
				require.JSONEq(t, `{"status":500,"title":"Internal Server Error"}`, r.body)
				require.NotContains(t, r.body, "secret")

				next := s.get("/carts/c-1")
				require.Equal(t, http.StatusOK, next.status, "the API keeps serving")
				require.JSONEq(t, `{"id":"c-1"}`, next.body)

				s.stop()
				requirePanicRecorded(t, s, http.MethodPost, "/carts/{cartId}/items")
			})
		})

		t.Run(tc.name+", by the operation's own catch-all, given an error without the panic's value", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serveAPI(t, []openapi.Operation{
					tc.op(openapi.CatchAll(http.StatusServiceUnavailable, detailedError, problemEncoder)),
				})

				r := s.do(http.MethodPost, "/carts/c-1/items", "application/json", `{"name":"apple","quantity":1}`)
				require.Equal(t, http.StatusServiceUnavailable, r.status)
				var body problem
				require.NoError(t, json.Unmarshal([]byte(r.body), &body))
				require.Equal(t, http.StatusServiceUnavailable, body.Status)
				require.NotEmpty(t, body.Detail, "wrap was given an error")
				require.NotContains(t, r.body, "secret")

				s.stop()
				requirePanicRecorded(t, s, http.MethodPost, "/carts/{cartId}/items")
			})
		})
	}
}

func TestAPI_answers_a_panic_in_ParamFrom_with_the_catch_all(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		other := openapi.PathParam[string]("other")
		s := serveAPI(t, []openapi.Operation{
			openapi.Get(cart, func(_ context.Context, req openapi.Request[openapi.EmptyBody]) (cartBody, error) {
				return cartBody{ID: openapi.ParamFrom(req, other)}, nil
			}, jsonEncoder),
		})

		r := s.get("/carts/c-1")
		require.Equal(t, http.StatusInternalServerError, r.status)
		require.JSONEq(t, `{"status":500,"title":"Internal Server Error"}`, r.body)

		s.stop()
		recs := records(s.tel)
		require.Len(t, recs, 1)
		require.Equal(t, "operation panicked", recs[0].Body().AsString())
		require.Contains(t, recordAttr(recs[0], "exception.message"), "doesn't read path parameter \"other\"")
	})
}

func TestAPI_sends_the_catch_alls_status_alone_when_its_encoder_fails_on_a_panic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failing := func(error) unencodable { return unencodable{Ch: make(chan int)} }
		s := serveAPI(t, []openapi.Operation{
			openapi.Post(items, func(context.Context, openapi.Request[item]) (item, error) { panic(secret) },
				decoderOf(panicCodec{}), encoderOf(panicCodec{}),
				openapi.CatchAll(http.StatusServiceUnavailable, failing, jsonEncoder),
			),
		})

		r := s.do(http.MethodPost, "/carts/c-1/items", "application/json", `{"name":"apple","quantity":1}`)
		require.Equal(t, http.StatusServiceUnavailable, r.status)
		require.Empty(t, r.body)

		s.stop()
		var bodies []string
		for _, rec := range records(s.tel) {
			bodies = append(bodies, rec.Body().AsString())
		}
		require.Equal(t, []string{"operation panicked", "request failed"}, bodies)
	})
}

func TestAPI_cuts_a_response_short_when_its_WriterTo_panics(t *testing.T) {
	for _, p := range []struct {
		name    string
		client  func(*server) *http.Client
		aborted func(t *testing.T, err error)
	}{
		{
			name:   "over HTTP/2",
			client: func(s *server) *http.Client { return s.client },
			aborted: func(t *testing.T, err error) {
				require.ErrorContains(t, err, "stream error", "want the stream reset")
			},
		},
		{
			name:   "over HTTP/1.1",
			client: func(s *server) *http.Client { return s.http1 },
			aborted: func(t *testing.T, err error) {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF, "want the response cut short before its final chunk")
			},
		},
	} {
		t.Run(p.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serveAPI(t, []openapi.Operation{
					openapi.Post(items, addItem, decoderOf(panicCodec{}), encoderOf(panicCodec{writeTo: true})),
					openapi.Get(cart, getCart, jsonEncoder),
				})

				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://localhost:8443/carts/c-1/items",
					strings.NewReader(`{"name":"apple","quantity":1}`))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				r := <-start(p.client(s), req)
				require.NoError(t, r.err, "the status went out before the panic")
				require.Equal(t, http.StatusOK, r.resp.StatusCode)
				body, err := io.ReadAll(r.resp.Body)
				_ = r.resp.Body.Close()
				p.aborted(t, err)
				require.NotContains(t, string(body), "quantity", "the body was cut short")
				require.NotContains(t, string(body), "secret")

				next := s.get("/carts/c-1")
				require.Equal(t, http.StatusOK, next.status, "the API keeps serving")

				s.stop()
				requirePanicRecorded(t, s, http.MethodPost, "/carts/{cartId}/items")
			})
		})
	}
}

func TestAPI_passes_a_panic_with_ErrAbortHandler_through(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   openapi.Operation
	}{
		{"in the handler", openapi.Get(cart, func(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
			panic(http.ErrAbortHandler)
		}, jsonEncoder)},
		{"in the WriterTo", openapi.Get(cart, func(context.Context, openapi.Request[openapi.EmptyBody]) (writerFunc, error) {
			return func(w io.Writer) (int64, error) {
				n, _ := io.WriteString(w, "row 1\n")
				if n > 0 {
					panic(http.ErrAbortHandler)
				}
				return int64(n), nil
			}, nil
		}, writerFuncEncoder)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serveAPI(t, []openapi.Operation{tc.op, openapi.Get(carts, func(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
					return cartBody{ID: "all"}, nil
				}, jsonEncoder)})

				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/carts/c-1", nil)
				require.NoError(t, err)
				r := <-start(s.client, req)
				if r.err == nil {
					_, r.err = io.ReadAll(r.resp.Body)
					_ = r.resp.Body.Close()
				}
				require.ErrorContains(t, r.err, "stream error", "want the stream reset")

				next := s.get("/carts")
				require.Equal(t, http.StatusOK, next.status, "the API keeps serving")

				s.stop()
				for _, span := range serverSpans(s.tel) {
					require.Empty(t, exceptions(span), "span %q has an exception event", span.Name())
				}
				require.Empty(t, records(s.tel))
				require.Empty(t, logged(s, "handler panicked"))
			})
		})
	}
}

func TestAPI_doesnt_recover_a_panic_while_answering(t *testing.T) {
	// answerPanic is the panic of the end user's answer: httpserver records
	// it, as "handler panicked", and aborts the response.
	const answerPanic = "the answer's own bug"
	panickingWrap := func(error) problem { panic(answerPanic) }
	panickingEncoder := func() openapi.Encoder[notFound] {
		return encoderFunc[notFound](func(notFound) (io.WriterTo, error) { panic(answerPanic) })
	}
	// allCarts is the operation each API also serves, to show it keeps
	// serving after the answer's panic.
	allCarts := openapi.Get(carts, func(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
		return cartBody{ID: "all"}, nil
	}, jsonEncoder)
	unmatchedPanics := openapi.Unmatched{
		NotFound:         http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(answerPanic) }),
		MethodNotAllowed: unmatched.MethodNotAllowed,
	}
	for _, tc := range []struct {
		name      string
		build     func(s *humus.Setup) (http.Handler, error)
		path      string
		recovered int // "operation panicked" records, for the panic being answered
	}{
		{
			name: "in a catch-all's wrap, answering an error",
			build: func(s *humus.Setup) (http.Handler, error) {
				return newAPI(s, []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.CatchAll(http.StatusServiceUnavailable, panickingWrap, problemEncoder)), allCarts})
			},
			path: "/carts/broken",
		},
		{
			name: "in a catch-all's wrap, answering a panic",
			build: func(s *humus.Setup) (http.Handler, error) {
				return newAPI(s, []openapi.Operation{openapi.Get(cart, func(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
					panic(secret)
				}, jsonEncoder, openapi.CatchAll(http.StatusServiceUnavailable, panickingWrap, problemEncoder)), allCarts})
			},
			path:      "/carts/c-1",
			recovered: 1,
		},
		{
			name: "in an Error's encoder",
			build: func(s *humus.Setup) (http.Handler, error) {
				return newAPI(s, []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.Error[notFound](http.StatusNotFound, panickingEncoder)), allCarts})
			},
			path: "/carts/missing",
		},
		{
			name: "in RequestErrors' wrap",
			build: func(s *humus.Setup) (http.Handler, error) {
				n := openapi.PathParam[int]("n")
				return newAPI(s, []openapi.Operation{openapi.Get(carts.Param(n), func(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
					return cartBody{}, nil
				}, jsonEncoder, openapi.RequestErrors(func(*openapi.RequestError) problem { panic(answerPanic) }, problemEncoder)), allCarts})
			},
			path: "/carts/not-a-number",
		},
		{
			name: "in Unmatched's handler",
			build: func(s *humus.Setup) (http.Handler, error) {
				return openapi.NewAPI(s,
					openapi.Info{Title: "Carts", Version: "1.0.0"},
					unmatchedPanics,
					openapi.RequestErrors(rejected, problemEncoder),
					openapi.CatchAll(http.StatusInternalServerError, internalError, problemEncoder),
					[]openapi.Operation{allCarts},
				)
			},
			path: "/nowhere",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serve(t, tc.build)

				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443"+tc.path, nil)
				require.NoError(t, err)
				r := <-start(s.client, req)
				require.Error(t, r.err, "want the request aborted")
				require.ErrorContains(t, r.err, "stream error", "want the stream reset")
				require.ErrorContains(t, r.err, "INTERNAL_ERROR", "want the stream reset")

				next := s.get("/carts")
				require.Equal(t, http.StatusOK, next.status, "the API keeps serving")

				s.stop()
				recs := logged(s, "handler panicked")
				require.Len(t, recs, 1, "httpserver recorded the answer's panic")
				require.Equal(t, answerPanic, recordAttr(recs[0], "exception.message"))
				require.Len(t, logged(s, "operation panicked"), tc.recovered, "the answer's panic was recovered")
			})
		})
	}
}

// encoderFunc is an Encoder whose Encode is a function.
type encoderFunc[T any] func(T) (io.WriterTo, error)

func (encoderFunc[T]) MediaType() string { return "application/problem+json" }

func (encoderFunc[T]) Schema() (openapi.Schema, error) { return nil, nil }

func (f encoderFunc[T]) Encode(v T) (io.WriterTo, error) { return f(v) }
