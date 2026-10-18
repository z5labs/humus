// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package problem_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	openapi "github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
)

// xmlProblem is the tests' own encoder: T as application/problem+xml,
// marshalled with encoding/xml.
func xmlProblem[T any]() openapi.Encoder[T] { return xmlCodec[T]{} }

type xmlCodec[T any] struct{}

func (xmlCodec[T]) MediaType() string { return "application/problem+xml" }

func (xmlCodec[T]) Schema() (openapi.Schema, error) { return nil, nil }

func (xmlCodec[T]) Encode(v T) (io.WriterTo, error) {
	b, err := xml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(b), nil
}

// failingEncoder is an encoder whose Encode always fails.
type failingEncoder struct{}

func (failingEncoder) MediaType() string { return "application/problem+json" }

func (failingEncoder) Schema() (openapi.Schema, error) { return nil, nil }

func (failingEncoder) Encode(problem.Details) (io.WriterTo, error) {
	return nil, errors.New("the encoder failed")
}

// failingWriterTo is a WriterTo that fails once it has written part of its
// body.
type failingWriterTo struct{}

func (failingWriterTo) WriteTo(w io.Writer) (int64, error) {
	n, _ := io.WriteString(w, "<problem")
	return int64(n), errors.New("the WriterTo failed")
}

// failingWriterEncoder is an encoder whose WriterTo fails.
type failingWriterEncoder struct{}

func (failingWriterEncoder) MediaType() string { return "application/problem+xml" }

func (failingWriterEncoder) Schema() (openapi.Schema, error) { return nil, nil }

func (failingWriterEncoder) Encode(problem.Details) (io.WriterTo, error) {
	return failingWriterTo{}, nil
}

// notFound is the end user's own problem detail.
type notFound struct{ problem.Details }

// outOfCredit is RFC 9457's example, less its extension members.
var outOfCredit = problem.Details{
	Type:     "https://example.com/probs/out-of-credit",
	Title:    "You do not have enough credit.",
	Status:   http.StatusForbidden,
	Detail:   "Your current balance is 30, but that costs 50.",
	Instance: "/account/12345/msgs/abc",
}

func TestDetails_encodes_as_RFC_9457s_JSON_form(t *testing.T) {
	const want = `{
		"type": "https://example.com/probs/out-of-credit",
		"title": "You do not have enough credit.",
		"status": 403,
		"detail": "Your current balance is 30, but that costs 50.",
		"instance": "/account/12345/msgs/abc"
	}`

	t.Run("every member", func(t *testing.T) {
		b, err := json.Marshal(outOfCredit)
		require.NoError(t, err)
		require.JSONEq(t, want, string(b))
	})

	t.Run("an empty member omitted", func(t *testing.T) {
		b, err := json.Marshal(problem.Details{Title: "Not Found", Status: http.StatusNotFound})
		require.NoError(t, err)
		require.JSONEq(t, `{"title":"Not Found","status":404}`, string(b))
	})

	t.Run("a type that embeds it", func(t *testing.T) {
		b, err := json.Marshal(notFound{outOfCredit})
		require.NoError(t, err)
		require.JSONEq(t, want, string(b))
	})

	t.Run("decoded from RFC 9457's example, extension members ignored", func(t *testing.T) {
		const example = `{
			"type": "https://example.com/probs/out-of-credit",
			"title": "You do not have enough credit.",
			"status": 403,
			"detail": "Your current balance is 30, but that costs 50.",
			"instance": "/account/12345/msgs/abc",
			"balance": 30,
			"accounts": ["/account/12345", "/account/67890"]
		}`
		var d problem.Details
		require.NoError(t, json.Unmarshal([]byte(example), &d))
		require.Equal(t, outOfCredit, d)
	})
}

func TestDetails_encodes_as_RFC_9457s_XML_form(t *testing.T) {
	const want = `<problem xmlns="urn:ietf:rfc:7807">` +
		`<type>https://example.com/probs/out-of-credit</type>` +
		`<title>You do not have enough credit.</title>` +
		`<status>403</status>` +
		`<detail>Your current balance is 30, but that costs 50.</detail>` +
		`<instance>/account/12345/msgs/abc</instance>` +
		`</problem>`

	t.Run("every member", func(t *testing.T) {
		b, err := xml.Marshal(outOfCredit)
		require.NoError(t, err)
		require.Equal(t, want, string(b))
	})

	t.Run("an empty member omitted", func(t *testing.T) {
		b, err := xml.Marshal(problem.Details{Title: "Not Found", Status: http.StatusNotFound})
		require.NoError(t, err)
		require.Equal(t, `<problem xmlns="urn:ietf:rfc:7807"><title>Not Found</title><status>404</status></problem>`, string(b))
	})

	t.Run("a type that embeds it", func(t *testing.T) {
		b, err := xml.Marshal(notFound{outOfCredit})
		require.NoError(t, err)
		require.Equal(t, want, string(b))
	})

	t.Run("decoded from RFC 9457's Appendix B, extension members ignored", func(t *testing.T) {
		const example = `<?xml version="1.0" encoding="UTF-8"?>
<problem xmlns="urn:ietf:rfc:7807">
  <type>https://example.com/probs/out-of-credit</type>
  <title>You do not have enough credit.</title>
  <detail>Your current balance is 30, but that costs 50.</detail>
  <instance>/account/12345/msgs/abc</instance>
  <balance>30</balance>
  <accounts>
    <i>/account/12345</i>
    <i>/account/67890</i>
  </accounts>
  <status>403</status>
</problem>`
		var d problem.Details
		require.NoError(t, xml.Unmarshal([]byte(example), &d))
		require.Equal(t, "urn:ietf:rfc:7807", d.XMLName.Space)
		require.Equal(t, "problem", d.XMLName.Local)
		d.XMLName = xml.Name{}
		require.Equal(t, outOfCredit, d)
	})

	t.Run("an element in another namespace isn't a problem detail", func(t *testing.T) {
		var d problem.Details
		err := xml.Unmarshal([]byte(`<problem xmlns="urn:example"><title>Not Found</title></problem>`), &d)
		require.Error(t, err)
	})
}

func TestDetails_Error(t *testing.T) {
	cases := []struct {
		name string
		d    problem.Details
		want string
	}{
		{name: "its detail", d: problem.Details{Title: "Not Found", Detail: "no cart c-1"}, want: "no cart c-1"},
		{name: "its title, with no detail", d: problem.Details{Title: "Not Found"}, want: "Not Found"},
		{name: "nothing, with neither", d: problem.Details{Status: http.StatusNotFound}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.EqualError(t, tc.d, tc.want)
		})
	}
}

func TestDetails_embedded_is_an_error_that_errors_As_matches(t *testing.T) {
	err := fmt.Errorf("finding cart c-1: %w", notFound{problem.Details{Title: "Not Found", Detail: "no cart c-1"}})

	var nf notFound
	require.ErrorAs(t, err, &nf)
	require.Equal(t, "no cart c-1", nf.Detail)
	require.EqualError(t, nf, "no cart c-1")
}

func TestFromRequestError(t *testing.T) {
	cases := []struct {
		name string
		e    *openapi.RequestError
		want problem.Details
	}{
		{
			name: "a 400 about a parameter",
			e:    &openapi.RequestError{Status: http.StatusBadRequest, Param: "limit", Err: errors.New(`query parameter "limit": not an integer`)},
			want: problem.Details{Type: "about:blank", Title: "Bad Request", Status: http.StatusBadRequest, Detail: `query parameter "limit": not an integer`},
		},
		{
			name: "a 413",
			e:    &openapi.RequestError{Status: http.StatusRequestEntityTooLarge, Err: &http.MaxBytesError{Limit: 20}},
			want: problem.Details{Type: "about:blank", Title: "Request Entity Too Large", Status: http.StatusRequestEntityTooLarge, Detail: "http: request body too large"},
		},
		{
			name: "a 415",
			e:    &openapi.RequestError{Status: http.StatusUnsupportedMediaType, Err: errors.New("the request has no Content-Type, but the operation reads application/json")},
			want: problem.Details{Type: "about:blank", Title: "Unsupported Media Type", Status: http.StatusUnsupportedMediaType, Detail: "the request has no Content-Type, but the operation reads application/json"},
		},
		{
			name: "no error",
			e:    &openapi.RequestError{Status: http.StatusBadRequest},
			want: problem.Details{Type: "about:blank", Title: "Bad Request", Status: http.StatusBadRequest},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, problem.FromRequestError(tc.e))
		})
	}
}

func TestHandler(t *testing.T) {
	t.Run("the status and a problem detail, written by its encoder", func(t *testing.T) {
		rec := httptest.NewRecorder()
		problem.Handler(http.StatusNotFound, xmlProblem).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/o-1", nil))

		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, "application/problem+xml", rec.Header().Get("Content-Type"))
		want := `<problem xmlns="urn:ietf:rfc:7807"><type>about:blank</type><title>Not Found</title><status>404</status></problem>`
		require.Equal(t, want, rec.Body.String())
		require.Equal(t, strconv.Itoa(len(want)), rec.Header().Get("Content-Length"))
	})

	t.Run("a header already set kept", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Allow", "GET, DELETE")
		problem.Handler(http.StatusMethodNotAllowed, xmlProblem).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/carts/c-1", nil))

		require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		require.Equal(t, []string{"GET, DELETE"}, rec.Header().Values("Allow"))
		require.Equal(t, `<problem xmlns="urn:ietf:rfc:7807"><type>about:blank</type><title>Method Not Allowed</title><status>405</status></problem>`, rec.Body.String())
	})

	t.Run("the status alone, when its encoder fails", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Allow", "GET, DELETE")
		h := problem.Handler(http.StatusMethodNotAllowed, func() openapi.Encoder[problem.Details] { return failingEncoder{} })
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/carts/c-1", nil))

		require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		require.Empty(t, rec.Header().Get("Content-Type"))
		require.Empty(t, rec.Body.String())
		require.Equal(t, []string{"GET, DELETE"}, rec.Header().Values("Allow"))
	})

	t.Run("cut short, when its encoder's WriterTo fails", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h := problem.Handler(http.StatusNotFound, func() openapi.Encoder[problem.Details] { return failingWriterEncoder{} })
		require.PanicsWithValue(t, http.ErrAbortHandler, func() {
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/o-1", nil))
		})
		require.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("its encoder function called once", func(t *testing.T) {
		calls := 0
		h := problem.Handler(http.StatusNotFound, func() openapi.Encoder[problem.Details] {
			calls++
			return xmlProblem[problem.Details]()
		})
		for range 3 {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders/o-1", nil))
		}
		require.Equal(t, 1, calls)
	})

	t.Run("the status alone, for a status with no body", func(t *testing.T) {
		for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
			rec := httptest.NewRecorder()
			h := problem.Handler(status, func() openapi.Encoder[problem.Details] { return failingWriterEncoder{} })
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orders/o-1", nil))

			require.Equal(t, status, rec.Code)
			require.Empty(t, rec.Header().Get("Content-Type"))
			require.Empty(t, rec.Header().Get("Content-Length"))
			require.Empty(t, rec.Body.String())
		}
	})

	t.Run("a panic for a status that isn't a final response's", func(t *testing.T) {
		for _, status := range []int{0, 99, http.StatusContinue, http.StatusEarlyHints, 600} {
			want := fmt.Sprintf("problem: Handler's status %d isn't the status of a final response: 200 to 599", status)
			require.PanicsWithValue(t, want, func() {
				problem.Handler(status, xmlProblem)
			})
		}
	})

	t.Run("a panic for a nil encoder function", func(t *testing.T) {
		require.PanicsWithValue(t, "problem: Handler's encoder function is nil", func() {
			problem.Handler(http.StatusNotFound, nil)
		})
	})

	t.Run("a panic for an encoder function that returns nil", func(t *testing.T) {
		require.PanicsWithValue(t, "problem: Handler's encoder function returned nil", func() {
			problem.Handler(http.StatusNotFound, func() openapi.Encoder[problem.Details] { return nil })
		})
	})
}

// The API the tests serve.
var (
	carts = openapi.Root().Lit("carts")
	cart  = carts.Param(cartID)
	items = cart.Lit("items")

	cartID = openapi.PathParam[string]("cartId")
	limit  = openapi.QueryParam[int]("limit")
)

// item is an item a client adds to a cart.
type item struct {
	Name string `json:"name"`
}

// getCart answers GET /carts/{cartId}, with a notFound, wrapped, for the
// cart "missing".
func getCart(_ context.Context, req openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
	if id := openapi.ParamFrom(req, cartID); id == "missing" {
		return openapi.EmptyBody{}, fmt.Errorf("finding cart: %w", notFound{problem.Details{
			Type:   "https://example.com/probs/no-cart",
			Title:  "Not Found",
			Status: http.StatusNotFound,
			Detail: "no cart " + id,
		}})
	}
	return openapi.EmptyBody{}, nil
}

// addItem answers POST /carts/{cartId}/items with nothing.
func addItem(context.Context, openapi.Request[item]) (openapi.EmptyBody, error) {
	return openapi.EmptyBody{}, nil
}

// internalError is the catch-all's wrap.
func internalError(error) problem.Details {
	return problem.Details{Type: "about:blank", Title: "Internal Server Error", Status: http.StatusInternalServerError}
}

// newAPI is the API the tests serve, answering every error with a problem
// detail as xmlProblem writes it, and unmatched requests with the handlers
// it is given.
func newAPI(s *humus.Setup, notFoundHandler, methodNotAllowed http.Handler) (*openapi.API, error) {
	return openapi.NewAPI(s,
		openapi.Info{Title: "Carts", Version: "1.0.0"},
		openapi.Unmatched{NotFound: notFoundHandler, MethodNotAllowed: methodNotAllowed},
		openapi.RequestErrors(problem.FromRequestError, xmlProblem),
		openapi.CatchAll(http.StatusInternalServerError, internalError, xmlProblem),
		[]openapi.Operation{
			openapi.Get(cart, getCart, openapi.EmptyEncoder, openapi.Query(limit)),
			openapi.Delete(cart, getCart, openapi.EmptyEncoder),
			openapi.Post(items, addItem, jsoncodec.Decoder, openapi.EmptyEncoder),
		},
		openapi.Error[notFound](http.StatusNotFound, xmlProblem),
	)
}

// server is an App serving an API through httpserver, on a humustest Host.
type server struct {
	t      *testing.T
	client *http.Client
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

// serve starts an App whose only Service serves the handler build returns.
func serve(t *testing.T, build func(s *humus.Setup) (http.Handler, error)) *server {
	t.Helper()
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "localhost")
	h := humustest.NewHost(t)

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
	}, humustest.WithApp(humus.WithHost(h.Host())))
	select {
	case <-h.Listening("localhost:8443"):
	case st := <-status:
		t.Fatalf("the App exited before it listened: %v", st.Cause())
	}
	return &server{t: t, client: httpservertest.NewClient(h, ca.Pool()), p: p, status: status}
}

// serveAPI serves newAPI, with problem.Handler as its Unmatched.
func serveAPI(t *testing.T) *server {
	t.Helper()
	return serve(t, func(s *humus.Setup) (http.Handler, error) {
		return newAPI(s,
			problem.Handler(http.StatusNotFound, xmlProblem),
			problem.Handler(http.StatusMethodNotAllowed, xmlProblem),
		)
	})
}

// stop stops the App, and checks that it exited cleanly, having written
// nothing to stderr.
func (s *server) stop() {
	s.t.Helper()
	s.p.Signal(syscall.SIGTERM)
	st := <-s.status
	require.Zero(s.t, st.Code(), "cause: %v", st.Cause())
	require.Empty(s.t, s.p.Stderr())
}

// reply is a response, read in full.
type reply struct {
	status int
	header http.Header
	body   string
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
	resp, err := s.client.Do(req)
	require.NoError(s.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	return reply{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

// details decodes r's body as an XML problem detail.
func (r reply) details(t *testing.T) problem.Details {
	t.Helper()
	require.Equal(t, "application/problem+xml", r.header.Get("Content-Type"))
	var d problem.Details
	require.NoError(t, xml.Unmarshal([]byte(r.body), &d))
	d.XMLName = xml.Name{}
	return d
}

func TestHandler_answers_through_Unmatched(t *testing.T) {
	t.Run("a 404", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t)

			r := s.do(http.MethodGet, "/orders/o-1", "", "")
			require.Equal(t, http.StatusNotFound, r.status)
			require.Equal(t, problem.Details{Type: "about:blank", Title: "Not Found", Status: http.StatusNotFound}, r.details(t))
			require.Equal(t, strconv.Itoa(len(r.body)), r.header.Get("Content-Length"))

			s.stop()
		})
	})

	t.Run("a 405, with the API's Allow kept", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t)

			r := s.do(http.MethodPut, "/carts/c-1", "", "")
			require.Equal(t, http.StatusMethodNotAllowed, r.status)
			require.Equal(t, []string{"GET, DELETE"}, r.header.Values("Allow"))
			require.Equal(t, problem.Details{Type: "about:blank", Title: "Method Not Allowed", Status: http.StatusMethodNotAllowed}, r.details(t))

			s.stop()
		})
	})

	t.Run("the status alone, when its encoder fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			failing := func() openapi.Encoder[problem.Details] { return failingEncoder{} }
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return newAPI(s,
					problem.Handler(http.StatusNotFound, failing),
					problem.Handler(http.StatusMethodNotAllowed, failing),
				)
			})

			r := s.do(http.MethodGet, "/orders/o-1", "", "")
			require.Equal(t, http.StatusNotFound, r.status)
			require.Empty(t, r.header.Get("Content-Type"))
			require.Empty(t, r.body)

			r = s.do(http.MethodPut, "/carts/c-1", "", "")
			require.Equal(t, http.StatusMethodNotAllowed, r.status)
			require.Equal(t, []string{"GET, DELETE"}, r.header.Values("Allow"))
			require.Empty(t, r.header.Get("Content-Type"))
			require.Empty(t, r.body)

			s.stop()
		})
	})
}

func TestFromRequestError_answers_through_RequestErrors(t *testing.T) {
	t.Run("a 400 about a parameter", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t)

			r := s.do(http.MethodGet, "/carts/c-1?limit=ten", "", "")
			require.Equal(t, http.StatusBadRequest, r.status)
			d := r.details(t)
			require.Equal(t, "about:blank", d.Type)
			require.Equal(t, "Bad Request", d.Title)
			require.Equal(t, http.StatusBadRequest, d.Status)
			require.Contains(t, d.Detail, "limit")

			s.stop()
		})
	})

	t.Run("a 413", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				api, err := newAPI(s, http.NotFoundHandler(), http.NotFoundHandler())
				if err != nil {
					return nil, err
				}
				return http.MaxBytesHandler(api, 8), nil
			})

			r := s.do(http.MethodPost, "/carts/c-1/items", "application/json", `{"name":"a name longer than the limit"}`)
			require.Equal(t, http.StatusRequestEntityTooLarge, r.status)
			require.Equal(t, problem.Details{
				Type:   "about:blank",
				Title:  "Request Entity Too Large",
				Status: http.StatusRequestEntityTooLarge,
				Detail: "http: request body too large",
			}, r.details(t))

			s.stop()
		})
	})

	t.Run("a 415", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t)

			r := s.do(http.MethodPost, "/carts/c-1/items", "text/plain", `{"name":"tea"}`)
			require.Equal(t, http.StatusUnsupportedMediaType, r.status)
			require.Equal(t, problem.Details{
				Type:   "about:blank",
				Title:  "Unsupported Media Type",
				Status: http.StatusUnsupportedMediaType,
				Detail: `the request's Content-Type is "text/plain", but the operation reads application/json`,
			}, r.details(t))

			s.stop()
		})
	})
}

func TestDetails_embedded_is_answered_by_openapi_Error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t)

		r := s.do(http.MethodGet, "/carts/missing", "", "")
		require.Equal(t, http.StatusNotFound, r.status)
		require.Equal(t,
			`<problem xmlns="urn:ietf:rfc:7807"><type>https://example.com/probs/no-cart</type><title>Not Found</title><status>404</status><detail>no cart missing</detail></problem>`,
			r.body)

		s.stop()
	})
}

// TestPackage_takes_a_generic_encoder_uninstantiated checks, by compiling,
// that RequestErrors and Handler infer the encoder's type argument, for
// the tests' own encoder and for jsoncodec's.
func TestPackage_takes_a_generic_encoder_uninstantiated(t *testing.T) {
	require.NotNil(t, openapi.RequestErrors(problem.FromRequestError, xmlProblem))
	require.NotNil(t, problem.Handler(http.StatusNotFound, xmlProblem))
	require.NotNil(t, openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem))
	require.NotNil(t, problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem))
}
