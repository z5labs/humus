// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	openapi "github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/telemetry"
)

// setup is a Setup for calling NewAPI outside an App.
func setup() *humus.Setup {
	return &humus.Setup{Telemetry: telemetry.Noop()}
}

// apiArgs is every argument of NewAPI, so a case changes only the one it is
// about.
type apiArgs struct {
	info          openapi.Info
	unmatched     openapi.Unmatched
	requestErrors openapi.RequestErrorAnswer
	catchAll      openapi.Fallback
	ops           []openapi.Operation
	opts          []openapi.APIOption
}

// validArgs returns arguments NewAPI accepts.
func validArgs() apiArgs {
	return apiArgs{
		info:          openapi.Info{Title: "Carts", Version: "1.0.0"},
		unmatched:     unmatched,
		requestErrors: openapi.RequestErrors(rejected, problemEncoder),
		catchAll:      openapi.CatchAll(http.StatusInternalServerError, internalError, problemEncoder),
		ops:           []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder)},
	}
}

func (a apiArgs) newAPI() (*openapi.API, error) {
	return openapi.NewAPI(setup(), a.info, a.unmatched, a.requestErrors, a.catchAll, a.ops, a.opts...)
}

// noBody is a handler of a GET or DELETE that returns a cart.
func noBody(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
	return cartBody{}, nil
}

// nothingBack is a handler that writes no body.
func nothingBack(context.Context, openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
	return openapi.EmptyBody{}, nil
}

// chanResponse is a handler whose response JSON can't encode, so jsonEncoder's
// Schema rejects it.
func chanResponse(context.Context, openapi.Request[openapi.EmptyBody]) (chan int, error) {
	return nil, nil
}

func TestNewAPI_reports_every_mistake(t *testing.T) {
	cases := []struct {
		name   string
		change func(a *apiArgs)
		want   []string
	}{
		{
			name:   "a missing Title",
			change: func(a *apiArgs) { a.info.Title = "" },
			want:   []string{"Info.Title is empty"},
		},
		{
			name:   "a missing Version",
			change: func(a *apiArgs) { a.info.Version = "" },
			want:   []string{"Info.Version is empty"},
		},
		{
			name:   "a nil Unmatched.NotFound",
			change: func(a *apiArgs) { a.unmatched.NotFound = nil },
			want:   []string{"Unmatched.NotFound is nil"},
		},
		{
			name:   "a nil Unmatched.MethodNotAllowed",
			change: func(a *apiArgs) { a.unmatched.MethodNotAllowed = nil },
			want:   []string{"Unmatched.MethodNotAllowed is nil"},
		},
		{
			name:   "a nil RequestErrors",
			change: func(a *apiArgs) { a.requestErrors = nil },
			want:   []string{"requestErrors is nil"},
		},
		{
			name:   "a nil catch-all",
			change: func(a *apiArgs) { a.catchAll = nil },
			want:   []string{"catchAll is nil"},
		},
		{
			name: "a nil decoder function",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Post(items, addItem, nil, jsonEncoder)}
			},
			want: []string{"POST /carts/{cartId}/items: request body: its decoder function is nil"},
		},
		{
			name: "a nil encoder function",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, nil)}
			},
			want: []string{"GET /carts/{cartId}: response: its encoder function is nil"},
		},
		{
			name: "an encoder function that returns nil",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, nilEncoder)}
			},
			want: []string{"GET /carts/{cartId}: response: its encoder function returned nil"},
		},
		{
			name: "a decoder function that returns nil",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Post(items, addItem, func() openapi.Decoder[item] { return nil }, jsonEncoder)}
			},
			want: []string{"POST /carts/{cartId}/items: request body: its decoder function returned nil"},
		},
		{
			name: "a nil codec function of an Error, a catch-all and RequestErrors",
			change: func(a *apiArgs) {
				a.requestErrors = openapi.RequestErrors(rejected, nilEncoder)
				a.catchAll = openapi.CatchAll[problem](http.StatusInternalServerError, internalError, nil)
				a.opts = []openapi.APIOption{openapi.Error[notFound](http.StatusNotFound, nil)}
			},
			want: []string{
				"requestErrors: its encoder function returned nil",
				"catchAll: its encoder function is nil",
				"Error[openapi_test.notFound]: its encoder function is nil",
			},
		},
		{
			name: "an error from a codec's Schema",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart, chanResponse, jsonEncoder)}
			},
			want: []string{"GET /carts/{cartId}: response: its encoder's Schema failed: JSON can't encode a chan int"},
		},
		{
			name:   "a zero Operation",
			change: func(a *apiArgs) { a.ops = append(a.ops, openapi.Operation{}) },
			want:   []string{"ops[1] is the zero Operation"},
		},
		{
			name:   "a nil option of NewAPI",
			change: func(a *apiArgs) { a.opts = []openapi.APIOption{nil} },
			want:   []string{"opts[0] is nil"},
		},
		{
			name: "a nil option of an operation",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder, openapi.Status(http.StatusOK), nil)}
			},
			want: []string{"GET /carts/{cartId}: its option 1 is nil"},
		},
		{
			name: "a nil handler",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get[cartBody](cart, nil, jsonEncoder)}
			},
			want: []string{"GET /carts/{cartId}: its handler is nil"},
		},
		{
			name: "a nil wrap",
			change: func(a *apiArgs) {
				a.requestErrors = openapi.RequestErrors[problem](nil, problemEncoder)
				a.catchAll = openapi.CatchAll[problem](http.StatusInternalServerError, nil, problemEncoder)
			},
			want: []string{"requestErrors: its wrap function is nil", "catchAll: its wrap function is nil"},
		},
		{
			name: "an empty literal",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(openapi.Root().Lit(""), noBody, jsonEncoder)}
			},
			want: []string{"path /: a literal is empty"},
		},
		{
			name: "a dot segment",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{
					openapi.Get(openapi.Root().Lit("."), noBody, jsonEncoder),
					openapi.Get(carts.Lit(".."), noBody, jsonEncoder),
				}
			},
			want: []string{`path /.: literal "." is a dot segment`, `path /carts/..: literal ".." is a dot segment`},
		},
		{
			name: "a literal with a reserved character",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{
					openapi.Get(openapi.Root().Lit("a/b"), noBody, jsonEncoder),
					openapi.Get(openapi.Root().Lit("a b"), noBody, jsonEncoder),
					openapi.Get(openapi.Root().Lit("{id}"), noBody, jsonEncoder),
					openapi.Get(openapi.Root().Lit("café"), noBody, jsonEncoder),
				}
			},
			want: []string{
				`path /a/b: literal "a/b" holds '/'`,
				`path /a b: literal "a b" holds ' '`,
				`path /{id}: literal "{id}" holds '{'`,
				`path /café: literal "café" holds 'é'`,
			},
		},
		{
			name: "a parameter name that isn't an ASCII Go identifier",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{
					openapi.Get(openapi.Root().Lit("a").Param(openapi.PathParam[string]("cart-id")), noBody, jsonEncoder),
					openapi.Get(openapi.Root().Lit("b").Param(openapi.PathParam[string]("1st")), noBody, jsonEncoder),
					openapi.Get(openapi.Root().Lit("c").Param(openapi.PathParameter[string]{}), noBody, jsonEncoder),
				}
			},
			want: []string{
				`path /a/{cart-id}: path parameter "cart-id" isn't an ASCII Go identifier`,
				`path /b/{1st}: path parameter "1st" isn't an ASCII Go identifier`,
				`path /c/{}: path parameter "" isn't an ASCII Go identifier`,
			},
		},
		{
			name: "a parameter twice in one path",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart.Lit("copies").Param(cartID), noBody, jsonEncoder)}
			},
			want: []string{`path /carts/{cartId}/copies/{cartId}: path parameter "cartId" appears more than once`},
		},
		{
			name: "two operations at one method and path",
			change: func(a *apiArgs) {
				a.ops = append(a.ops, openapi.Get(carts.Param(cartID), noBody, jsonEncoder))
			},
			want: []string{"two operations are at GET /carts/{cartId}"},
		},
		{
			name: "two paths that differ only in their parameters' names",
			change: func(a *apiArgs) {
				a.ops = append(a.ops, openapi.Delete(carts.Param(openapi.PathParam[string]("id")), nothingBack, openapi.EmptyEncoder))
			},
			want: []string{"paths /carts/{cartId} and /carts/{id} differ only in their parameters' names"},
		},
		{
			name: "two of the API's Errors at one status",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{
					openapi.Error[notFound](http.StatusNotFound, problemEncoder),
					openapi.Error[gone](http.StatusNotFound, problemEncoder),
				}
			},
			want: []string{"Error[openapi_test.notFound] and Error[openapi_test.gone] both answer with 404"},
		},
		{
			name: "two of an operation's Errors at one status",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.Error[notFound](http.StatusNotFound, problemEncoder),
					openapi.Error[gone](http.StatusNotFound, problemEncoder),
				)}
			},
			want: []string{"GET /carts/{cartId}: Error[openapi_test.notFound] and Error[openapi_test.gone] both answer with 404"},
		},
		{
			name: "an operation's Error at an Error of the API's status",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Error[notFound](http.StatusNotFound, problemEncoder)}
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.Error[gone](http.StatusNotFound, problemEncoder),
				)}
			},
			want: []string{"GET /carts/{cartId}: Error[openapi_test.gone] and Error[openapi_test.notFound] both answer with 404"},
		},
		{
			name: "an API Error at the catch-all's status",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Error[notFound](http.StatusInternalServerError, problemEncoder)}
			},
			want: []string{"the catch-all and Error[openapi_test.notFound] both answer with 500"},
		},
		{
			name: "an operation's catch-all at an API Error's status",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Error[notFound](http.StatusNotFound, problemEncoder)}
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.CatchAll(http.StatusNotFound, internalError, problemEncoder),
				)}
			},
			want: []string{"GET /carts/{cartId}: the catch-all and Error[openapi_test.notFound] both answer with 404"},
		},
		{
			name: "an Error at RequestErrors' 400 on an operation that reads a parameter",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Error[notFound](http.StatusBadRequest, problemEncoder)}
			},
			want: []string{"GET /carts/{cartId}: RequestErrors and Error[openapi_test.notFound] both answer with 400"},
		},
		{
			name: "an Error at RequestErrors' 415 on an operation that reads a body",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Post(carts, addItem, jsonDecoder, jsonEncoder,
					openapi.Error[notFound](http.StatusUnsupportedMediaType, problemEncoder),
				)}
			},
			want: []string{"POST /carts: RequestErrors and Error[openapi_test.notFound] both answer with 415"},
		},
		{
			name: "a catch-all at RequestErrors' 413 on an operation that reads a body",
			change: func(a *apiArgs) {
				a.catchAll = openapi.CatchAll(http.StatusRequestEntityTooLarge, internalError, problemEncoder)
				a.ops = []openapi.Operation{openapi.Post(carts, addItem, jsonDecoder, jsonEncoder)}
			},
			want: []string{"POST /carts: RequestErrors and the catch-all both answer with 413"},
		},
		{
			name: "Status(204) with an encoder whose media type isn't empty",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder, openapi.Status(http.StatusNoContent))}
			},
			want: []string{`GET /carts/{cartId}: Status(204) with an encoder whose media type is "application/json", not ""`},
		},
		{
			name: "a status that isn't an HTTP status",
			change: func(a *apiArgs) {
				a.catchAll = openapi.CatchAll(0, internalError, problemEncoder)
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.Status(1000),
					openapi.Error[notFound](99, problemEncoder),
				)}
			},
			want: []string{
				"catchAll: its status 0 isn't an HTTP status",
				"GET /carts/{cartId}: Error[openapi_test.notFound]: its status 99 isn't an HTTP status",
				"GET /carts/{cartId}: Status(1000) isn't an HTTP status",
			},
		},
		{
			name: "Pattern, MinLength and MaxLength on a type that isn't a string",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Query(openapi.QueryParam[int]("page").Pattern(regexp.MustCompile(`^[0-9]+$`)).MinLength(1).MaxLength(3)),
				)}
			},
			want: []string{
				`GET /carts: query parameter "page": Pattern needs a string type, but its type is int`,
				`GET /carts: query parameter "page": MinLength needs a string type, but its type is int`,
				`GET /carts: query parameter "page": MaxLength needs a string type, but its type is int`,
			},
		},
		{
			name: "Minimum and Maximum on a type that isn't a number",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Header(openapi.HeaderParam[flag]("X-Strict").Minimum(false).Maximum(true)),
				)}
			},
			want: []string{
				`GET /carts: header parameter "X-Strict": Minimum needs a number type, but its type is openapi_test.flag`,
				`GET /carts: header parameter "X-Strict": Maximum needs a number type, but its type is openapi_test.flag`,
			},
		},
		{
			name: "a constraint on a path parameter's type that doesn't fit it",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts.Param(openapi.PathParam[include]("include").Minimum("a")), noBody, jsonEncoder)}
			},
			want: []string{`path /carts/{include}: path parameter "include": Minimum needs a number type, but its type is openapi_test.include`},
		},
		{
			name: "a nil Pattern",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Query(openapi.QueryParam[string]("sku").Pattern(nil)),
				)}
			},
			want: []string{`GET /carts: query parameter "sku": its Pattern is nil`},
		},
		{
			name: "a Default that breaks Enum",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Query(includeParam.Default("all")),
				)}
			},
			want: []string{`GET /carts: query parameter "include": its Default: "all" isn't one of "items", "count"`},
		},
		{
			name: "a Default that breaks Pattern",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Header(openapi.HeaderParam[string]("X-Sku").Pattern(regexp.MustCompile(`^[A-Z]+$`)).Default("abc")),
				)}
			},
			want: []string{`GET /carts: header parameter "X-Sku": its Default: "abc" doesn't match the pattern ^[A-Z]+$`},
		},
		{
			name: "an Example that breaks MaxLength",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Query(openapi.QueryParam[string]("name").MaxLength(3).Example("abcd")),
				)}
			},
			want: []string{`GET /carts: query parameter "name": its Example: "abcd" is longer than 3 characters`},
		},
		{
			name: "an Example and a Default that break the bounds",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Query(openapi.QueryParam[int]("limit").Minimum(1).Maximum(100).Example(0).Default(101)),
				)}
			},
			want: []string{
				`GET /carts: query parameter "limit": its Default: 101 is greater than the maximum, 100`,
				`GET /carts: query parameter "limit": its Example: 0 is less than the minimum, 1`,
			},
		},
		{
			name: "an Example of a path parameter that breaks MinLength",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts.Param(openapi.PathParam[string]("cartId").MinLength(3).Example("c1")), noBody, jsonEncoder)}
			},
			want: []string{`path /carts/{cartId}: path parameter "cartId": its Example: "c1" is shorter than 3 characters`},
		},
		{
			name: "a parameter given twice to one operation",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Query(includeParam),
					openapi.Query(openapi.QueryParam[int]("include")),
					openapi.Header(openapi.HeaderParam[string]("X-Mode")),
					openapi.Header(openapi.HeaderParam[string]("x-mode")),
				)}
			},
			want: []string{
				`GET /carts: query parameter "include" is given twice`,
				`GET /carts: header parameter "x-mode" is given twice`,
			},
		},
		{
			name: "a zero parameter",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{
					openapi.Get(carts, noBody, jsonEncoder,
						openapi.Query(openapi.QueryParameter[string]{}),
						openapi.Header(openapi.HeaderParameter[int]{}),
					),
					openapi.Get(carts.Param(openapi.PathParameter[string]{}), noBody, jsonEncoder),
				}
			},
			want: []string{
				"GET /carts: a QueryParameter has no name: is it the zero QueryParameter?",
				"GET /carts: a HeaderParameter has no name: is it the zero HeaderParameter?",
				`path /carts/{}: path parameter "" isn't an ASCII Go identifier`,
			},
		},
		{
			name: "two mistakes, reported together",
			change: func(a *apiArgs) {
				a.info.Title = ""
				a.ops = append(a.ops, openapi.Get(openapi.Root().Lit(".."), noBody, nil))
			},
			want: []string{
				"Info.Title is empty",
				`path /..: literal ".." is a dot segment`,
				"GET /..: response: its encoder function is nil",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := validArgs()
			tc.change(&a)
			api, err := a.newAPI()
			require.Nil(t, api)
			require.Error(t, err)
			msg := err.Error()
			require.True(t, strings.HasPrefix(msg, "openapi: NewAPI: "), msg)
			for _, want := range tc.want {
				require.Contains(t, msg, want)
			}
			lines := strings.Split(strings.TrimPrefix(msg, "openapi: NewAPI: "), "\n")
			require.Len(t, lines, len(tc.want), "one line per mistake: %s", msg)
		})
	}
}

func TestNewAPI_accepts(t *testing.T) {
	cases := []struct {
		name   string
		change func(a *apiArgs)
	}{
		{
			name: "an Error at 400, 413 and 415 on an operation that reads no parameter and no body",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Error[notFound](http.StatusBadRequest, problemEncoder),
					openapi.Error[gone](http.StatusUnsupportedMediaType, problemEncoder),
				)}
			},
		},
		{
			name: "an Error at 413 and 415 on an operation that reads a parameter and no body",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{
					openapi.Error[notFound](http.StatusRequestEntityTooLarge, problemEncoder),
					openapi.Error[gone](http.StatusUnsupportedMediaType, problemEncoder),
				}
			},
		},
		{
			name: "an operation's Error replacing the API's at the status of another of the API's",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{
					openapi.Error[notFound](http.StatusNotFound, problemEncoder),
					openapi.Error[gone](http.StatusGone, problemEncoder),
				}
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.Error[gone](http.StatusConflict, problemEncoder),
				)}
			},
		},
		{
			name: "one path at two methods, and paths that differ in a literal",
			change: func(a *apiArgs) {
				a.ops = append(a.ops,
					openapi.Delete(cart, nothingBack, openapi.EmptyEncoder),
					openapi.Get(carts.Lit("recent"), noBody, jsonEncoder),
				)
			},
		},
		{
			name: "one name at a path, a query and a header parameter, and a parameter shared by two operations",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{
					openapi.Get(cart, getCart, jsonEncoder,
						openapi.Query(openapi.QueryParam[string]("cartId")),
						openapi.Header(openapi.HeaderParam[string]("cartId")),
						openapi.Query(includeParam),
					),
					openapi.Get(carts, noBody, jsonEncoder, openapi.Query(includeParam)),
				}
			},
		},
		{
			name: "a Default and an Example that pass every constraint",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(carts, noBody, jsonEncoder,
					openapi.Query(openapi.QueryParam[string]("sku").Pattern(regexp.MustCompile(`^[A-Z]+$`)).MinLength(2).MaxLength(4).Default("AB").Example("ABCD")),
					openapi.Query(openapi.QueryParam[float64]("ratio").Minimum(0).Maximum(1).Default(0.5).Example(1)),
				)}
			},
		},
		{
			name: "no operations",
			change: func(a *apiArgs) {
				a.ops = nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := validArgs()
			tc.change(&a)
			api, err := a.newAPI()
			require.NoError(t, err)
			require.NotNil(t, api)
		})
	}
}

func TestNewAPI_calls_each_codec_function_and_Schema_once(t *testing.T) {
	var calls, schemas int
	enc := func() openapi.Encoder[cartBody] {
		calls++
		return countingEncoder[cartBody]{Encoder: jsonEncoder[cartBody](), schemas: &schemas}
	}
	a := validArgs()
	a.ops = []openapi.Operation{openapi.Get(cart, getCart, enc)}
	_, err := a.newAPI()
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, schemas)
}

// countingEncoder counts the calls to its Schema.
type countingEncoder[T any] struct {
	openapi.Encoder[T]
	schemas *int
}

func (e countingEncoder[T]) Schema() (openapi.Schema, error) {
	*e.schemas++
	return e.Encoder.Schema()
}

func TestPath(t *testing.T) {
	t.Run("String writes the OpenAPI path", func(t *testing.T) {
		require.Equal(t, "/", openapi.Root().String())
		require.Equal(t, "/", openapi.Path{}.String())
		require.Equal(t, "/carts", carts.String())
		require.Equal(t, "/carts/{cartId}/items", items.String())
	})

	t.Run("two paths built from one prefix share no backing array", func(t *testing.T) {
		prefix := openapi.Root().Lit("a").Lit("b")
		x := prefix.Lit("x")
		y := prefix.Lit("y")
		z := prefix.Param(cartID)
		require.Equal(t, "/a/b/x", x.String())
		require.Equal(t, "/a/b/y", y.String())
		require.Equal(t, "/a/b/{cartId}", z.String())
		require.Equal(t, "/a/b", prefix.String())
	})
}

func TestParamFrom_panics_for_a_parameter_the_operation_doesnt_read(t *testing.T) {
	itemID := openapi.PathParam[string]("itemId")

	t.Run("a Request no API made", func(t *testing.T) {
		require.PanicsWithValue(t, `openapi: ParamFrom: operation (none) doesn't read path parameter "itemId"`, func() {
			openapi.ParamFrom(openapi.Request[openapi.EmptyBody]{}, itemID)
		})
	})

	t.Run("a parameter of another path, or of another type", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cartAsInt := openapi.PathParam[int]("cartId")
			panics := make(chan any, 2)
			recovering := func(read func(openapi.Request[openapi.EmptyBody])) func(context.Context, openapi.Request[openapi.EmptyBody]) (cartBody, error) {
				return func(_ context.Context, req openapi.Request[openapi.EmptyBody]) (cartBody, error) {
					defer func() { panics <- recover() }()
					read(req)
					return cartBody{}, nil
				}
			}
			s := serveAPI(t, []openapi.Operation{
				openapi.Get(cart, recovering(func(req openapi.Request[openapi.EmptyBody]) { openapi.ParamFrom(req, itemID) }), jsonEncoder),
				openapi.Get(cart.Lit("n"), recovering(func(req openapi.Request[openapi.EmptyBody]) { openapi.ParamFrom(req, cartAsInt) }), jsonEncoder),
			})

			s.get("/carts/c-1")
			require.Equal(t, `openapi: ParamFrom: operation GET /carts/{cartId} doesn't read path parameter "itemId"`, <-panics)
			s.get("/carts/c-1/n")
			require.Equal(t, `openapi: ParamFrom: operation GET /carts/{cartId}/n reads path parameter "cartId" as string, not as int`, <-panics)

			s.p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-s.status).Code())
		})
	})
}

func TestRequestError(t *testing.T) {
	err := &openapi.RequestError{Status: http.StatusBadRequest, Param: "cartId", Err: context.Canceled}
	require.Equal(t, "openapi: request rejected with 400 Bad Request: context canceled", err.Error())
	require.ErrorIs(t, err, context.Canceled)

	err = &openapi.RequestError{Status: http.StatusUnsupportedMediaType}
	require.Equal(t, "openapi: request rejected with 415 Unsupported Media Type", err.Error())
	require.NoError(t, err.Unwrap())
}
