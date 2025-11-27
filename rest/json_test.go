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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test utilities and mock types

// SimpleRequest is a mock request type for testing
type SimpleRequest struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

// SimpleResponse is a mock response type for testing
type SimpleResponse struct {
	Message string `json:"message"`
	ID      int    `json:"id"`
}

// NestedStruct is a complex nested type for testing
type NestedStruct struct {
	Parent string `json:"parent"`
	Child  struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	} `json:"child"`
}

// PetList is a slice type for testing array responses
type PetList []Pet

type Pet struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

// mockJsonHandler implements Handler for testing
type mockJsonHandler struct {
	handleFunc func(context.Context, *SimpleRequest) (*SimpleResponse, error)
}

func (m *mockJsonHandler) Handle(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
	if m.handleFunc != nil {
		return m.handleFunc(ctx, req)
	}
	return &SimpleResponse{Message: "success", ID: 123}, nil
}

// mockProducer implements Producer for testing
type mockProducer struct {
	produceFunc func(context.Context) (*SimpleResponse, error)
}

func (m *mockProducer) Produce(ctx context.Context) (*SimpleResponse, error) {
	if m.produceFunc != nil {
		return m.produceFunc(ctx)
	}
	return &SimpleResponse{Message: "produced", ID: 456}, nil
}

// mockConsumer implements Consumer for testing
type mockConsumer struct {
	consumeFunc func(context.Context, *SimpleRequest) error
	capturedReq *SimpleRequest
}

func (m *mockConsumer) Consume(ctx context.Context, req *SimpleRequest) error {
	m.capturedReq = req
	if m.consumeFunc != nil {
		return m.consumeFunc(ctx, req)
	}
	return nil
}

// jsonBody creates a JSON request body from a value
func jsonBody(t *testing.T, v any) io.Reader {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	err := enc.Encode(v)
	require.NoError(t, err)
	return &buf
}

// decodeJson decodes JSON from a reader into a value
func decodeJson[T any](t *testing.T, r io.Reader) *T {
	var result T
	dec := json.NewDecoder(r)
	err := dec.Decode(&result)
	require.NoError(t, err)
	return &result
}

// Priority 1: Core Happy Path Tests

func TestJsonResponse_WriteResponse(t *testing.T) {
	t.Run("writes valid JSON with correct content-type header", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "hello", ID: 42}, nil
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

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/test")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

		result := decodeJson[SimpleResponse](t, resp.Body)
		assert.Equal(t, "hello", result.Message)
		assert.Equal(t, 42, result.ID)
	})

	t.Run("serializes complex nested structs", func(t *testing.T) {
		producer := ProducerFunc[NestedStruct](func(ctx context.Context) (*NestedStruct, error) {
			nested := &NestedStruct{
				Parent: "parent-value",
			}
			nested.Child.Name = "child-name"
			nested.Child.Value = 99
			return nested, nil
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

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		result := decodeJson[NestedStruct](t, resp.Body)
		assert.Equal(t, "parent-value", result.Parent)
		assert.Equal(t, "child-name", result.Child.Name)
		assert.Equal(t, 99, result.Child.Value)
	})

	t.Run("serializes slice responses", func(t *testing.T) {
		producer := ProducerFunc[[]Pet](func(ctx context.Context) (*[]Pet, error) {
			pets := []Pet{
				{Name: "Fluffy", Age: 3},
				{Name: "Spot", Age: 5},
			}
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

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/pets")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		result := decodeJson[[]Pet](t, resp.Body)
		require.Len(t, *result, 2)
		assert.Equal(t, "Fluffy", (*result)[0].Name)
		assert.Equal(t, 3, (*result)[0].Age)
		assert.Equal(t, "Spot", (*result)[1].Name)
		assert.Equal(t, 5, (*result)[1].Age)
	})

	t.Run("returns 200 status code", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "ok"}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/status"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/status")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

func TestJsonResponse_Spec(t *testing.T) {
	t.Run("generates valid OpenAPI response spec", func(t *testing.T) {
		var resp JsonResponse[SimpleResponse]
		status, spec, err := resp.Spec()

		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
		require.NotNil(t, spec.Response)
		assert.Contains(t, spec.Response.Content, "application/json")
	})

	t.Run("generates spec for nested structs", func(t *testing.T) {
		var resp JsonResponse[NestedStruct]
		status, spec, err := resp.Spec()

		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
		require.NotNil(t, spec.Response)
		assert.Contains(t, spec.Response.Content, "application/json")
	})

	t.Run("generates spec for slice types", func(t *testing.T) {
		var resp JsonResponse[[]Pet]
		status, spec, err := resp.Spec()

		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
		require.NotNil(t, spec.Response)
		assert.Contains(t, spec.Response.Content, "application/json")
	})
}

func TestJsonRequest_ReadRequest(t *testing.T) {
	t.Run("deserializes valid JSON request with correct content-type", func(t *testing.T) {
		var capturedReq *SimpleRequest

		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			capturedReq = req
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "Alice", Age: 30})
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		require.NotNil(t, capturedReq)
		assert.Equal(t, "Alice", capturedReq.Name)
		assert.Equal(t, 30, capturedReq.Age)
	})

	t.Run("returns BadRequestError for invalid content-type", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := bytes.NewBufferString(`{"name":"Alice","age":30}`)
		resp, err := http.Post(srv.URL+"/test", "text/plain", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("returns error for malformed JSON", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := bytes.NewBufferString(`{invalid json`)
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("deserializes nested structs", func(t *testing.T) {
		var capturedReq *NestedStruct

		consumer := ConsumerFunc[NestedStruct](func(ctx context.Context, req *NestedStruct) error {
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

		nested := &NestedStruct{Parent: "parent-val"}
		nested.Child.Name = "child-name"
		nested.Child.Value = 123

		body := jsonBody(t, nested)
		resp, err := http.Post(srv.URL+"/nested", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		require.NotNil(t, capturedReq)
		assert.Equal(t, "parent-val", capturedReq.Parent)
		assert.Equal(t, "child-name", capturedReq.Child.Name)
		assert.Equal(t, 123, capturedReq.Child.Value)
	})

	t.Run("handles empty JSON object {}", func(t *testing.T) {
		var capturedReq *SimpleRequest

		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			capturedReq = req
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/empty"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := bytes.NewBufferString(`{}`)
		resp, err := http.Post(srv.URL+"/empty", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		require.NotNil(t, capturedReq)
		assert.Equal(t, "", capturedReq.Name)
		assert.Equal(t, 0, capturedReq.Age)
	})
}

func TestJsonRequest_Spec(t *testing.T) {
	t.Run("generates valid OpenAPI request body spec", func(t *testing.T) {
		var req JsonRequest[SimpleRequest]
		spec, err := req.Spec()

		require.NoError(t, err)
		require.NotNil(t, spec.RequestBody)
		assert.True(t, *spec.RequestBody.Required)
		assert.Contains(t, spec.RequestBody.Content, "application/json")
	})

	t.Run("marks request body as required", func(t *testing.T) {
		var req JsonRequest[SimpleRequest]
		spec, err := req.Spec()

		require.NoError(t, err)
		require.NotNil(t, spec.RequestBody)
		require.NotNil(t, spec.RequestBody.Required)
		assert.True(t, *spec.RequestBody.Required)
	})
}

func TestProduceJson(t *testing.T) {
	t.Run("creates handler that only produces response", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return &SimpleResponse{Message: "produced", ID: 789}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/produce"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/produce")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		result := decodeJson[SimpleResponse](t, resp.Body)
		assert.Equal(t, "produced", result.Message)
		assert.Equal(t, 789, result.ID)
	})

	t.Run("propagates producer errors", func(t *testing.T) {
		producer := ProducerFunc[SimpleResponse](func(ctx context.Context) (*SimpleResponse, error) {
			return nil, errors.New("producer error")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodGet,
				BasePath("/error"),
				ProduceJson(producer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/error")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})
}

func TestConsumeOnlyJson(t *testing.T) {
	t.Run("creates handler that only consumes request", func(t *testing.T) {
		var capturedReq *SimpleRequest

		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			capturedReq = req
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/consume"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "Bob", Age: 25})
		resp, err := http.Post(srv.URL+"/consume", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		require.NotNil(t, capturedReq)
		assert.Equal(t, "Bob", capturedReq.Name)
		assert.Equal(t, 25, capturedReq.Age)
	})

	t.Run("returns empty response body with 200 status", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/empty-response"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "Charlie", Age: 35})
		resp, err := http.Post(srv.URL+"/empty-response", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Empty(t, bodyBytes)
	})

	t.Run("propagates consumer errors", func(t *testing.T) {
		consumer := ConsumerFunc[SimpleRequest](func(ctx context.Context, req *SimpleRequest) error {
			return errors.New("consumer error")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/error"),
				ConsumeOnlyJson(consumer),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "Error", Age: 1})
		resp, err := http.Post(srv.URL+"/error", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})
}

func TestHandleJson(t *testing.T) {
	t.Run("deserializes request and serializes response", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return &SimpleResponse{
				Message: "Hello, " + req.Name,
				ID:      req.Age * 10,
			}, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/handle"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "David", Age: 40})
		resp, err := http.Post(srv.URL+"/handle", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

		result := decodeJson[SimpleResponse](t, resp.Body)
		assert.Equal(t, "Hello, David", result.Message)
		assert.Equal(t, 400, result.ID)
	})

	t.Run("propagates handler errors", func(t *testing.T) {
		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return nil, errors.New("handler error")
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/error"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := jsonBody(t, SimpleRequest{Name: "Error", Age: 1})
		resp, err := http.Post(srv.URL+"/error", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})

	t.Run("handles complex request/response types", func(t *testing.T) {
		handler := HandlerFunc[NestedStruct, NestedStruct](func(ctx context.Context, req *NestedStruct) (*NestedStruct, error) {
			result := &NestedStruct{
				Parent: "Processed: " + req.Parent,
			}
			result.Child.Name = "Transformed: " + req.Child.Name
			result.Child.Value = req.Child.Value * 2
			return result, nil
		})

		api := NewApi(
			"Test",
			"v1",
			Operation(
				http.MethodPost,
				BasePath("/complex"),
				HandleJson(handler),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		input := &NestedStruct{Parent: "input"}
		input.Child.Name = "child"
		input.Child.Value = 50

		body := jsonBody(t, input)
		resp, err := http.Post(srv.URL+"/complex", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		result := decodeJson[NestedStruct](t, resp.Body)
		assert.Equal(t, "Processed: input", result.Parent)
		assert.Equal(t, "Transformed: child", result.Child.Name)
		assert.Equal(t, 100, result.Child.Value)
	})
}

func TestReturnJson(t *testing.T) {
	t.Run("wraps handler and returns JsonResponse", func(t *testing.T) {
		handler := &mockJsonHandler{
			handleFunc: func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
				return &SimpleResponse{Message: "wrapped", ID: 999}, nil
			},
		}

		wrapped := ReturnJson[SimpleRequest, SimpleResponse](handler)

		resp, err := wrapped.Handle(context.Background(), &SimpleRequest{Name: "test", Age: 20})

		require.NoError(t, err)
		require.NotNil(t, resp)
		require.NotNil(t, resp.inner)
		assert.Equal(t, "wrapped", resp.inner.Message)
		assert.Equal(t, 999, resp.inner.ID)
	})

	t.Run("propagates handler errors", func(t *testing.T) {
		expectedErr := errors.New("handler error")
		handler := &mockJsonHandler{
			handleFunc: func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
				return nil, expectedErr
			},
		}

		wrapped := ReturnJson[SimpleRequest, SimpleResponse](handler)

		resp, err := wrapped.Handle(context.Background(), &SimpleRequest{Name: "test", Age: 20})

		assert.ErrorIs(t, err, expectedErr)
		assert.Nil(t, resp)
	})
}

func TestConsumeJson(t *testing.T) {
	t.Run("wraps handler and extracts inner request value", func(t *testing.T) {
		var capturedReq *SimpleRequest

		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			capturedReq = req
			return &SimpleResponse{Message: "ok"}, nil
		})

		wrapped := ConsumeJson[SimpleRequest, SimpleResponse](handler)

		jsonReq := &JsonRequest[SimpleRequest]{
			inner: SimpleRequest{Name: "extracted", Age: 50},
		}

		resp, err := wrapped.Handle(context.Background(), jsonReq)

		require.NoError(t, err)
		require.NotNil(t, resp)
		require.NotNil(t, capturedReq)
		assert.Equal(t, "extracted", capturedReq.Name)
		assert.Equal(t, 50, capturedReq.Age)
	})

	t.Run("propagates handler errors", func(t *testing.T) {
		expectedErr := errors.New("handler error")

		handler := HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
			return nil, expectedErr
		})

		wrapped := ConsumeJson[SimpleRequest, SimpleResponse](handler)

		jsonReq := &JsonRequest[SimpleRequest]{
			inner: SimpleRequest{Name: "test", Age: 1},
		}

		resp, err := wrapped.Handle(context.Background(), jsonReq)

		assert.ErrorIs(t, err, expectedErr)
		assert.Nil(t, resp)
	})
}
