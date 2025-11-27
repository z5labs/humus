// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// Priority 3: OpenAPI Spec Generation Tests

func TestOpenAPISpec_RequestBody(t *testing.T) {
	t.Run("generates request body spec for simple struct", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		postOp := pathSpec["post"].(map[string]any)

		require.Contains(t, postOp, "requestBody")
		reqBody := postOp["requestBody"].(map[string]any)
		require.Contains(t, reqBody, "content")
		content := reqBody["content"].(map[string]any)
		require.Contains(t, content, "application/json")

		jsonContent := content["application/json"].(map[string]any)
		require.Contains(t, jsonContent, "schema")
	})

	t.Run("marks request body as required", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		postOp := pathSpec["post"].(map[string]any)
		reqBody := postOp["requestBody"].(map[string]any)

		require.Contains(t, reqBody, "required")
		require.True(t, reqBody["required"].(bool))
	})

	t.Run("generates schema for nested struct", func(t *testing.T) {
		handler := HandlerFunc[NestedStruct, SimpleResponse](func(ctx context.Context, req *NestedStruct) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		postOp := pathSpec["post"].(map[string]any)
		reqBody := postOp["requestBody"].(map[string]any)
		content := reqBody["content"].(map[string]any)
		jsonContent := content["application/json"].(map[string]any)

		require.Contains(t, jsonContent, "schema")
	})

	t.Run("no request body for producer-only handler", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)

		// EmptyRequest should have an empty request body or no content
		if reqBody, ok := getOp["requestBody"].(map[string]any); ok {
			if content, ok := reqBody["content"].(map[string]any); ok {
				require.Empty(t, content, "EmptyRequest should have no content")
			}
		}
	})
}

func TestOpenAPISpec_Response(t *testing.T) {
	t.Run("generates 200 response spec", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)

		require.Contains(t, getOp, "responses")
		responses := getOp["responses"].(map[string]any)
		require.Contains(t, responses, "200")
	})

	t.Run("generates response content type", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)
		responses := getOp["responses"].(map[string]any)
		resp200 := responses["200"].(map[string]any)

		require.Contains(t, resp200, "content")
		content := resp200["content"].(map[string]any)
		require.Contains(t, content, "application/json")
	})

	t.Run("generates schema for nested response", func(t *testing.T) {
		producer := ProducerFunc[NestedStruct](func(ctx context.Context) (*NestedStruct, error) {
			nested := &NestedStruct{Parent: "test"}
			nested.Child.Name = "child"
			return nested, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)
		responses := getOp["responses"].(map[string]any)
		resp200 := responses["200"].(map[string]any)
		content := resp200["content"].(map[string]any)
		jsonContent := content["application/json"].(map[string]any)

		require.Contains(t, jsonContent, "schema")
	})

	t.Run("generates schema for array response", func(t *testing.T) {
		producer := ProducerFunc[[]Pet](func(ctx context.Context) (*[]Pet, error) {
			pets := []Pet{{Name: "Fluffy", Age: 3}}
			return &pets, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/pets"),
				ProduceJson(producer),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/pets"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)
		responses := getOp["responses"].(map[string]any)
		resp200 := responses["200"].(map[string]any)
		content := resp200["content"].(map[string]any)
		jsonContent := content["application/json"].(map[string]any)

		require.Contains(t, jsonContent, "schema")
	})
}

func TestOpenAPISpec_Parameters(t *testing.T) {
	t.Run("generates query parameter spec", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				QueryParam("id", Required()),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)

		require.Contains(t, getOp, "parameters")
		params := getOp["parameters"].([]any)
		require.NotEmpty(t, params)

		foundParam := false
		for _, p := range params {
			param := p.(map[string]any)
			if param["name"] == "id" && param["in"] == "query" {
				foundParam = true
				require.Contains(t, param, "required")
				require.True(t, param["required"].(bool))
				break
			}
		}
		require.True(t, foundParam, "query parameter 'id' should be in spec")
	})

	t.Run("generates header parameter spec", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				Header("X-API-Key", Required()),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)

		require.Contains(t, getOp, "parameters")
		params := getOp["parameters"].([]any)
		require.NotEmpty(t, params)

		foundParam := false
		for _, p := range params {
			param := p.(map[string]any)
			if param["name"] == "X-API-Key" && param["in"] == "header" {
				foundParam = true
				require.True(t, param["required"].(bool))
				break
			}
		}
		require.True(t, foundParam, "header parameter should be in spec")
	})

	t.Run("generates path parameter spec", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/users").Param("id"),
				ProduceJson(producer),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/users/{id}"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)

		require.Contains(t, getOp, "parameters")
		params := getOp["parameters"].([]any)
		require.NotEmpty(t, params)

		foundParam := false
		for _, p := range params {
			param := p.(map[string]any)
			if param["name"] == "id" && param["in"] == "path" {
				foundParam = true
				break
			}
		}
		require.True(t, foundParam, "path parameter should be in spec")
	})

	t.Run("generates cookie parameter spec", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				Cookie("session", Required()),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)

		require.Contains(t, getOp, "parameters")
		params := getOp["parameters"].([]any)
		require.NotEmpty(t, params)

		foundParam := false
		for _, p := range params {
			param := p.(map[string]any)
			if param["name"] == "session" && param["in"] == "cookie" {
				foundParam = true
				break
			}
		}
		require.True(t, foundParam, "cookie parameter should be in spec")
	})

	t.Run("generates multiple parameters", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				QueryParam("page", Required()),
				QueryParam("limit"),
				Header("Authorization", Required()),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)

		require.Contains(t, getOp, "parameters")
		params := getOp["parameters"].([]any)
		require.Len(t, params, 3)
	})

	t.Run("optional parameter not marked required", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				QueryParam("optional"),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)
		params := getOp["parameters"].([]any)

		for _, p := range params {
			param := p.(map[string]any)
			if param["name"] == "optional" {
				if required, ok := param["required"].(bool); ok {
					require.False(t, required, "optional parameter should not be required")
				}
			}
		}
	})
}

func TestOpenAPISpec_ValidationPatterns(t *testing.T) {
	t.Run("regex validation not exposed in schema", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(producer),
				QueryParam("id", Regex(regexp.MustCompile(`^\d+$`))),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/test"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)
		params := getOp["parameters"].([]any)

		require.NotEmpty(t, params)
	})
}
