// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test utilities and mock types for operation testing

// TestRequest is a test request type
type TestRequest struct {
	Input string `json:"input"`
	Count int    `json:"count"`
}

// TestResponse is a test response type
type TestResponse struct {
	Output string `json:"output"`
	Result int    `json:"result"`
}

// captureErrorHandler captures error handler calls for testing
type captureErrorHandler struct {
	called      bool
	capturedCtx context.Context
	capturedErr error
}

func (c *captureErrorHandler) OnError(ctx context.Context, w http.ResponseWriter, err error) {
	c.called = true
	c.capturedCtx = ctx
	c.capturedErr = err
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte("custom error"))
}

// addContextValue creates a transform function that adds a value to the context
func addContextValue(key, value string) func(*http.Request) (*http.Request, error) {
	return func(r *http.Request) (*http.Request, error) {
		ctx := context.WithValue(r.Context(), key, value)
		return r.WithContext(ctx), nil
	}
}

// failingTransform creates a transform that returns an error
func failingTransform(err error) func(*http.Request) (*http.Request, error) {
	return func(r *http.Request) (*http.Request, error) {
		return nil, err
	}
}

// getOpenAPISpec fetches and parses the OpenAPI spec from an API
func getOpenAPISpec(t *testing.T, api *Api) map[string]any {
	srv := httptest.NewServer(api)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/openapi.json")
	require.NoError(t, err)
	defer resp.Body.Close()

	var spec map[string]any
	err = json.NewDecoder(resp.Body).Decode(&spec)
	require.NoError(t, err)

	return spec
}

// hasOperation checks if an operation exists in the OpenAPI spec
func hasOperation(spec map[string]any, method, path string) bool {
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		return false
	}

	pathSpec, ok := paths[path].(map[string]any)
	if !ok {
		return false
	}

	_, ok = pathSpec[strings.ToLower(method)]
	return ok
}

// Priority 1: Core Happy Path Tests

func TestOperation_ServeHTTP(t *testing.T) {
	t.Run("handles successful request/response flow", func(t *testing.T) {
		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return &TestResponse{
				Output: "Processed: " + req.Input,
				Result: req.Count * 2,
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/process"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 5}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/process", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var respBody TestResponse
		err = json.NewDecoder(resp.Body).Decode(&respBody)
		require.NoError(t, err)

		assert.Equal(t, "Processed: test", respBody.Output)
		assert.Equal(t, 10, respBody.Result)
	})

	t.Run("stops processing on read error", func(t *testing.T) {
		handlerCalled := false

		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			handlerCalled = true
			return &TestResponse{}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/read-error"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// Send invalid JSON
		body := bytes.NewBufferString(`{invalid json}`)
		resp, err := http.Post(srv.URL+"/read-error", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.False(t, handlerCalled, "handler should not be called on read error")
	})

	t.Run("stops processing on handler error", func(t *testing.T) {
		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return nil, errors.New("handler error")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/handler-error"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/handler-error", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("does not call error handler on success", func(t *testing.T) {
		errHandler := &captureErrorHandler{}

		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return &TestResponse{Output: "success"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/success"),
				HandleJson(handler),
				OnError(errHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/success", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.False(t, errHandler.called, "error handler should not be called on success")
	})

	t.Run("handles empty request body for EmptyRequest types", func(t *testing.T) {
		producer := ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
			return &TestResponse{Output: "no input needed", Result: 42}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/no-input"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/no-input")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var respBody TestResponse
		err = json.NewDecoder(resp.Body).Decode(&respBody)
		require.NoError(t, err)

		assert.Equal(t, "no input needed", respBody.Output)
		assert.Equal(t, 42, respBody.Result)
	})

	t.Run("handles empty response for EmptyResponse types", func(t *testing.T) {
		consumer := ConsumerFunc[TestRequest](func(ctx context.Context, req *TestRequest) error {
			// Process request but don't return response
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/no-output"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/no-output", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Empty(t, bodyBytes, "response body should be empty")
	})

	t.Run("preserves request context through pipeline", func(t *testing.T) {
		// Note: This test validates that the request context flows through
		// the operation pipeline. The HTTP client context doesn't transfer
		// to the server, so we test that the server creates its own context
		// and it flows through to the handler.

		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			// Verify context is not nil
			require.NotNil(t, ctx, "context should not be nil")
			return &TestResponse{Output: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/context"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/context", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	// Priority 2: Error Handling Tests

	t.Run("invokes error handler on panic recovery", func(t *testing.T) {
		errHandler := &captureErrorHandler{}

		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			panic("test panic")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/panic"),
				HandleJson(handler),
				OnError(errHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/panic", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.True(t, errHandler.called, "error handler should be called on panic")
		require.NotNil(t, errHandler.capturedErr, "error should be captured from panic")
	})

	t.Run("calls error handler with correct context and error", func(t *testing.T) {
		errHandler := &captureErrorHandler{}
		expectedErr := errors.New("specific test error")

		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return nil, expectedErr
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/error-details"),
				HandleJson(handler),
				OnError(errHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/error-details", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.True(t, errHandler.called, "error handler should be called")
		require.NotNil(t, errHandler.capturedCtx, "context should be passed to error handler")
		require.NotNil(t, errHandler.capturedErr, "error should be passed to error handler")
		assert.ErrorIs(t, errHandler.capturedErr, expectedErr)
	})
}

func TestOperation(t *testing.T) {
	t.Run("registers operation at correct path and method", func(t *testing.T) {
		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return &TestResponse{Output: "ok"}, nil
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

		// Test correct method works
		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/test", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// Test wrong method returns 405
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/test", nil)
		require.NoError(t, err)

		resp2, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp2.Body.Close()

		assert.Equal(t, http.StatusMethodNotAllowed, resp2.StatusCode)
	})

	t.Run("adds operation to OpenAPI spec", func(t *testing.T) {
		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return &TestResponse{}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/spec-test"),
				HandleJson(handler),
			),
		)

		spec := getOpenAPISpec(t, api)

		assert.True(t, hasOperation(spec, http.MethodPost, "/spec-test"), "operation should be in OpenAPI spec")
	})

	t.Run("includes request body in spec", func(t *testing.T) {
		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return &TestResponse{}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/req-body"),
				HandleJson(handler),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/req-body"].(map[string]any)
		postOp := pathSpec["post"].(map[string]any)

		require.Contains(t, postOp, "requestBody")
		reqBody := postOp["requestBody"].(map[string]any)
		content := reqBody["content"].(map[string]any)
		assert.Contains(t, content, "application/json")
	})

	t.Run("includes response in spec", func(t *testing.T) {
		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return &TestResponse{}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/response"),
				HandleJson(handler),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/response"].(map[string]any)
		postOp := pathSpec["post"].(map[string]any)

		require.Contains(t, postOp, "responses")
		responses := postOp["responses"].(map[string]any)
		require.Contains(t, responses, "200")
		resp200 := responses["200"].(map[string]any)
		content := resp200["content"].(map[string]any)
		assert.Contains(t, content, "application/json")
	})

	t.Run("uses default error handler when none specified", func(t *testing.T) {
		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return nil, errors.New("test error")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/default-error"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/default-error", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	// Priority 3: OpenAPI Spec Generation Tests

	t.Run("registers path parameters automatically", func(t *testing.T) {
		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/users").Param("id"),
				ProduceJson(ProducerFunc[TestResponse](func(ctx context.Context) (*TestResponse, error) {
					return &TestResponse{Output: "ok"}, nil
				})),
			),
		)

		spec := getOpenAPISpec(t, api)

		paths := spec["paths"].(map[string]any)
		pathSpec := paths["/users/{id}"].(map[string]any)
		getOp := pathSpec["get"].(map[string]any)

		require.Contains(t, getOp, "parameters")
		params := getOp["parameters"].([]any)
		require.NotEmpty(t, params, "path parameters should be in spec")

		// Check that one of the parameters is the path parameter "id"
		foundParam := false
		for _, p := range params {
			param := p.(map[string]any)
			if param["name"] == "id" && param["in"] == "path" {
				foundParam = true
				break
			}
		}
		assert.True(t, foundParam, "path parameter 'id' should be in OpenAPI spec")
	})
}

func TestOnError(t *testing.T) {
	t.Run("custom handler called on errors", func(t *testing.T) {
		customHandler := ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
			w.WriteHeader(http.StatusTeapot)
			w.Write([]byte("custom error response"))
		})

		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return nil, errors.New("test error")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/custom-error"),
				HandleJson(handler),
				OnError(customHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/custom-error", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusTeapot, resp.StatusCode)

		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "custom error response", string(bodyBytes))
	})

	t.Run("custom handler receives correct parameters", func(t *testing.T) {
		errHandler := &captureErrorHandler{}
		expectedErr := errors.New("test error")

		handler := HandlerFunc[TestRequest, TestResponse](func(ctx context.Context, req *TestRequest) (*TestResponse, error) {
			return nil, expectedErr
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/capture"),
				HandleJson(handler),
				OnError(errHandler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		reqBody := TestRequest{Input: "test", Count: 1}
		body, err := json.Marshal(reqBody)
		require.NoError(t, err)

		resp, err := http.Post(srv.URL+"/capture", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.True(t, errHandler.called, "error handler should be called")
		require.NotNil(t, errHandler.capturedCtx, "context should be passed to error handler")
		require.NotNil(t, errHandler.capturedErr, "error should be passed to error handler")
		assert.Equal(t, expectedErr.Error(), errHandler.capturedErr.Error())
	})
}
