// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
)

func TestJsonResponse_Spec(t *testing.T) {
	t.Run("will define simple flat structs", func(t *testing.T) {
		p := ProducerFunc[msgResponse](func(_ context.Context) (*msgResponse, error) {
			return nil, nil
		})

		h := ProduceJson(p)

		responses := h.Responses()

		spec := struct {
			Responses map[string]interface{} `json:"responses"`
		}{
			Responses: make(map[string]interface{}),
		}
		for k, v := range responses.MapOfResponseOrRefValues {
			spec.Responses[k] = v
		}

		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		err := enc.Encode(spec)
		if !assert.Nil(t, err) {
			return
		}

		var def struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Type       string `json:"type"`
						Properties map[string]struct {
							Type string `json:"type"`
						} `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		}
		dec := json.NewDecoder(&buf)
		err = dec.Decode(&def)
		if !assert.Nil(t, err) {
			return
		}

		decodedResponses := def.Responses
		if !assert.Len(t, decodedResponses, 1) {
			return
		}
		if !assert.Contains(t, decodedResponses, "200") {
			return
		}

		content := decodedResponses["200"].Content
		if !assert.Len(t, content, 1) {
			return
		}
		if !assert.Contains(t, content, "application/json") {
			return
		}

		schema := content["application/json"].Schema
		if !assert.Equal(t, "object", schema.Type) {
			return
		}

		props := schema.Properties
		if !assert.Len(t, props, 1) {
			return
		}
		if !assert.Contains(t, props, "msg") {
			return
		}
		if !assert.Equal(t, "string", props["msg"].Type) {
			return
		}
	})

	t.Run("will not use references for struct fields of non-primitive types", func(t *testing.T) {
		type outerType[T any] struct {
			Inner T `json:"inner"`
		}

		h := ReturnJson(HandlerFunc[emptyRequest, outerType[msgResponse]](func(_ context.Context, _ *emptyRequest) (*outerType[msgResponse], error) {
			return nil, nil
		}))

		responses := h.Responses()

		spec := struct {
			Responses map[string]interface{} `json:"responses"`
		}{
			Responses: make(map[string]interface{}),
		}
		for k, v := range responses.MapOfResponseOrRefValues {
			spec.Responses[k] = v
		}

		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		err := enc.Encode(spec)
		if !assert.Nil(t, err) {
			return
		}

		type Schema struct {
			Type       string            `json:"type"`
			Properties map[string]Schema `json:"properties"`
		}

		var def struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema Schema `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		}
		dec := json.NewDecoder(&buf)
		err = dec.Decode(&def)
		if !assert.Nil(t, err) {
			return
		}

		decodedResponses := def.Responses
		if !assert.Len(t, decodedResponses, 1) {
			return
		}
		if !assert.Contains(t, decodedResponses, "200") {
			return
		}

		content := decodedResponses["200"].Content
		if !assert.Len(t, content, 1) {
			return
		}
		if !assert.Contains(t, content, "application/json") {
			return
		}

		schema := content["application/json"].Schema
		if !assert.Equal(t, "object", schema.Type) {
			return
		}

		props := schema.Properties
		if !assert.Len(t, props, 1) {
			return
		}
		if !assert.Contains(t, props, "inner") {
			return
		}

		innerProp := props["inner"]
		if !assert.Equal(t, "object", innerProp.Type) {
			return
		}

		props = innerProp.Properties
		if !assert.Len(t, props, 1) {
			return
		}
		if !assert.Contains(t, props, "msg") {
			return
		}
		if !assert.Equal(t, "string", props["msg"].Type) {
			return
		}
	})
}

func TestJsonRequest_ReadRequest(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if the request content type is not application/json", func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://example.com", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "text/plain")

			jr := JsonRequest[msgRequest]{
				tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
			}
			err := jr.ReadRequest(context.Background(), r)

			var ierr InvalidContentTypeError
			if !assert.ErrorAs(t, err, &ierr) {
				return
			}
			if !assert.NotEmpty(t, ierr.Error()) {
				return
			}
			if !assert.Equal(t, "text/plain", ierr.ContentType) {
				return
			}
		})
	})
}

func TestJsonRequest_Spec(t *testing.T) {
	t.Run("will define simple flat structs", func(t *testing.T) {
		c := ConsumerFunc[msgRequest](func(_ context.Context, _ *msgRequest) error {
			return nil
		})

		h := ConsumeOnlyJson(c)

		reqBody := h.RequestBody()

		spec := struct {
			RequestBody interface{} `json:"requestBody"`
		}{
			RequestBody: reqBody,
		}

		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		err := enc.Encode(spec)
		if !assert.Nil(t, err) {
			return
		}

		type Schema struct {
			Type       string            `json:"type"`
			Properties map[string]Schema `json:"properties"`
		}

		var def struct {
			RequestBody struct {
				Content map[string]struct {
					Schema Schema `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		}
		dec := json.NewDecoder(&buf)
		err = dec.Decode(&def)
		if !assert.Nil(t, err) {
			return
		}

		content := def.RequestBody.Content
		if !assert.Len(t, content, 1) {
			return
		}
		if !assert.Contains(t, content, "application/json") {
			return
		}

		schema := content["application/json"].Schema
		if !assert.Equal(t, "object", schema.Type) {
			return
		}

		props := schema.Properties
		if !assert.Len(t, props, 1) {
			return
		}
		if !assert.Contains(t, props, "msg") {
			return
		}
		if !assert.Equal(t, "string", props["msg"].Type) {
			return
		}
	})

	t.Run("will not use references for struct fields of non-primitive types", func(t *testing.T) {
		type outerType[T any] struct {
			Inner T `json:"inner"`
		}

		h := ConsumeJson(HandlerFunc[outerType[msgRequest], emptyResponse](func(_ context.Context, _ *outerType[msgRequest]) (*emptyResponse, error) {
			return &emptyResponse{tracer: otel.Tracer("rpc")}, nil
		}))

		reqBody := h.RequestBody()

		spec := struct {
			RequestBody interface{} `json:"requestBody"`
		}{
			RequestBody: reqBody,
		}

		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		err := enc.Encode(spec)
		if !assert.Nil(t, err) {
			return
		}

		type Schema struct {
			Type       string            `json:"type"`
			Properties map[string]Schema `json:"properties"`
		}

		var def struct {
			RequestBody struct {
				Content map[string]struct {
					Schema Schema `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		}
		dec := json.NewDecoder(&buf)
		err = dec.Decode(&def)
		if !assert.Nil(t, err) {
			return
		}

		content := def.RequestBody.Content
		if !assert.Len(t, content, 1) {
			return
		}
		if !assert.Contains(t, content, "application/json") {
			return
		}

		schema := content["application/json"].Schema
		if !assert.Equal(t, "object", schema.Type) {
			return
		}

		props := schema.Properties
		if !assert.Len(t, props, 1) {
			return
		}
		if !assert.Contains(t, props, "inner") {
			return
		}

		innerProp := props["inner"]
		if !assert.Equal(t, "object", innerProp.Type) {
			return
		}

		props = innerProp.Properties
		if !assert.Len(t, props, 1) {
			return
		}
		if !assert.Contains(t, props, "msg") {
			return
		}
		if !assert.Equal(t, "string", props["msg"].Type) {
			return
		}
	})
}

func TestReturnJsonHandler_RequestBody(t *testing.T) {
	t.Run("will return empty request body", func(t *testing.T) {
		h := HandlerFunc[string, string](func(ctx context.Context, req *string) (*string, error) {
			return req, nil
		})

		rjh := ReturnJson(h)

		reqBody := rjh.RequestBody()

		assert.Nil(t, reqBody.RequestBody)
	})
}

func TestReturnJsonHandler_ServeHTTP(t *testing.T) {
	t.Run("will panic when Handle returns error", func(t *testing.T) {
		expectedErr := errors.New("handle error")
		h := HandlerFunc[string, string](func(ctx context.Context, req *string) (*string, error) {
			return nil, expectedErr
		})

		rjh := ReturnJson(h)

		r := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
		w := httptest.NewRecorder()

		assert.Panics(t, func() {
			rjh.ServeHTTP(w, r)
		})
	})
}

func TestConsumeJsonHandler_Responses(t *testing.T) {
	t.Run("will return empty responses", func(t *testing.T) {
		h := HandlerFunc[string, string](func(ctx context.Context, req *string) (*string, error) {
			return req, nil
		})

		cjh := ConsumeJson(h)

		responses := cjh.Responses()

		assert.Empty(t, responses.MapOfResponseOrRefValues)
	})
}

func TestConsumeJsonHandler_ServeHTTP(t *testing.T) {
	t.Run("will panic when ReadRequest fails", func(t *testing.T) {
		h := HandlerFunc[msgRequest, string](func(ctx context.Context, req *msgRequest) (*string, error) {
			resp := "ok"
			return &resp, nil
		})

		cjh := ConsumeJson(h)

		// Invalid content type will cause ReadRequest to fail
		r := httptest.NewRequest(http.MethodPost, "http://example.com", strings.NewReader(`{"msg":"test"}`))
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()

		assert.Panics(t, func() {
			cjh.ServeHTTP(w, r)
		})
	})

	t.Run("will panic when Handle returns error", func(t *testing.T) {
		expectedErr := errors.New("handle error")
		h := HandlerFunc[msgRequest, string](func(ctx context.Context, req *msgRequest) (*string, error) {
			return nil, expectedErr
		})

		cjh := ConsumeJson(h)

		r := httptest.NewRequest(http.MethodPost, "http://example.com", strings.NewReader(`{"msg":"test"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		assert.Panics(t, func() {
			cjh.ServeHTTP(w, r)
		})
	})
}

func TestHandleJson(t *testing.T) {
	t.Run("will compose ConsumeJson and ReturnJson", func(t *testing.T) {
		h := HandlerFunc[msgRequest, msgResponse](func(ctx context.Context, req *msgRequest) (*msgResponse, error) {
			return &msgResponse{Msg: "response: " + req.Msg}, nil
		})

		hjh := HandleJson(h)

		// Verify it returns a ConsumeJsonHandler
		assert.NotNil(t, hjh)

		// Check request body spec is present
		reqBody := hjh.RequestBody()
		assert.NotNil(t, reqBody.RequestBody)
		assert.NotNil(t, reqBody.RequestBody.Content)
		assert.Contains(t, reqBody.RequestBody.Content, "application/json")
	})
}

func TestProduceJson_Integration(t *testing.T) {
	t.Run("will produce JSON response for GET endpoint", func(t *testing.T) {
		p := ProducerFunc[msgResponse](func(ctx context.Context) (*msgResponse, error) {
			return &msgResponse{Msg: "hello"}, nil
		})

		h := ProduceJson(p)

		r := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, r)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

		var resp msgResponse
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		assert.Nil(t, err)
		assert.Equal(t, "hello", resp.Msg)
	})

	t.Run("will panic when producer returns error", func(t *testing.T) {
		expectedErr := errors.New("producer error")
		p := ProducerFunc[msgResponse](func(ctx context.Context) (*msgResponse, error) {
			return nil, expectedErr
		})

		h := ProduceJson(p)

		r := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
		w := httptest.NewRecorder()

		assert.Panics(t, func() {
			h.ServeHTTP(w, r)
		})
	})
}

func TestConsumeOnlyJson_Integration(t *testing.T) {
	t.Run("will consume JSON request without response body", func(t *testing.T) {
		receivedMsg := ""
		c := ConsumerFunc[msgRequest](func(ctx context.Context, req *msgRequest) error {
			receivedMsg = req.Msg
			return nil
		})

		h := ConsumeOnlyJson(c)

		r := httptest.NewRequest(http.MethodPost, "http://example.com", strings.NewReader(`{"msg":"test"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		h.ServeHTTP(w, r)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "test", receivedMsg)
	})

	t.Run("will panic on invalid content type", func(t *testing.T) {
		c := ConsumerFunc[msgRequest](func(ctx context.Context, req *msgRequest) error {
			return nil
		})

		h := ConsumeOnlyJson(c)

		r := httptest.NewRequest(http.MethodPost, "http://example.com", strings.NewReader(`{"msg":"test"}`))
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()

		assert.Panics(t, func() {
			h.ServeHTTP(w, r)
		})
	})

	t.Run("will panic on malformed JSON", func(t *testing.T) {
		c := ConsumerFunc[msgRequest](func(ctx context.Context, req *msgRequest) error {
			return nil
		})

		h := ConsumeOnlyJson(c)

		r := httptest.NewRequest(http.MethodPost, "http://example.com", strings.NewReader(`{invalid json`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		assert.Panics(t, func() {
			h.ServeHTTP(w, r)
		})
	})
}
