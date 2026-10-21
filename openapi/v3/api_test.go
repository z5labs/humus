// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	openapi "github.com/z5labs/humus/openapi/v3"
)

// loggerName is the logger the package logs through.
const loggerName = "github.com/z5labs/humus/openapi/v3"

// The paths of the carts API the tests serve.
var (
	cartID = openapi.PathParam[string]("cartId")
	carts  = openapi.Root().Lit("carts")
	cart   = carts.Param(cartID)
	items  = cart.Lit("items")
)

// cartBody is a cart, as the API returns it.
type cartBody struct {
	ID string `json:"id"`
}

// item is an item, as a client adds it and the API returns it.
type item struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

// getCart answers GET /carts/{cartId}: "missing" is a notFound, "gone" a
// gone, "broken" an error nothing but the catch-all answers, and any other
// ID a cart.
func getCart(_ context.Context, req openapi.Request[openapi.EmptyBody]) (cartBody, error) {
	id := openapi.ParamFrom(req, cartID)
	switch id {
	case "missing":
		return cartBody{}, notFound{problem{Status: http.StatusNotFound, Title: "Not Found", Detail: "no cart " + id}}
	case "gone":
		return cartBody{}, gone{problem{Status: http.StatusGone, Title: "Gone", Detail: "cart " + id + " is gone"}}
	case "broken":
		return cartBody{}, errors.New("the database is down")
	}
	return cartBody{ID: id}, nil
}

// addItem answers POST /carts/{cartId}/items with the item it was given.
func addItem(_ context.Context, req openapi.Request[item]) (item, error) {
	if openapi.ParamFrom(req, cartID) == "missing" {
		return item{}, notFound{problem{Status: http.StatusNotFound, Title: "Not Found"}}
	}
	return req.Body(), nil
}

// unmatched is an Unmatched whose answers are the end user's own: plain
// text, with the Allow header the 405 handler was called with.
var unmatched = openapi.Unmatched{
	NotFound: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "the end user's 404")
	}),
	MethodNotAllowed: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		allow := w.Header().Get("Allow")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, "the end user's 405, allowing "+allow)
	}),
}

// newAPI is NewAPI with the info, Unmatched, RequestErrors and catch-all
// every test shares.
func newAPI(s *humus.Setup, ops []openapi.Operation, opts ...openapi.APIOption) (*openapi.API, error) {
	return openapi.NewAPI(s,
		openapi.Info{Title: "Carts", Version: "1.0.0"},
		unmatched,
		openapi.RequestErrors(rejected, problemEncoder),
		openapi.CatchAll(http.StatusInternalServerError, internalError, problemEncoder),
		ops,
		opts...,
	)
}

// server is an App serving an API through httpserver, on a humustest Host.
type server struct {
	t      *testing.T
	tel    *humustest.Telemetry
	client *http.Client // speaks only HTTP/2
	http1  *http.Client // speaks only HTTP/1.1
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

// serve starts an App whose only Service serves the handler build returns,
// with httpserver, through httpservertest, recording its telemetry.
func serve(t *testing.T, build func(s *humus.Setup) (http.Handler, error)) *server {
	t.Helper()
	return serveOn(t, func(h humus.Host) humus.Host { return h }, build)
}

// serveOn is serve, on the Host host returns from the humustest Host.
func serveOn(t *testing.T, host func(humus.Host) humus.Host, build func(s *humus.Setup) (http.Handler, error)) *server {
	t.Helper()
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "localhost")
	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()

	p, status := httpservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		api, err := build(s)
		if err != nil {
			return nil, err
		}
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, api)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(host(h.Host())), humus.WithTelemetry(tel.Builder())))
	select {
	case <-h.Listening("localhost:8443"):
	case st := <-status:
		t.Fatalf("the App exited before it listened: %v", st.Cause())
	}

	return &server{t: t, tel: tel, client: httpservertest.NewClient(h, ca.Pool()), http1: http1Client(h, ca), p: p, status: status}
}

// http1Client returns an http.Client that dials h, trusts only ca, and
// speaks only HTTP/1.1.
func http1Client(h *humustest.Host, ca *humustest.CA) *http.Client {
	c := h.HTTPClient()
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: ca.Pool(), NextProtos: []string{"http/1.1"}}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	tr.Protocols = &protocols
	return c
}

// serveAPI serves the API newAPI builds from ops and opts.
func serveAPI(t *testing.T, ops []openapi.Operation, opts ...openapi.APIOption) *server {
	t.Helper()
	return serve(t, func(s *humus.Setup) (http.Handler, error) {
		return newAPI(s, ops, opts...)
	})
}

// stop stops the App, so every span the requests started has ended, and
// checks that it exited cleanly, having written nothing to stderr.
func (s *server) stop() {
	s.t.Helper()
	s.p.Signal(syscall.SIGTERM)
	st := <-s.status
	require.Zero(s.t, st.Code(), "cause: %v", st.Cause())
	require.Empty(s.t, s.p.Stderr())
}

// reply is a response, read in full.
type reply struct {
	status      int
	contentType string
	header      http.Header
	body        string
}

// do sends a request of method to path, with body as contentType when
// contentType isn't "".
func (s *server) do(method, path, contentType, body string) reply {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), method, "https://localhost:8443"+path, strings.NewReader(body))
	require.NoError(s.t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return s.send(req)
}

// send sends req.
func (s *server) send(req *http.Request) reply {
	s.t.Helper()
	resp, err := s.client.Do(req)
	require.NoError(s.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	return reply{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), header: resp.Header, body: string(b)}
}

func (s *server) get(path string) reply {
	s.t.Helper()
	return s.do(http.MethodGet, path, "", "")
}

// serverSpans returns the spans httpserver's otelhttp recorded.
func serverSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range tel.Spans() {
		if s.InstrumentationScope().Name == otelhttp.ScopeName {
			spans = append(spans, s)
		}
	}
	return spans
}

// spanNames returns the names of the server spans.
func spanNames(tel *humustest.Telemetry) []string {
	var names []string
	for _, s := range serverSpans(tel) {
		names = append(names, s.Name())
	}
	return names
}

// records returns the records the package logged.
func records(tel *humustest.Telemetry) []sdklog.Record {
	var recs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == loggerName {
			recs = append(recs, r)
		}
	}
	return recs
}

// attrs returns r's attributes as strings.
func attrs(r sdklog.Record) map[string]string {
	m := make(map[string]string)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		m[string(kv.Key)] = kv.Value.String()
		return true
	})
	return m
}

// exceptionMessages returns the exception.message of each exception event
// on span.
func exceptionMessages(span sdktrace.ReadOnlySpan) []string {
	var msgs []string
	for _, e := range span.Events() {
		if e.Name != "exception" {
			continue
		}
		for _, kv := range e.Attributes {
			if kv.Key == "exception.message" {
				msgs = append(msgs, kv.Value.AsString())
			}
		}
	}
	return msgs
}

func TestAPI_serves_an_operation(t *testing.T) {
	t.Run("a GET, its span named after the route", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder)})

			r := s.get("/carts/c-1")
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "application/json", r.contentType)
			require.JSONEq(t, `{"id":"c-1"}`, r.body)

			s.stop()
			require.Equal(t, []string{"GET /carts/{cartId}"}, spanNames(s.tel))
			require.Empty(t, records(s.tel))
		})
	})

	t.Run("a POST, with its body decoded and its Status", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, []openapi.Operation{
				openapi.Post(items, addItem, jsonDecoder, jsonEncoder, openapi.Status(http.StatusCreated)),
			})

			r := s.do(http.MethodPost, "/carts/c-1/items", "application/json; charset=utf-8", `{"name":"tea","quantity":2}`)
			require.Equal(t, http.StatusCreated, r.status)
			require.Equal(t, "application/json", r.contentType)
			require.JSONEq(t, `{"name":"tea","quantity":2}`, r.body)

			s.stop()
			require.Equal(t, []string{"POST /carts/{cartId}/items"}, spanNames(s.tel))
		})
	})

	t.Run("the root, its span named GET /", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, []openapi.Operation{
				openapi.Get(openapi.Root(), func(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
					return cartBody{ID: "root"}, nil
				}, jsonEncoder),
			})

			r := s.get("/")
			require.Equal(t, http.StatusOK, r.status)
			require.JSONEq(t, `{"id":"root"}`, r.body)

			s.stop()
			require.Equal(t, []string{"GET /"}, spanNames(s.tel))
		})
	})

	t.Run("an EmptyEncoder response, a 204 with no body and no Content-Type", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			removed := make(chan string, 1)
			s := serveAPI(t, []openapi.Operation{
				openapi.Delete(cart, func(_ context.Context, req openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
					removed <- openapi.ParamFrom(req, cartID)
					return openapi.EmptyBody{}, nil
				}, openapi.EmptyEncoder),
				openapi.Put(cart, func(context.Context, openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
					return openapi.EmptyBody{}, nil
				}, openapi.EmptyDecoder, openapi.EmptyEncoder, openapi.Status(http.StatusAccepted)),
			})

			r := s.do(http.MethodDelete, "/carts/c-1", "", "")
			require.Equal(t, http.StatusNoContent, r.status)
			require.Empty(t, r.contentType)
			require.Empty(t, r.body)
			require.Equal(t, "c-1", <-removed)

			// EmptyDecoder reads no body, so no Content-Type is checked,
			// and Status replaces the 204.
			r = s.do(http.MethodPut, "/carts/c-1", "", "")
			require.Equal(t, http.StatusAccepted, r.status)
			require.Empty(t, r.contentType)

			s.stop()
		})
	})

	t.Run("a path parameter of another type, parsed before the handler runs", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			page := openapi.PathParam[int64]("page")
			s := serveAPI(t, []openapi.Operation{
				openapi.Get(openapi.Root().Lit("pages").Param(page), func(_ context.Context, req openapi.Request[openapi.EmptyBody]) (int64, error) {
					return openapi.ParamFrom(req, page) * 2, nil
				}, jsonEncoder),
				openapi.Post(items, addItem, jsonDecoder, jsonEncoder),
			})

			r := s.get("/pages/21")
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "42", r.body)

			r = s.do(http.MethodPost, "/carts//items", "application/json", `{"name":"tea"}`)
			require.Equal(t, http.StatusBadRequest, r.status)
			require.JSONEq(t, `{"status":400,"title":"Bad Request","param":"cartId","detail":"path parameter \"cartId\" is empty"}`, r.body,
				"chi matches an empty segment, but a path parameter is required")

			r = s.get("/pages/twenty")
			require.Equal(t, http.StatusBadRequest, r.status)
			require.Equal(t, "application/problem+json", r.contentType)
			require.JSONEq(t, `{"status":400,"title":"Bad Request","param":"page","detail":"path parameter \"page\": \"twenty\" isn't an integer of 64 bits"}`, r.body)

			s.stop()
			require.Empty(t, records(s.tel), "a rejected request is the client's mistake")
		})
	})
}

func TestAPI_unescapes_path_values(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t, []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder)})

		r := s.get("/carts/a%2Fb")
		require.Equal(t, http.StatusOK, r.status)
		require.JSONEq(t, `{"id":"a/b"}`, r.body)

		// A path whose escaped form is its default one is matched
		// unescaped, and isn't unescaped twice.
		r = s.get("/carts/100%25")
		require.Equal(t, http.StatusOK, r.status)
		require.JSONEq(t, `{"id":"100%"}`, r.body)

		s.stop()
	})
}

func TestAPI_answers_unmatched_requests_with_Unmatched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t, []openapi.Operation{
			openapi.Get(cart, getCart, jsonEncoder),
			openapi.Delete(cart, func(context.Context, openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
				return openapi.EmptyBody{}, nil
			}, openapi.EmptyEncoder),
		})

		r := s.get("/orders/o-1")
		require.Equal(t, http.StatusNotFound, r.status)
		require.Equal(t, "the end user's 404", r.body)

		// chi redirects nothing, where ServeMux redirects //carts.
		r = s.get("//carts/c-1")
		require.Equal(t, http.StatusNotFound, r.status)
		require.Equal(t, "the end user's 404", r.body)
		require.Empty(t, r.header.Get("Location"))

		r = s.do(http.MethodPost, "/carts/c-1", "application/json", `{}`)
		require.Equal(t, http.StatusMethodNotAllowed, r.status)
		require.Equal(t, "the end user's 405, allowing GET, DELETE", r.body)
		require.Equal(t, []string{"GET, DELETE"}, r.header.Values("Allow"))

		// A method chi doesn't know is a 405 at a path an operation serves,
		// and a 404 at any other.
		r = s.do("PURGE", "/carts/c-1", "", "")
		require.Equal(t, http.StatusMethodNotAllowed, r.status)
		require.Equal(t, "the end user's 405, allowing GET, DELETE", r.body)
		r = s.do("PURGE", "/orders/o-1", "", "")
		require.Equal(t, http.StatusNotFound, r.status)
		require.Equal(t, "the end user's 404", r.body)

		s.stop()
		require.Empty(t, records(s.tel), "the package logs nothing for an unmatched request")
		require.Equal(t, []string{"GET", "GET", "POST", "HTTP", "HTTP"}, spanNames(s.tel),
			"an unmatched request has no route")
	})
}

func TestAPI_answers_rejected_requests_with_RequestErrors(t *testing.T) {
	ops := func() []openapi.Operation {
		return []openapi.Operation{
			openapi.Post(items, addItem, jsonDecoder, jsonEncoder),
			openapi.Put(items, addItem, jsonDecoder, jsonEncoder,
				openapi.RequestErrors(rejectedByOperation, problemEncoder),
			),
		}
	}

	t.Run("a 415 for another Content-Type, or none", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, ops())

			r := s.do(http.MethodPost, "/carts/c-1/items", "text/plain", `{"name":"tea"}`)
			require.Equal(t, http.StatusUnsupportedMediaType, r.status)
			require.Equal(t, "application/problem+json", r.contentType)
			require.JSONEq(t, `{"status":415,"title":"Unsupported Media Type","detail":"the request's Content-Type is \"text/plain\", but the operation reads application/json"}`, r.body)

			r = s.do(http.MethodPost, "/carts/c-1/items", "", `{"name":"tea"}`)
			require.Equal(t, http.StatusUnsupportedMediaType, r.status)
			require.JSONEq(t, `{"status":415,"title":"Unsupported Media Type","detail":"the request has no Content-Type, but the operation reads application/json"}`, r.body)

			r = s.do(http.MethodPut, "/carts/c-1/items", "text/plain", `{"name":"tea"}`)
			require.Equal(t, http.StatusUnsupportedMediaType, r.status)
			require.JSONEq(t, `{"status":415,"title":"rejected by the operation","detail":"the request's Content-Type is \"text/plain\", but the operation reads application/json"}`, r.body)

			s.stop()
			require.Empty(t, records(s.tel))
		})
	})

	t.Run("a 400 for a body that doesn't decode", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, ops())

			r := s.do(http.MethodPost, "/carts/c-1/items", "application/json", `{"name":`)
			require.Equal(t, http.StatusBadRequest, r.status)
			require.Equal(t, "application/problem+json", r.contentType)
			require.JSONEq(t, `{"status":400,"title":"Bad Request","detail":"unexpected EOF"}`, r.body)

			r = s.do(http.MethodPut, "/carts/c-1/items", "application/json", `{"name":"tea"} {}`)
			require.Equal(t, http.StatusBadRequest, r.status)
			require.JSONEq(t, `{"status":400,"title":"rejected by the operation","detail":"data after the JSON value"}`, r.body)

			s.stop()
			require.Empty(t, records(s.tel))
		})
	})

	t.Run("a 413 for a body over http.MaxBytesHandler's limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				api, err := newAPI(s, ops())
				if err != nil {
					return nil, err
				}
				return http.MaxBytesHandler(api, 8), nil
			})

			r := s.do(http.MethodPost, "/carts/c-1/items", "application/json", `{"name":"a name longer than eight bytes"}`)
			require.Equal(t, http.StatusRequestEntityTooLarge, r.status)
			require.JSONEq(t, `{"status":413,"title":"Request Entity Too Large","detail":"http: request body too large"}`, r.body)

			r = s.do(http.MethodPut, "/carts/c-1/items", "application/json", `{"name":"a name longer than eight bytes"}`)
			require.Equal(t, http.StatusRequestEntityTooLarge, r.status)
			require.JSONEq(t, `{"status":413,"title":"rejected by the operation","detail":"http: request body too large"}`, r.body)

			s.stop()
			require.Empty(t, records(s.tel))
		})
	})
}

func TestAPI_answers_errors_with_Error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t,
			[]openapi.Operation{
				openapi.Get(cart, getCart, jsonEncoder),
				// The operation's own Error for notFound replaces the API's,
				// and answers with another status and encoder.
				openapi.Post(items, addItem, jsonDecoder, jsonEncoder,
					openapi.Error[notFound](http.StatusConflict, jsonEncoder),
				),
			},
			openapi.Error[notFound](http.StatusNotFound, problemEncoder),
			openapi.Error[gone](http.StatusGone, problemEncoder),
		)

		r := s.get("/carts/missing")
		require.Equal(t, http.StatusNotFound, r.status)
		require.Equal(t, "application/problem+json", r.contentType)
		require.JSONEq(t, `{"status":404,"title":"Not Found","detail":"no cart missing"}`, r.body)

		r = s.get("/carts/gone")
		require.Equal(t, http.StatusGone, r.status)
		require.JSONEq(t, `{"status":410,"title":"Gone","detail":"cart gone is gone"}`, r.body)

		r = s.do(http.MethodPost, "/carts/missing/items", "application/json", `{"name":"tea"}`)
		require.Equal(t, http.StatusConflict, r.status)
		require.Equal(t, "application/json", r.contentType)
		require.JSONEq(t, `{"status":404,"title":"Not Found"}`, r.body)

		s.stop()
		require.Empty(t, records(s.tel), "an Error's answer isn't logged")
	})
}

func TestAPI_answers_other_errors_with_the_catch_all(t *testing.T) {
	t.Run("the error, answered and recorded", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder)})

			r := s.get("/carts/broken")
			require.Equal(t, http.StatusInternalServerError, r.status)
			require.Equal(t, "application/problem+json", r.contentType)
			require.JSONEq(t, `{"status":500,"title":"Internal Server Error"}`, r.body, "wrap hides the error's text")

			s.stop()
			spans := serverSpans(s.tel)
			require.Len(t, spans, 1)
			require.Equal(t, []string{"the database is down"}, exceptionMessages(spans[0]))

			recs := records(s.tel)
			require.Len(t, recs, 1)
			require.Equal(t, "request failed", recs[0].Body().AsString())
			require.Equal(t, log.SeverityError, recs[0].Severity())
			require.Equal(t, spans[0].SpanContext().TraceID(), recs[0].TraceID())
			require.Equal(t, spans[0].SpanContext().SpanID(), recs[0].SpanID())
			require.Equal(t, map[string]string{
				"http.request.method": "GET",
				"http.route":          "/carts/{cartId}",
				"error.type":          "*errors.errorString",
				"exception.message":   "the database is down",
			}, attrs(recs[0]))
		})
	})

	t.Run("the error itself, when it matches E", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, []openapi.Operation{
				openapi.Get(cart, getCart, jsonEncoder,
					openapi.CatchAll(http.StatusServiceUnavailable, func(error) notFound {
						return notFound{problem{Title: "wrapped"}}
					}, jsonEncoder),
				),
			})

			// notFound matches E, so it is answered as itself.
			r := s.get("/carts/missing")
			require.Equal(t, http.StatusServiceUnavailable, r.status)
			require.Equal(t, "application/json", r.contentType)
			require.JSONEq(t, `{"status":404,"title":"Not Found","detail":"no cart missing"}`, r.body)

			r = s.get("/carts/broken")
			require.Equal(t, http.StatusServiceUnavailable, r.status)
			require.JSONEq(t, `{"status":0,"title":"wrapped"}`, r.body)

			s.stop()
			require.Len(t, records(s.tel), 2)
		})
	})

	t.Run("an error after the client went away, on the span only", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			started := make(chan struct{})
			s := serveAPI(t, []openapi.Operation{
				openapi.Get(cart, func(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (cartBody, error) {
					close(started)
					<-ctx.Done()
					return cartBody{}, ctx.Err()
				}, jsonEncoder),
			})

			ctx, cancel := context.WithCancel(t.Context())
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://localhost:8443/carts/c-1", nil)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				resp, err := s.client.Do(req)
				if err == nil {
					_ = resp.Body.Close()
				}
				done <- err
			}()
			<-started
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)

			s.stop()
			spans := serverSpans(s.tel)
			require.Len(t, spans, 1)
			require.Equal(t, []string{"context canceled"}, exceptionMessages(spans[0]))
			require.Empty(t, records(s.tel))
		})
	})

	t.Run("an encoder's error, never a 200 with a broken body", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, []openapi.Operation{
				openapi.Get(cart, func(context.Context, openapi.Request[openapi.EmptyBody]) (map[string]any, error) {
					return map[string]any{"ch": make(chan int)}, nil
				}, jsonEncoder),
			})

			r := s.get("/carts/c-1")
			require.Equal(t, http.StatusInternalServerError, r.status)
			require.JSONEq(t, `{"status":500,"title":"Internal Server Error"}`, r.body)

			s.stop()
			recs := records(s.tel)
			require.Len(t, recs, 1)
			require.Equal(t, "encoding the response: json: unsupported type: chan int", attrs(recs[0])["exception.message"])
		})
	})

	t.Run("an Error's encoder's error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t,
				[]openapi.Operation{openapi.Get(cart, func(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
					return cartBody{}, unencodable{Ch: make(chan int)}
				}, jsonEncoder)},
				openapi.Error[unencodable](http.StatusConflict, jsonEncoder),
			)

			r := s.get("/carts/c-1")
			require.Equal(t, http.StatusInternalServerError, r.status)
			require.JSONEq(t, `{"status":500,"title":"Internal Server Error"}`, r.body)

			s.stop()
			recs := records(s.tel)
			require.Len(t, recs, 1)
			require.Contains(t, attrs(recs[0])["exception.message"], "json: unsupported type: chan int")
		})
	})

	t.Run("the catch-all's own encoder's error, its status alone", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, []openapi.Operation{
				openapi.Get(cart, getCart, jsonEncoder,
					openapi.CatchAll(http.StatusBadGateway, func(error) unencodable {
						return unencodable{Ch: make(chan int)}
					}, jsonEncoder),
				),
			})

			r := s.get("/carts/broken")
			require.Equal(t, http.StatusBadGateway, r.status)
			require.Empty(t, r.body)
			require.Empty(t, r.contentType)

			s.stop()
			require.Len(t, records(s.tel), 2, "the error, and the encoder's")
		})
	})
}

// unencodable is an error JSON can't encode, though its schema reflects:
// its Ch holds a channel.
type unencodable struct {
	Ch any `json:"ch"`
}

func (unencodable) Error() string { return "unencodable" }
