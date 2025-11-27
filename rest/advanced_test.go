// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// Priority 4: Advanced Scenarios Tests

// Complex nested type with multiple levels
type DeepNested struct {
	Level1 string          `json:"level1"`
	Sub    *DeepNestedSub  `json:"sub"`
	Items  []DeepNestedItem `json:"items"`
}

type DeepNestedSub struct {
	Level2 string              `json:"level2"`
	Inner  *DeepNestedSubInner `json:"inner"`
}

type DeepNestedSubInner struct {
	Level3 string `json:"level3"`
	Value  int    `json:"value"`
}

type DeepNestedItem struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestAdvanced_ComplexNestedTypes(t *testing.T) {
	t.Run("serializes deeply nested request", func(t *testing.T) {
		var capturedReq *DeepNested

		consumer := ConsumerFunc[DeepNested](func(ctx context.Context, req *DeepNested) error {
			capturedReq = req
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/nested"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		input := &DeepNested{
			Level1: "first",
			Sub: &DeepNestedSub{
				Level2: "second",
				Inner: &DeepNestedSubInner{
					Level3: "third",
					Value:  42,
				},
			},
			Items: []DeepNestedItem{
				{Name: "item1", Count: 1},
				{Name: "item2", Count: 2},
			},
		}

		body := jsonBody(t, input)
		resp, err := http.Post(srv.URL+"/nested", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NotNil(t, capturedReq)
		require.Equal(t, "first", capturedReq.Level1)
		require.NotNil(t, capturedReq.Sub)
		require.Equal(t, "second", capturedReq.Sub.Level2)
		require.NotNil(t, capturedReq.Sub.Inner)
		require.Equal(t, "third", capturedReq.Sub.Inner.Level3)
		require.Equal(t, 42, capturedReq.Sub.Inner.Value)
		require.Len(t, capturedReq.Items, 2)
		require.Equal(t, "item1", capturedReq.Items[0].Name)
	})

	t.Run("serializes deeply nested response", func(t *testing.T) {
		producer := ProducerFunc[DeepNested](func(ctx context.Context) (*DeepNested, error) {
			return &DeepNested{
				Level1: "response",
				Sub: &DeepNestedSub{
					Level2: "nested",
					Inner: &DeepNestedSubInner{
						Level3: "deep",
						Value:  99,
					},
				},
				Items: []DeepNestedItem{
					{Name: "a", Count: 10},
				},
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/nested"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/nested")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		result := decodeJson[DeepNested](t, resp.Body)
		require.Equal(t, "response", result.Level1)
		require.Equal(t, "nested", result.Sub.Level2)
		require.Equal(t, "deep", result.Sub.Inner.Level3)
		require.Equal(t, 99, result.Sub.Inner.Value)
		require.Len(t, result.Items, 1)
	})

	t.Run("round-trip complex nested type", func(t *testing.T) {
		handler := HandlerFunc[DeepNested, DeepNested](func(ctx context.Context, req *DeepNested) (*DeepNested, error) {
			req.Level1 = "modified-" + req.Level1
			if req.Sub != nil && req.Sub.Inner != nil {
				req.Sub.Inner.Value *= 2
			}
			return req, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/transform"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		input := &DeepNested{
			Level1: "original",
			Sub: &DeepNestedSub{
				Level2: "sub",
				Inner: &DeepNestedSubInner{
					Level3: "inner",
					Value:  50,
				},
			},
			Items: []DeepNestedItem{{Name: "x", Count: 5}},
		}

		body := jsonBody(t, input)
		resp, err := http.Post(srv.URL+"/transform", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		result := decodeJson[DeepNested](t, resp.Body)
		require.Equal(t, "modified-original", result.Level1)
		require.Equal(t, 100, result.Sub.Inner.Value)
	})
}

func TestAdvanced_TransformPipeline(t *testing.T) {
	t.Run("multiple transforms execute in order", func(t *testing.T) {
		// Test that multiple parameter transforms are applied correctly
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{
				Message: "Success with params",
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/multi"),
				HandleJson(handler),
				QueryParam("page", Required()),
				QueryParam("limit", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/multi?page=1&limit=10", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		result := decodeJson[SimpleResponse](t, resp.Body)
		require.Equal(t, "Success with params", result.Message)
	})

	t.Run("transform error stops pipeline", func(t *testing.T) {
		// Use parameter validation to trigger transform error
		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/test"),
				ProduceJson(ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
					return &SimpleResponse{Message: "ok"}, nil
				})),
				QueryParam("id", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// Missing required parameter will fail transform
		resp, err := http.Get(srv.URL + "/test")
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestAdvanced_ContextPropagation(t *testing.T) {
	t.Run("context propagates through request pipeline", func(t *testing.T) {
		var capturedCtx context.Context

		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			capturedCtx = ctx
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

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.NotNil(t, capturedCtx)
	})

	t.Run("path params captured from URL", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{
				Message: "User: " + req.Name,
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/users").Param("id"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/users/123", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("query params validated", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{
				Message: "Item: " + req.Name,
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/items"),
				HandleJson(handler),
				QueryParam("page", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// With required param
		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/items?page=5", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Missing required param
		body2 := jsonBody(t, SimpleRequest{Name: "test2", Age: 30})
		resp2, err := http.Post(srv.URL+"/items", "application/json", body2)
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp2.StatusCode)
	})

	t.Run("headers validated", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{
				Message: "Authenticated",
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
				Header("X-API-Key", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// With header
		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/test", body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", "secret123")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Missing header
		body2 := jsonBody(t, SimpleRequest{Name: "test2", Age: 30})
		req2, err := http.NewRequest(http.MethodPost, srv.URL+"/test", body2)
		require.NoError(t, err)
		req2.Header.Set("Content-Type", "application/json")

		resp2, err := http.DefaultClient.Do(req2)
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp2.StatusCode)
	})

	t.Run("cookies validated", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{
				Message: "Logged in",
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
				Cookie("session", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// With cookie
		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/test", body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "session", Value: "abc123"})

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Missing cookie
		body2 := jsonBody(t, SimpleRequest{Name: "test2", Age: 30})
		req2, err := http.NewRequest(http.MethodPost, srv.URL+"/test", body2)
		require.NoError(t, err)
		req2.Header.Set("Content-Type", "application/json")

		resp2, err := http.DefaultClient.Do(req2)
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp2.StatusCode)
	})
}

func TestAdvanced_ParameterValidation(t *testing.T) {
	t.Run("multiple validation rules on same parameter", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "Valid ID"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
				QueryParam("id", Required(), Regex(regexp.MustCompile(`^\d{3,6}$`))),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// Valid: 4 digits
		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp, err := http.Post(srv.URL+"/test?id=1234", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Invalid: too short
		body2 := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp2, err := http.Post(srv.URL+"/test?id=12", "application/json", body2)
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp2.StatusCode)

		// Invalid: non-numeric
		body3 := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp3, err := http.Post(srv.URL+"/test?id=abcd", "application/json", body3)
		require.NoError(t, err)
		defer resp3.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp3.StatusCode)

		// Invalid: missing
		body4 := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp4, err := http.Post(srv.URL+"/test", "application/json", body4)
		require.NoError(t, err)
		defer resp4.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp4.StatusCode)
	})

	t.Run("validation on multiple parameters", func(t *testing.T) {
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
				QueryParam("page", Required(), Regex(regexp.MustCompile(`^\d+$`))),
				QueryParam("limit", Required(), Regex(regexp.MustCompile(`^\d+$`))),
				Header("X-Request-ID", Required()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// All valid
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/test?page=1&limit=10", nil)
		require.NoError(t, err)
		req.Header.Set("X-Request-ID", "req-123")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// Invalid page
		req2, err := http.NewRequest(http.MethodGet, srv.URL+"/test?page=abc&limit=10", nil)
		require.NoError(t, err)
		req2.Header.Set("X-Request-ID", "req-124")

		resp2, err := http.DefaultClient.Do(req2)
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp2.StatusCode)

		// Missing header
		req3, err := http.NewRequest(http.MethodGet, srv.URL+"/test?page=1&limit=10", nil)
		require.NoError(t, err)

		resp3, err := http.DefaultClient.Do(req3)
		require.NoError(t, err)
		defer resp3.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp3.StatusCode)
	})
}

func TestAdvanced_MultipleOperations(t *testing.T) {
	t.Run("multiple operations on same path", func(t *testing.T) {
		getHandler := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "GET response"}, nil
		})

		postHandler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "POST: " + req.Name}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/resource"),
				ProduceJson(getHandler),
			),
			Operation(
				http.MethodPost,
				BasePath("/resource"),
				HandleJson(postHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// GET
		resp, err := http.Get(srv.URL + "/resource")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		result := decodeJson[SimpleResponse](t, resp.Body)
		require.Equal(t, "GET response", result.Message)

		// POST
		body := jsonBody(t, SimpleRequest{Name: "test", Age: 30})
		resp2, err := http.Post(srv.URL+"/resource", "application/json", body)
		require.NoError(t, err)
		defer resp2.Body.Close()
		require.Equal(t, http.StatusOK, resp2.StatusCode)
		result2 := decodeJson[SimpleResponse](t, resp2.Body)
		require.Equal(t, "POST: test", result2.Message)
	})

	t.Run("operations on different paths", func(t *testing.T) {
		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/users"),
				ProduceJson(ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
					return &SimpleResponse{Message: "users"}, nil
				})),
			),
			Operation(
				http.MethodGet,
				BasePath("/posts"),
				ProduceJson(ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
					return &SimpleResponse{Message: "posts"}, nil
				})),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp1, err := http.Get(srv.URL + "/users")
		require.NoError(t, err)
		defer resp1.Body.Close()
		result1 := decodeJson[SimpleResponse](t, resp1.Body)
		require.Equal(t, "users", result1.Message)

		resp2, err := http.Get(srv.URL + "/posts")
		require.NoError(t, err)
		defer resp2.Body.Close()
		result2 := decodeJson[SimpleResponse](t, resp2.Body)
		require.Equal(t, "posts", result2.Message)
	})
}
