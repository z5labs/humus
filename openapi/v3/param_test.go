// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	openapi "github.com/z5labs/humus/openapi/v3"
)

// Named types of each kind a parameter can have.
type (
	include    string
	flag       bool
	pageNumber int
	cursor     int64
	ratio      float64
)

const (
	includeItems include = "items"
	includeCount include = "count"
)

// includeParam is the query parameter of the proposal's example.
var includeParam = openapi.QueryParam[include]("include").
	Description("What the cart includes").
	Enum(includeItems, includeCount).
	Default(includeItems)

// getWith sends a GET of path with header.
func (s *server) getWith(path string, header http.Header) reply {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodGet, "https://localhost:8443"+path, nil)
	require.NoError(s.t, err)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return s.send(req)
}

// typed formats v with its type, so a test sees that a parameter decoded
// as its own type.
func typed(v any) string {
	return fmt.Sprintf("%T=%v", v, v)
}

func TestAPI_decodes_parameters(t *testing.T) {
	var (
		q      = openapi.QueryParam[string]("q")
		exact  = openapi.QueryParam[bool]("exact")
		page   = openapi.QueryParam[int]("page")
		after  = openapi.QueryParam[int64]("after")
		score  = openapi.QueryParam[float64]("score")
		strict = openapi.QueryParam[flag]("strict")
		n      = openapi.QueryParam[pageNumber]("n")
		r      = openapi.QueryParam[ratio]("ratio")
		cur    = openapi.HeaderParam[cursor]("X-Cursor")
		label  = openapi.HeaderParam[string]("X-Label").Default("none")
	)
	search := func(_ context.Context, req openapi.Request[openapi.EmptyBody]) ([]string, error) {
		return []string{
			typed(openapi.ParamFrom(req, q)),
			typed(openapi.ParamFrom(req, exact)),
			typed(openapi.ParamFrom(req, page)),
			typed(openapi.ParamFrom(req, after)),
			typed(openapi.ParamFrom(req, score)),
			typed(openapi.ParamFrom(req, includeParam)),
			typed(openapi.ParamFrom(req, strict)),
			typed(openapi.ParamFrom(req, n)),
			typed(openapi.ParamFrom(req, r)),
			typed(openapi.ParamFrom(req, cur)),
			typed(openapi.ParamFrom(req, label)),
		}, nil
	}
	ops := func() []openapi.Operation {
		return []openapi.Operation{
			openapi.Get(openapi.Root().Lit("search"), search, jsonEncoder,
				openapi.Query(q), openapi.Query(exact), openapi.Query(page), openapi.Query(after),
				openapi.Query(score), openapi.Query(includeParam), openapi.Query(strict),
				openapi.Query(n), openapi.Query(r), openapi.Header(cur), openapi.Header(label),
			),
		}
	}
	decode := func(t *testing.T, r reply) []string {
		t.Helper()
		require.Equal(t, http.StatusOK, r.status, r.body)
		var got []string
		require.NoError(t, json.Unmarshal([]byte(r.body), &got))
		return got
	}

	t.Run("each type, named types included", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, ops())

			r := s.getWith("/search?q=green+tea&exact=true&page=2&after=9000000000&score=0.5&include=count&strict=1&n=3&ratio=1.5",
				http.Header{"X-Cursor": {"42"}, "X-Label": {"sale"}})
			require.Equal(t, []string{
				"string=green tea",
				"bool=true",
				"int=2",
				"int64=9000000000",
				"float64=0.5",
				"openapi_test.include=count",
				"openapi_test.flag=true",
				"openapi_test.pageNumber=3",
				"openapi_test.ratio=1.5",
				"openapi_test.cursor=42",
				"string=sale",
			}, decode(t, r))

			s.stop()
		})
	})

	t.Run("an absent optional parameter, its Default or T's zero value", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, ops())

			r := s.get("/search")
			require.Equal(t, []string{
				"string=",
				"bool=false",
				"int=0",
				"int64=0",
				"float64=0",
				"openapi_test.include=items",
				"openapi_test.flag=false",
				"openapi_test.pageNumber=0",
				"openapi_test.ratio=0",
				"openapi_test.cursor=0",
				"string=none",
			}, decode(t, r))

			s.stop()
		})
	})

	t.Run("a repeated query parameter, a 400", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, ops())

			r := s.get("/search?page=1&page=2")
			require.Equal(t, http.StatusBadRequest, r.status)
			require.Equal(t, "application/problem+json", r.contentType)
			require.JSONEq(t, `{"status":400,"title":"Bad Request","param":"page","detail":"query parameter \"page\" appears 2 times, but has one value"}`, r.body)

			s.stop()
			require.Empty(t, records(s.tel), "a rejected request is the client's mistake")
		})
	})
}

func TestAPI_decodes_header_parameters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requestID := openapi.HeaderParam[string]("x-request-id").Required()
		s := serveAPI(t, []openapi.Operation{
			openapi.Get(carts, func(_ context.Context, req openapi.Request[openapi.EmptyBody]) (string, error) {
				return openapi.ParamFrom(req, requestID), nil
			}, jsonEncoder, openapi.Header(requestID)),
		})

		// A header's name is matched whatever its case.
		r := s.getWith("/carts", http.Header{"X-Request-Id": {"r-1"}})
		require.Equal(t, http.StatusOK, r.status)
		require.JSONEq(t, `"r-1"`, r.body)

		// HTTP reads two lines of one field as their values joined by
		// commas.
		r = s.getWith("/carts", http.Header{"X-Request-Id": {"r-1", "r-2"}})
		require.Equal(t, http.StatusOK, r.status)
		require.JSONEq(t, `"r-1, r-2"`, r.body)

		r = s.get("/carts")
		require.Equal(t, http.StatusBadRequest, r.status)
		require.JSONEq(t, `{"status":400,"title":"Bad Request","param":"x-request-id","detail":"header parameter \"x-request-id\" is required, but the request has none"}`, r.body)

		s.stop()
	})
}

func TestAPI_answers_bad_parameters_with_RequestErrors(t *testing.T) {
	orderID := openapi.PathParam[int64]("orderId").Minimum(1)
	var (
		q     = openapi.QueryParam[string]("q").Required()
		sku   = openapi.QueryParam[string]("sku").Pattern(regexp.MustCompile(`^[A-Z]{3}-[0-9]+$`))
		name  = openapi.QueryParam[string]("name").MinLength(2).MaxLength(5)
		limit = openapi.QueryParam[int]("limit").Minimum(1).Maximum(100)
		r     = openapi.QueryParam[float64]("ratio").Maximum(1)
		exact = openapi.QueryParam[bool]("exact")
		mode  = openapi.HeaderParam[string]("X-Mode").Enum("fast", "slow")
	)

	cases := []struct {
		name   string
		path   string
		header http.Header
		param  string
		detail string
	}{
		{"a missing required parameter", "/orders/7", nil, "q", `query parameter "q" is required, but the request has none`},
		{"a value not in Enum", "/orders/7?q=tea&include=all", nil, "include", `query parameter "include": "all" isn't one of "items", "count"`},
		{"a value that doesn't match Pattern", "/orders/7?q=tea&sku=abc", nil, "sku", `query parameter "sku": "abc" doesn't match the pattern ^[A-Z]{3}-[0-9]+$`},
		{"a value under MinLength", "/orders/7?q=tea&name=a", nil, "name", `query parameter "name": "a" is shorter than 2 characters`},
		{"a value over MaxLength", "/orders/7?q=tea&name=abcdef", nil, "name", `query parameter "name": "abcdef" is longer than 5 characters`},
		{"a value under Minimum", "/orders/7?q=tea&limit=0", nil, "limit", `query parameter "limit": 0 is less than the minimum, 1`},
		{"a value over Maximum", "/orders/7?q=tea&limit=101", nil, "limit", `query parameter "limit": 101 is greater than the maximum, 100`},
		{"a number over Maximum", "/orders/7?q=tea&ratio=1.5", nil, "ratio", `query parameter "ratio": 1.5 is greater than the maximum, 1`},
		{"an integer that doesn't parse", "/orders/7?q=tea&limit=ten", nil, "limit", `query parameter "limit": "ten" isn't an integer of 64 bits`},
		{"a bool that doesn't parse", "/orders/7?q=tea&exact=maybe", nil, "exact", `query parameter "exact": "maybe" isn't a bool`},
		{"a path parameter under Minimum", "/orders/0?q=tea", nil, "orderId", `path parameter "orderId": 0 is less than the minimum, 1`},
		{"a header parameter not in Enum", "/orders/7?q=tea", http.Header{"X-Mode": {"turbo"}}, "X-Mode", `header parameter "X-Mode": "turbo" isn't one of "fast", "slow"`},
		{"a repeated query parameter", "/orders/7?q=tea&q=coffee", nil, "q", `query parameter "q" appears 2 times, but has one value`},
	}

	synctest.Test(t, func(t *testing.T) {
		var called atomic.Int32
		s := serveAPI(t, []openapi.Operation{
			openapi.Get(openapi.Root().Lit("orders").Param(orderID), func(_ context.Context, req openapi.Request[openapi.EmptyBody]) (int64, error) {
				called.Add(1)
				return openapi.ParamFrom(req, orderID), nil
			}, jsonEncoder,
				openapi.Query(q), openapi.Query(includeParam), openapi.Query(sku), openapi.Query(name),
				openapi.Query(limit), openapi.Query(r), openapi.Query(exact), openapi.Header(mode),
			),
		})

		for _, tc := range cases {
			got := s.getWith(tc.path, tc.header)
			require.Equal(t, http.StatusBadRequest, got.status, tc.name)
			require.Equal(t, "application/problem+json", got.contentType, tc.name)
			var p problem
			require.NoError(t, json.Unmarshal([]byte(got.body), &p), tc.name)
			require.Equal(t, problem{Status: http.StatusBadRequest, Title: "Bad Request", Param: tc.param, Detail: tc.detail}, p, tc.name)
		}
		require.Zero(t, called.Load(), "the handler isn't called for a rejected request")

		got := s.getWith("/orders/7?q=tea&include=count&sku=ABC-1&name=ab&limit=100&ratio=1&exact=false", http.Header{"X-Mode": {"slow"}})
		require.Equal(t, http.StatusOK, got.status, got.body)
		require.Equal(t, "7", got.body)
		require.Equal(t, int32(1), called.Load())

		s.stop()
		require.Empty(t, records(s.tel), "a rejected request is the client's mistake")
	})
}

func TestParameter_methods_return_a_copy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := openapi.QueryParam[int]("limit")
		values := []int{1, 2}
		small := base.Maximum(10).Enum(values...)
		large := base.Maximum(100)
		values[0] = 50 // Enum keeps its own copy of its values.

		read := func(p openapi.QueryParameter[int]) func(context.Context, openapi.Request[openapi.EmptyBody]) (int, error) {
			return func(_ context.Context, req openapi.Request[openapi.EmptyBody]) (int, error) {
				return openapi.ParamFrom(req, p), nil
			}
		}
		s := serveAPI(t, []openapi.Operation{
			openapi.Get(openapi.Root().Lit("small"), read(small), jsonEncoder, openapi.Query(small)),
			openapi.Get(openapi.Root().Lit("large"), read(large), jsonEncoder, openapi.Query(large)),
			openapi.Get(openapi.Root().Lit("base"), read(base), jsonEncoder, openapi.Query(base)),
		})

		r := s.get("/small?limit=50")
		require.Equal(t, http.StatusBadRequest, r.status)
		require.Contains(t, r.body, `\"limit\": 50 isn't one of 1, 2`)

		r = s.get("/small?limit=2")
		require.Equal(t, http.StatusOK, r.status)

		r = s.get("/large?limit=50")
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "50", r.body)

		r = s.get("/base?limit=5000")
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "5000", r.body)

		s.stop()
	})
}

func TestParamFrom_panics_for_a_query_or_header_parameter_the_operation_doesnt_read(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mode := openapi.HeaderParam[string]("X-Mode")
		panics := make(chan any, 2)
		recovering := func(read func(openapi.Request[openapi.EmptyBody])) func(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
			return func(_ context.Context, req openapi.Request[openapi.EmptyBody]) (cartBody, error) {
				defer func() { panics <- recover() }()
				read(req)
				return cartBody{}, nil
			}
		}
		s := serveAPI(t, []openapi.Operation{
			openapi.Get(cart, recovering(func(req openapi.Request[openapi.EmptyBody]) { openapi.ParamFrom(req, includeParam) }), jsonEncoder),
			openapi.Get(carts, recovering(func(req openapi.Request[openapi.EmptyBody]) { openapi.ParamFrom(req, mode) }), jsonEncoder,
				openapi.Query(includeParam),
			),
		})

		s.get("/carts/c-1")
		require.Equal(t, `openapi: ParamFrom: operation GET /carts/{cartId} doesn't read query parameter "include"`, <-panics)
		s.get("/carts")
		require.Equal(t, `openapi: ParamFrom: operation GET /carts doesn't read header parameter "X-Mode"`, <-panics)

		s.p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-s.status).Code())
	})
}
