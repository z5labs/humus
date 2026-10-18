// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	openapi "github.com/z5labs/humus/openapi/v3"
)

// docPath is the path the tests serve the document at.
var docPath = openapi.Root().Lit("openapi.json")

// schemaBaseID is the $id of the OpenAPI Initiative's published 3.2
// schema-base, which checks each Schema Object against the 3.2 dialect too.
const schemaBaseID = "https://spec.openapis.org/oas/3.2/schema-base/2025-09-17"

// oas32 is the 3.2 schema-base, compiled once from testdata/oas3.2, which
// holds it with the schema, the dialect and the meta-schema it refers to,
// so validating reads nothing from the network.
var oas32 = sync.OnceValues(func() (*jsonschema.Schema, error) {
	files, err := filepath.Glob(filepath.Join("testdata", "oas3.2", "*.json"))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		id, _ := doc.(map[string]any)["$id"].(string)
		if err := c.AddResource(id, doc); err != nil {
			return nil, err
		}
	}
	return c.Compile(schemaBaseID)
})

// validate validates doc against the OpenAPI Initiative's 3.2 schema-base.
func validate(doc []byte) error {
	schema, err := oas32()
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return err
	}
	return schema.Validate(inst)
}

// requireValid checks that doc is a valid OpenAPI 3.2 document. Every
// document a test generates goes through it.
func requireValid(t *testing.T, doc []byte) {
	t.Helper()
	require.NoError(t, validate(doc), "%s", doc)
}

// documentOf builds the API of ops and opts, with a Document at docPath,
// and returns the document it serves, checked valid.
func documentOf(t *testing.T, ops []openapi.Operation, opts ...openapi.APIOption) []byte {
	t.Helper()
	api, err := newAPI(setup(), append(ops, openapi.Document(docPath)), opts...)
	require.NoError(t, err)
	return serveDocument(t, api)
}

// serveDocument returns the document api serves at docPath, checked valid.
func serveDocument(t *testing.T, api http.Handler) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, docPath.String(), nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	doc := rec.Body.Bytes()
	requireValid(t, doc)
	return doc
}

// field returns the JSON at keys within doc, such as the operation at
// "paths", "/carts", "get".
func field(t *testing.T, doc []byte, keys ...string) string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(doc, &v))
	for _, k := range keys {
		m, ok := v.(map[string]any)
		require.True(t, ok, "%s isn't an object in %s", k, doc)
		v, ok = m[k]
		require.True(t, ok, "no %s in %s", k, doc)
	}
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// keys returns the keys of the object at keys within doc, sorted.
func keys(t *testing.T, doc []byte, at ...string) []string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(field(t, doc, at...)), &m))
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// schemaOf is SchemaOf's schema of T, by tag, as a string.
func schemaOf[T any](t *testing.T, tag string) string {
	t.Helper()
	s, err := openapi.SchemaOf[T](tag)
	require.NoError(t, err)
	return string(s)
}

func TestDocument_is_served_at_its_path(t *testing.T) {
	t.Run("the same bytes on every request, describing the API but not itself", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serve(t, func(st *humus.Setup) (http.Handler, error) {
				return openapi.NewAPI(st,
					openapi.Info{Title: "Carts", Version: "1.0.0", Description: "Shopping carts"},
					unmatched,
					openapi.RequestErrors(rejected, problemEncoder),
					openapi.CatchAll(http.StatusInternalServerError, internalError, problemEncoder),
					[]openapi.Operation{openapi.Get(cart, getCart, jsonEncoder), openapi.Document(docPath)},
				)
			})

			first := s.get("/openapi.json")
			require.Equal(t, http.StatusOK, first.status)
			require.Equal(t, "application/json", first.contentType)
			requireValid(t, []byte(first.body))
			second := s.get("/openapi.json")
			require.Equal(t, first.body, second.body)

			doc := []byte(first.body)
			require.Equal(t, `"3.2.0"`, field(t, doc, "openapi"))
			require.JSONEq(t, `{"title":"Carts","version":"1.0.0","description":"Shopping carts"}`, field(t, doc, "info"))
			require.Equal(t, []string{"/carts/{cartId}"}, keys(t, doc, "paths"))

			// The document's path is routed like an operation's.
			r := s.do(http.MethodPost, "/openapi.json", "", "")
			require.Equal(t, http.StatusMethodNotAllowed, r.status)
			require.Equal(t, "the end user's 405, allowing GET", r.body)
			s.stop()
		})
	})

	t.Run("no document without a Document", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := serveAPI(t, []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder)})

			r := s.get("/openapi.json")
			require.Equal(t, http.StatusNotFound, r.status)
			require.Equal(t, "the end user's 404", r.body)
			s.stop()
		})
	})
}

// sku is a body with a schema of its own under each codec: its properties
// are named by json tags as JSON, and by xml tags as XML.
type sku struct {
	Code  string  `json:"code" xml:"code,attr"`
	Units int64   `json:"units" xml:"count"`
	Price float64 `json:"price" xml:"price"`
	Size  size    `json:"size" xml:"dimensions"`
}

type size struct {
	Width int `json:"width" xml:"w"`
}

func echoSKU(_ context.Context, req openapi.Request[sku]) (sku, error) { return req.Body(), nil }

func echoBytes(_ context.Context, req openapi.Request[[]byte]) ([]byte, error) {
	return req.Body(), nil
}

func TestDocument_lists_each_body_under_its_codec(t *testing.T) {
	skus := openapi.Root().Lit("skus")
	doc := documentOf(t, []openapi.Operation{
		openapi.Post(skus, echoSKU, jsonDecoder, xmlEncoder, openapi.Status(http.StatusCreated)),
		openapi.Put(skus, echoSKU, xmlDecoder, jsonEncoder),
		openapi.Post(openapi.Root().Lit("blobs"), echoBytes, rawDecoder, rawEncoder),
		openapi.Delete(cart, nothingBack, openapi.EmptyEncoder),
	})
	asJSON, asXML := schemaOf[sku](t, "json"), schemaOf[sku](t, "xml")
	require.NotEqual(t, asJSON, asXML)

	t.Run("a JSON-like body and an XML-like body, each under its media type and schema", func(t *testing.T) {
		require.JSONEq(t, `{"required":true,"content":{"application/json":{"schema":`+asJSON+`}}}`,
			field(t, doc, "paths", "/skus", "post", "requestBody"))
		require.JSONEq(t, `{"description":"Created","content":{"application/xml":{"schema":`+asXML+`}}}`,
			field(t, doc, "paths", "/skus", "post", "responses", "201"))
		require.JSONEq(t, `{"required":true,"content":{"application/xml":{"schema":`+asXML+`}}}`,
			field(t, doc, "paths", "/skus", "put", "requestBody"))
		require.JSONEq(t, `{"description":"OK","content":{"application/json":{"schema":`+asJSON+`}}}`,
			field(t, doc, "paths", "/skus", "put", "responses", "200"))
	})

	t.Run("a raw body, its media type with no schema", func(t *testing.T) {
		require.JSONEq(t, `{"required":true,"content":{"application/octet-stream":{}}}`,
			field(t, doc, "paths", "/blobs", "post", "requestBody"))
		require.JSONEq(t, `{"description":"OK","content":{"application/octet-stream":{}}}`,
			field(t, doc, "paths", "/blobs", "post", "responses", "200"))
	})

	t.Run("EmptyDecoder's no request body, and EmptyEncoder's 204 with no content", func(t *testing.T) {
		require.NotContains(t, field(t, doc, "paths", "/carts/{cartId}", "delete"), "requestBody")
		require.JSONEq(t, `{"description":"No Content"}`, field(t, doc, "paths", "/carts/{cartId}", "delete", "responses", "204"))
	})
}

func TestDocument_lists_RequestErrors_where_the_operation_can_answer_with_them(t *testing.T) {
	doc := documentOf(t, []openapi.Operation{
		openapi.Get(carts, noBody, jsonEncoder),
		openapi.Get(cart, getCart, jsonEncoder),
		openapi.Get(carts.Lit("recent"), noBody, jsonEncoder, openapi.Query(includeParam)),
		openapi.Post(carts, addItem, jsonDecoder, jsonEncoder),
	})
	problemContent := `"content":{"application/problem+json":{"schema":` + schemaOf[problem](t, "json") + `}}`

	require.Equal(t, []string{"200", "500"}, keys(t, doc, "paths", "/carts", "get", "responses"), "no parameter and no body")
	require.Equal(t, []string{"200", "400", "500"}, keys(t, doc, "paths", "/carts/{cartId}", "get", "responses"), "a path parameter")
	require.Equal(t, []string{"200", "400", "500"}, keys(t, doc, "paths", "/carts/recent", "get", "responses"), "a query parameter")
	require.Equal(t, []string{"200", "400", "413", "415", "500"}, keys(t, doc, "paths", "/carts", "post", "responses"), "a body")

	for status, text := range map[string]string{"400": "Bad Request", "413": "Request Entity Too Large", "415": "Unsupported Media Type", "500": "Internal Server Error"} {
		require.JSONEq(t, `{"description":"`+text+`",`+problemContent+`}`, field(t, doc, "paths", "/carts", "post", "responses", status))
	}
}

func TestDocument_lists_each_Error(t *testing.T) {
	doc := documentOf(t, []openapi.Operation{
		openapi.Get(cart, getCart, jsonEncoder),
		openapi.Delete(cart, nothingBack, openapi.EmptyEncoder, openapi.Error[gone](http.StatusGone, jsonEncoder)),
		openapi.Post(carts, addItem, jsonDecoder, jsonEncoder, openapi.Error[notFound](http.StatusConflict, jsonEncoder)),
	}, openapi.Error[notFound](http.StatusNotFound, problemEncoder))

	notFoundProblem := `{"description":"Not Found","content":{"application/problem+json":{"schema":` + schemaOf[notFound](t, "json") + `}}}`
	require.JSONEq(t, notFoundProblem, field(t, doc, "paths", "/carts/{cartId}", "get", "responses", "404"), "the API's Error")
	require.JSONEq(t, notFoundProblem, field(t, doc, "paths", "/carts/{cartId}", "delete", "responses", "404"), "the API's Error")
	require.JSONEq(t, `{"description":"Gone","content":{"application/json":{"schema":`+schemaOf[gone](t, "json")+`}}}`,
		field(t, doc, "paths", "/carts/{cartId}", "delete", "responses", "410"), "the operation's own")

	// The operation's own Error for notFound replaces the API's.
	require.Equal(t, []string{"200", "400", "409", "413", "415", "500"}, keys(t, doc, "paths", "/carts", "post", "responses"))
	require.JSONEq(t, `{"description":"Conflict","content":{"application/json":{"schema":`+schemaOf[notFound](t, "json")+`}}}`,
		field(t, doc, "paths", "/carts", "post", "responses", "409"))
}

func TestDocument_lists_each_metadata_part(t *testing.T) {
	doc := documentOf(t, []openapi.Operation{
		openapi.Get(cart, getCart, jsonEncoder,
			openapi.Summary("Get a cart"),
			openapi.Description("Returns the cart, with **every** item."),
			openapi.OperationID("getCart"),
			openapi.Tags("carts", "read"),
			openapi.Tags("public"),
			openapi.Deprecated(),
		),
		openapi.Get(carts, noBody, jsonEncoder),
	})

	op := field(t, doc, "paths", "/carts/{cartId}", "get")
	require.Equal(t, `"Get a cart"`, field(t, []byte(op), "summary"))
	require.Equal(t, `"Returns the cart, with **every** item."`, field(t, []byte(op), "description"))
	require.Equal(t, `"getCart"`, field(t, []byte(op), "operationId"))
	require.Equal(t, `["carts","read","public"]`, field(t, []byte(op), "tags"))
	require.Equal(t, `true`, field(t, []byte(op), "deprecated"))

	require.Equal(t, []string{"responses"}, keys(t, doc, "paths", "/carts", "get"), "an operation without metadata")
}

func TestDocument_lists_each_parameter_with_its_constraints(t *testing.T) {
	type strict bool
	itemID := openapi.PathParam[int64]("itemId").Description("The item").Minimum(1).Example(7)
	doc := documentOf(t, []openapi.Operation{
		openapi.Get(cart.Lit("items").Param(itemID), noBody, jsonEncoder,
			openapi.Query(includeParam.Description("What the cart includes").Default("count")),
			openapi.Query(openapi.QueryParam[int]("limit").Required().Minimum(1).Maximum(100).Example(10)),
			openapi.Query(openapi.QueryParam[float64]("ratio").Minimum(0).Maximum(1).Default(0.5)),
			openapi.Header(openapi.HeaderParam[string]("X-Sku").Pattern(regexp.MustCompile(`^[A-Z]+$`)).MinLength(2).MaxLength(8)),
			openapi.Header(openapi.HeaderParam[strict]("X-Strict").Enum(true)),
		),
	})

	require.JSONEq(t, `[
		{"name":"cartId","in":"path","required":true,"schema":{"type":"string"}},
		{"name":"itemId","in":"path","description":"The item","required":true,"schema":{"type":"integer","format":"int64","minimum":1},"example":7},
		{"name":"include","in":"query","description":"What the cart includes","schema":{"type":"string","enum":["items","count"],"default":"count"}},
		{"name":"limit","in":"query","required":true,"schema":{"type":"integer","minimum":1,"maximum":100},"example":10},
		{"name":"ratio","in":"query","schema":{"type":"number","format":"double","default":0.5,"minimum":0,"maximum":1}},
		{"name":"X-Sku","in":"header","schema":{"type":"string","pattern":"^[A-Z]+$","minLength":2,"maxLength":8}},
		{"name":"X-Strict","in":"header","schema":{"type":"boolean","enum":[true]}}
	]`, field(t, doc, "paths", "/carts/{cartId}/items/{itemId}", "get", "parameters"))
}

func TestDocument_golden(t *testing.T) {
	itemID := openapi.PathParam[string]("itemId").Description("Cart item identifier")
	doc := documentOf(t, []openapi.Operation{
		openapi.Get(cart, getCart, jsonEncoder,
			openapi.OperationID("getCart"),
			openapi.Summary("Get a shopping cart"),
			openapi.Tags("carts"),
		),
		openapi.Post(items, addItem, jsonDecoder, jsonEncoder,
			openapi.OperationID("addCartItem"),
			openapi.Summary("Add an item to a shopping cart"),
			openapi.Tags("carts", "items"),
			openapi.Status(http.StatusCreated),
		),
		openapi.Delete(items.Param(itemID), nothingBack, openapi.EmptyEncoder,
			openapi.OperationID("removeCartItem"),
			openapi.Summary("Remove an item from a shopping cart"),
			openapi.Tags("carts", "items"),
			openapi.Header(openapi.HeaderParam[string]("If-Match").Description("The cart's ETag")),
		),
	}, openapi.Error[notFound](http.StatusNotFound, problemEncoder))

	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, doc, "", "  "))
	indented.WriteByte('\n')
	want, err := os.ReadFile(filepath.Join("testdata", "document.golden.json"))
	require.NoError(t, err)
	require.Equal(t, string(want), indented.String())
}

func TestDocument_is_checked_against_OpenAPI_3_2(t *testing.T) {
	doc := documentOf(t, []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder)})
	require.NoError(t, validate(doc))

	older := strings.Replace(string(doc), `"openapi":"3.2.0"`, `"openapi":"3.1.0"`, 1)
	require.NotEqual(t, string(doc), older)
	require.Error(t, validate([]byte(older)), "a 3.1.0 document fails the 3.2 schema")
}
