// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jsoncodec_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"math"
	"net/http"
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
)

// The paths of the API the tests serve.
var (
	items    = openapi.Root().Lit("items")
	readings = openapi.Root().Lit("readings")
	docPath  = openapi.Root().Lit("openapi.json")
)

// item is an item, as a client adds it and the API returns it.
type item struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

// reading holds a float, which JSON can't encode when it is NaN.
type reading struct {
	Value float64 `json:"value"`
}

// problem is the tests' error body.
type problem struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

func (p problem) Error() string { return p.Title }

// notFound is a problem addItem returns.
type notFound struct{ problem }

// addItem answers POST /items with the item it was given, or a notFound
// for an item named "missing".
func addItem(_ context.Context, req openapi.Request[item]) (item, error) {
	if req.Body().Name == "missing" {
		return item{}, notFound{problem{Status: http.StatusNotFound, Title: "Not Found", Detail: "no such item"}}
	}
	return req.Body(), nil
}

// getReading answers GET /readings with a reading JSON can't encode.
func getReading(context.Context, openapi.Request[openapi.EmptyBody]) (reading, error) {
	return reading{Value: math.NaN()}, nil
}

// rejected wraps a RequestError as a problem.
func rejected(e *openapi.RequestError) problem {
	return problem{Status: e.Status, Title: http.StatusText(e.Status), Detail: e.Err.Error()}
}

// internalError wraps an error the catch-all answers, hiding its text.
func internalError(error) problem {
	return problem{Status: http.StatusInternalServerError, Title: "Internal Server Error"}
}

// newAPI builds an API of ops, every body and answer through jsoncodec.
func newAPI(s *humus.Setup, ops ...openapi.Operation) (*openapi.API, error) {
	return openapi.NewAPI(s,
		openapi.Info{Title: "Items", Version: "1.0.0"},
		openapi.Unmatched{NotFound: http.NotFoundHandler(), MethodNotAllowed: http.NotFoundHandler()},
		openapi.RequestErrors(rejected, jsoncodec.Problem),
		openapi.CatchAll(http.StatusInternalServerError, internalError, jsoncodec.Problem),
		ops,
		openapi.Error[notFound](http.StatusNotFound, jsoncodec.Problem),
	)
}

// ops are the operations most tests serve.
func ops() []openapi.Operation {
	return []openapi.Operation{
		openapi.Post(items, addItem, jsoncodec.Decoder, jsoncodec.Encoder, openapi.Status(http.StatusCreated)),
		openapi.Get(readings, getReading, jsoncodec.Encoder),
		openapi.Document(docPath),
	}
}

// server is an App serving an API through httpserver, on a humustest Host.
type server struct {
	t      *testing.T
	client *http.Client
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

// start starts an App whose only Service serves the handler build returns.
// It returns the App's exit status if the App exited before it listened.
func start(t *testing.T, build func(s *humus.Setup) (http.Handler, error)) (*server, *bedrock.ExitStatus) {
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
		return nil, &st
	}
	return &server{t: t, client: httpservertest.NewClient(h, ca.Pool()), p: p, status: status}, nil
}

// serve serves the handler build returns.
func serve(t *testing.T, build func(s *humus.Setup) (http.Handler, error)) *server {
	t.Helper()
	s, st := start(t, build)
	if st != nil {
		t.Fatalf("the App exited before it listened: %v", st.Cause())
	}
	return s
}

// serveAPI serves the API of ops().
func serveAPI(t *testing.T) *server {
	t.Helper()
	return serve(t, func(s *humus.Setup) (http.Handler, error) { return newAPI(s, ops()...) })
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

// post posts body to /items as application/json.
func (s *server) post(body string) reply {
	s.t.Helper()
	return s.do(http.MethodPost, items.String(), "application/json", body)
}

func TestDecoder_and_Encoder(t *testing.T) {
	t.Run("a body decoded, and a response encoded with its Content-Length", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t)

			r := s.post(`{"name":"tea","quantity":2}`)
			require.Equal(t, http.StatusCreated, r.status)
			require.Equal(t, "application/json", r.header.Get("Content-Type"))
			require.JSONEq(t, `{"name":"tea","quantity":2}`, r.body)
			require.Equal(t, strconv.Itoa(len(r.body)), r.header.Get("Content-Length"))

			s.stop()
		})
	})

	t.Run("whitespace around the value", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t)

			r := s.post(" \n{\"name\":\"tea\",\"quantity\":2}\n\t ")
			require.Equal(t, http.StatusCreated, r.status)
			require.JSONEq(t, `{"name":"tea","quantity":2}`, r.body)

			s.stop()
		})
	})

	t.Run("an unknown field ignored", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t)

			r := s.post(`{"name":"tea","quantity":2,"colour":"green"}`)
			require.Equal(t, http.StatusCreated, r.status)
			require.JSONEq(t, `{"name":"tea","quantity":2}`, r.body)

			s.stop()
		})
	})
}

func TestDecoder_answers_a_body_it_cant_decode_with_a_400(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		detail string
	}{
		{name: "an empty body", body: "", detail: "the body is empty"},
		{name: "a body of whitespace", body: " \n ", detail: "the body is empty"},
		{name: "malformed JSON", body: `{"name":`, detail: "unexpected EOF"},
		{name: "an invalid character", body: `{"name" "tea"}`, detail: "invalid character '\"' after object key"},
		{name: "a wrong type", body: `{"name":"tea","quantity":"two"}`, detail: "json: cannot unmarshal string into Go struct field item.quantity of type int"},
		{name: "a second value", body: `{"name":"tea"} {"name":"milk"}`, detail: "data after the JSON value"},
		{name: "a stray closing brace", body: `{"name":"tea"}}`, detail: "data after the JSON value"},
		{name: "a stray character", body: `{"name":"tea"}x`, detail: "data after the JSON value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serveAPI(t)

				r := s.post(tc.body)
				require.Equal(t, http.StatusBadRequest, r.status)
				require.Equal(t, "application/problem+json", r.header.Get("Content-Type"))
				var p problem
				require.NoError(t, json.Unmarshal([]byte(r.body), &p))
				require.Equal(t, problem{Status: http.StatusBadRequest, Title: "Bad Request", Detail: tc.detail}, p)

				s.stop()
			})
		})
	}
}

func TestDecoder_answers_a_body_over_MaxBytesHandlers_limit_with_a_413(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "within the value", body: `{"name":"a name longer than the limit"}`},
		{name: "after the value", body: `{"name":"tea"}` + strings.Repeat(" ", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serve(t, func(s *humus.Setup) (http.Handler, error) {
					api, err := newAPI(s, ops()...)
					if err != nil {
						return nil, err
					}
					return http.MaxBytesHandler(api, 20), nil
				})

				r := s.post(tc.body)
				require.Equal(t, http.StatusRequestEntityTooLarge, r.status)
				require.Equal(t, "application/problem+json", r.header.Get("Content-Type"))
				require.JSONEq(t, `{"status":413,"title":"Request Entity Too Large","detail":"http: request body too large"}`, r.body)

				s.stop()
			})
		})
	}
}

func TestProblem_writes_application_problem_json(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t)

		r := s.post(`{"name":"missing"}`)
		require.Equal(t, http.StatusNotFound, r.status)
		require.Equal(t, "application/problem+json", r.header.Get("Content-Type"))
		require.JSONEq(t, `{"status":404,"title":"Not Found","detail":"no such item"}`, r.body)
		require.Equal(t, strconv.Itoa(len(r.body)), r.header.Get("Content-Length"))

		s.stop()
	})
}

func TestEncoder_leaves_a_value_it_cant_encode_to_the_catch_all(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t)

		r := s.do(http.MethodGet, readings.String(), "", "")
		require.Equal(t, http.StatusInternalServerError, r.status)
		require.Equal(t, "application/problem+json", r.header.Get("Content-Type"))
		require.JSONEq(t, `{"status":500,"title":"Internal Server Error"}`, r.body)

		s.stop()
	})
}

// handlers whose bodies JSON can't decode into or encode.
func chanResponse(context.Context, openapi.Request[openapi.EmptyBody]) (chan int, error) {
	return nil, nil
}

func funcResponse(context.Context, openapi.Request[openapi.EmptyBody]) (func(), error) {
	return nil, nil
}

func readerBody(context.Context, openapi.Request[io.Reader]) (item, error) {
	return item{}, nil
}

func anyResponse(context.Context, openapi.Request[openapi.EmptyBody]) (any, error) {
	return nil, nil
}

func complexResponse(context.Context, openapi.Request[openapi.EmptyBody]) (complex128, error) {
	return 0, nil
}

func chanPointerBody(context.Context, openapi.Request[*chan int]) (item, error) {
	return item{}, nil
}

func TestSchema_rejects_a_type_JSON_cant_handle_so_NewAPI_fails(t *testing.T) {
	cases := []struct {
		name string
		op   openapi.Operation
		want string
	}{
		{
			name: "a channel",
			op:   openapi.Get(items, chanResponse, jsoncodec.Encoder),
			want: "GET /items: response: its encoder's Schema failed: jsoncodec: JSON can't decode into or encode the type chan int",
		},
		{
			name: "a function",
			op:   openapi.Get(items, funcResponse, jsoncodec.Encoder),
			want: "GET /items: response: its encoder's Schema failed: jsoncodec: JSON can't decode into or encode the type func()",
		},
		{
			name: "an io.Reader",
			op:   openapi.Post(items, readerBody, jsoncodec.Decoder, jsoncodec.Encoder),
			want: "POST /items: request body: its decoder's Schema failed: jsoncodec: JSON can't decode into or encode the type io.Reader",
		},
		{
			name: "an empty interface",
			op:   openapi.Get(items, anyResponse, jsoncodec.Encoder),
			want: "GET /items: response: its encoder's Schema failed: jsoncodec: JSON can't decode into or encode the type interface {}",
		},
		{
			name: "a complex number",
			op:   openapi.Get(items, complexResponse, jsoncodec.Encoder),
			want: "GET /items: response: its encoder's Schema failed: jsoncodec: JSON can't decode into or encode the type complex128",
		},
		{
			name: "a pointer to a channel",
			op:   openapi.Post(items, chanPointerBody, jsoncodec.Decoder, jsoncodec.Encoder),
			want: "POST /items: request body: its decoder's Schema failed: jsoncodec: JSON can't decode into or encode the type *chan int",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, st := start(t, func(s *humus.Setup) (http.Handler, error) { return newAPI(s, tc.op) })
				require.Nil(t, s, "the App served an API NewAPI should have refused")
				require.NotZero(t, st.Code())
				require.ErrorContains(t, st.Cause(), tc.want)
			})
		})
	}
}

func TestSchema_lists_application_json_with_the_types_schema(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveAPI(t)

		r := s.do(http.MethodGet, docPath.String(), "", "")
		require.Equal(t, http.StatusOK, r.status)

		var doc struct {
			Paths map[string]map[string]struct {
				RequestBody struct {
					Content map[string]struct {
						Schema json.RawMessage `json:"schema"`
					} `json:"content"`
				} `json:"requestBody"`
				Responses map[string]struct {
					Content map[string]struct {
						Schema json.RawMessage `json:"schema"`
					} `json:"content"`
				} `json:"responses"`
			} `json:"paths"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.body), &doc))

		want, err := openapi.SchemaOf[item]("json")
		require.NoError(t, err)
		post := doc.Paths["/items"]["post"]
		require.Len(t, post.RequestBody.Content, 1)
		require.JSONEq(t, string(want), string(post.RequestBody.Content["application/json"].Schema))
		require.Len(t, post.Responses["201"].Content, 1)
		require.JSONEq(t, string(want), string(post.Responses["201"].Content["application/json"].Schema))

		wantProblem, err := openapi.SchemaOf[notFound]("json")
		require.NoError(t, err)
		require.Len(t, post.Responses["404"].Content, 1)
		require.JSONEq(t, string(wantProblem), string(post.Responses["404"].Content["application/problem+json"].Schema))

		s.stop()
	})
}
