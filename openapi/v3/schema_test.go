// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	openapi "github.com/z5labs/humus/openapi/v3"
)

// order is a body with nested and repeated structs, numbers of every size,
// and fields the reflection leaves out.
type order struct {
	ID       string            `json:"id" xml:"id,attr"`
	Total    int64             `json:"total" xml:"total"`
	Count    int               `json:"count" xml:"count"`
	Discount float64           `json:"discount" xml:"discount"`
	Weight   float32           `json:"weight" xml:"weight"`
	Note     *int64            `json:"note,omitempty"`
	Lines    []line            `json:"lines" xml:"line"`
	Ship     address           `json:"ship" xml:"shipTo"`
	Meta     map[string]string `json:"meta"`
	Untagged string
	Hidden   string `json:"-" xml:"-"`
}

type line struct {
	SKU   string  `json:"sku" xml:"sku"`
	Price float64 `json:"price" xml:"price"`
}

type address struct {
	City string `json:"city" xml:"city"`
}

// node holds itself, by way of a slice.
type node struct {
	Name     string `json:"name"`
	Children []node `json:"children"`
}

// branch holds itself through another type.
type branch struct {
	Leaf *leaf `json:"leaf"`
}

type leaf struct {
	Parent *branch `json:"parent"`
}

// skipsItself holds itself only through a field its tag leaves out.
type skipsItself struct {
	Name string       `json:"name"`
	Self *skipsItself `json:"-"`
}

func TestSchemaOf(t *testing.T) {
	t.Run("properties are named after the tag, refs are inlined, and int64 and float64 are formatted", func(t *testing.T) {
		require.JSONEq(t, `{
			"type": "object",
			"properties": {
				"id": {"type": "string"},
				"total": {"type": "integer", "format": "int64"},
				"count": {"type": "integer"},
				"discount": {"type": "number", "format": "double"},
				"weight": {"type": "number"},
				"note": {"type": ["null", "integer"], "format": "int64"},
				"lines": {"type": ["array", "null"], "items": {
					"type": "object",
					"properties": {"sku": {"type": "string"}, "price": {"type": "number", "format": "double"}}
				}},
				"ship": {"type": "object", "properties": {"city": {"type": "string"}}},
				"meta": {"type": ["object", "null"], "additionalProperties": {"type": "string"}},
				"Untagged": {"type": "string"}
			}
		}`, schemaOf[order](t, "json"))
	})

	t.Run("another tag names other properties, and a field without it is named after the field", func(t *testing.T) {
		require.JSONEq(t, `{
			"type": "object",
			"properties": {
				"id": {"type": "string"},
				"total": {"type": "integer", "format": "int64"},
				"count": {"type": "integer"},
				"discount": {"type": "number", "format": "double"},
				"weight": {"type": "number"},
				"line": {"type": ["array", "null"], "items": {
					"type": "object",
					"properties": {"sku": {"type": "string"}, "price": {"type": "number", "format": "double"}}
				}},
				"shipTo": {"type": "object", "properties": {"city": {"type": "string"}}},
				"Note": {"type": ["null", "integer"], "format": "int64"},
				"Meta": {"type": ["object", "null"], "additionalProperties": {"type": "string"}},
				"Untagged": {"type": "string"}
			}
		}`, schemaOf[order](t, "xml"))
	})

	t.Run("a type that isn't a struct", func(t *testing.T) {
		require.JSONEq(t, `{"type": "integer", "format": "int64"}`, schemaOf[int64](t, "json"))
		require.JSONEq(t, `{"type": "number", "format": "double"}`, schemaOf[float64](t, "json"))
		require.JSONEq(t, `{"type": "array", "items": {"type": "string"}}`, schemaOf[[]string](t, "json"))
		require.JSONEq(t, `{}`, schemaOf[any](t, "json"))
		require.JSONEq(t, schemaOf[address](t, "json"), schemaOf[*address](t, "json"))
	})

	t.Run("a type that holds itself only through a field it leaves out", func(t *testing.T) {
		require.JSONEq(t, `{"type": "object", "properties": {"name": {"type": "string"}}}`, schemaOf[skipsItself](t, "json"))
	})

	t.Run("its errors", func(t *testing.T) {
		cases := []struct {
			name   string
			schema func() (openapi.Schema, error)
			want   string
		}{
			{
				name:   "an empty tag",
				schema: func() (openapi.Schema, error) { return openapi.SchemaOf[order]("") },
				want:   "openapi: SchemaOf[openapi_test.order]: the tag is empty",
			},
			{
				name:   "a type that holds itself",
				schema: func() (openapi.Schema, error) { return openapi.SchemaOf[node]("json") },
				want:   "openapi: SchemaOf[openapi_test.node]: openapi_test.node holds itself, so its schema can't be inlined",
			},
			{
				name:   "a type that holds itself through another",
				schema: func() (openapi.Schema, error) { return openapi.SchemaOf[[]branch]("json") },
				want:   "openapi: SchemaOf[[]openapi_test.branch]: openapi_test.branch holds itself, so its schema can't be inlined",
			},
			{
				name:   "a type with no JSON Schema",
				schema: func() (openapi.Schema, error) { return openapi.SchemaOf[chan int]("json") },
				want:   "openapi: SchemaOf[chan int]: ",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s, err := tc.schema()
				require.Nil(t, s)
				require.ErrorContains(t, err, tc.want)
			})
		}
	})
}
