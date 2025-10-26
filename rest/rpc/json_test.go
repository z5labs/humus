// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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

		h := ReturnJson(HandlerFunc[EmptyRequest, outerType[msgResponse]](func(_ context.Context, _ *EmptyRequest) (*outerType[msgResponse], error) {
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

			var jr JsonRequest[msgRequest]
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

		h := ConsumeJson(HandlerFunc[outerType[msgRequest], EmptyResponse](func(_ context.Context, _ *outerType[msgRequest]) (*EmptyResponse, error) {
			return &EmptyResponse{}, nil
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
