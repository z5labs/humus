// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
)

func TestEmptyRequest_ReadRequest(t *testing.T) {
	t.Run("will always return nil", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.com", nil)

		var er emptyRequest
		err := er.ReadRequest(context.Background(), r)

		assert.Nil(t, err)
	})
}

func TestEmptyResponse_WriteResponse(t *testing.T) {
	t.Run("will write HTTP 200 status", func(t *testing.T) {
		w := httptest.NewRecorder()

		er := emptyResponse{
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}
		err := er.WriteResponse(context.Background(), w)

		assert.Nil(t, err)
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestConsumerHandler_Handle(t *testing.T) {
	t.Run("will return empty response on success", func(t *testing.T) {
		called := false
		c := ConsumerFunc[string](func(ctx context.Context, req *string) error {
			called = true
			assert.Equal(t, "test", *req)
			return nil
		})

		h := &consumerHandler[string]{
			c:      c,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		req := "test"
		resp, err := h.Handle(context.Background(), &req)

		assert.True(t, called)
		assert.Nil(t, err)
		assert.NotNil(t, resp)
	})

	t.Run("will return error when consumer fails", func(t *testing.T) {
		expectedErr := errors.New("consumer error")
		c := ConsumerFunc[string](func(ctx context.Context, req *string) error {
			return expectedErr
		})

		h := &consumerHandler[string]{
			c:      c,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		req := "test"
		resp, err := h.Handle(context.Background(), &req)

		assert.Nil(t, resp)
		assert.Equal(t, expectedErr, err)
	})
}

func TestConsumerHandler_RequestBody(t *testing.T) {
	t.Run("will return empty request body", func(t *testing.T) {
		c := ConsumerFunc[string](func(ctx context.Context, req *string) error {
			return nil
		})

		h := &consumerHandler[string]{
			c:      c,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		reqBody := h.RequestBody()

		assert.Nil(t, reqBody.RequestBody)
	})
}

func TestConsumerHandler_Responses(t *testing.T) {
	t.Run("will return 200 status code in responses", func(t *testing.T) {
		c := ConsumerFunc[string](func(ctx context.Context, req *string) error {
			return nil
		})

		h := &consumerHandler[string]{
			c:      c,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		responses := h.Responses()

		assert.NotNil(t, responses.MapOfResponseOrRefValues)
		assert.Contains(t, responses.MapOfResponseOrRefValues, "200")
	})
}

func TestConsumerHandler_ServeHTTP(t *testing.T) {
	t.Run("will handle request successfully", func(t *testing.T) {
		called := false
		c := ConsumerFunc[emptyRequest](func(ctx context.Context, req *emptyRequest) error {
			called = true
			return nil
		})

		h := &consumerHandler[emptyRequest]{
			c:      c,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		r := httptest.NewRequest(http.MethodPost, "http://example.com", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, r)

		assert.True(t, called)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("will panic when consumer returns error", func(t *testing.T) {
		expectedErr := errors.New("consumer error")
		c := ConsumerFunc[emptyRequest](func(ctx context.Context, req *emptyRequest) error {
			return expectedErr
		})

		h := &consumerHandler[emptyRequest]{
			c:      c,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		r := httptest.NewRequest(http.MethodPost, "http://example.com", nil)
		w := httptest.NewRecorder()

		assert.Panics(t, func() {
			h.ServeHTTP(w, r)
		})
	})
}

func TestProducerHandler_Handle(t *testing.T) {
	t.Run("will return response on success", func(t *testing.T) {
		expectedResp := "test response"
		p := ProducerFunc[string](func(ctx context.Context) (*string, error) {
			return &expectedResp, nil
		})

		h := &producerHandler[string]{
			p:      p,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		var req emptyRequest
		resp, err := h.Handle(context.Background(), &req)

		assert.Nil(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, expectedResp, *resp)
	})

	t.Run("will return error when producer fails", func(t *testing.T) {
		expectedErr := errors.New("producer error")
		p := ProducerFunc[string](func(ctx context.Context) (*string, error) {
			return nil, expectedErr
		})

		h := &producerHandler[string]{
			p:      p,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		var req emptyRequest
		resp, err := h.Handle(context.Background(), &req)

		assert.Nil(t, resp)
		assert.Equal(t, expectedErr, err)
	})
}

func TestProducerHandler_RequestBody(t *testing.T) {
	t.Run("will return empty request body", func(t *testing.T) {
		p := ProducerFunc[string](func(ctx context.Context) (*string, error) {
			return nil, nil
		})

		h := &producerHandler[string]{
			p:      p,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		reqBody := h.RequestBody()

		assert.Nil(t, reqBody.RequestBody)
	})
}

func TestProducerHandler_Responses(t *testing.T) {
	t.Run("will return empty responses", func(t *testing.T) {
		p := ProducerFunc[string](func(ctx context.Context) (*string, error) {
			return nil, nil
		})

		h := &producerHandler[string]{
			p:      p,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		responses := h.Responses()

		assert.Empty(t, responses.MapOfResponseOrRefValues)
	})
}

func TestProducerHandler_ServeHTTP(t *testing.T) {
	t.Run("will handle request successfully with ResponseWriter type", func(t *testing.T) {
		called := false
		resp := &msgResponse{Msg: "test"}
		p := ProducerFunc[msgResponse](func(ctx context.Context) (*msgResponse, error) {
			called = true
			return resp, nil
		})

		h := &producerHandler[msgResponse]{
			p:      p,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		r := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
		w := httptest.NewRecorder()

		h.ServeHTTP(w, r)

		assert.True(t, called)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("will panic when producer returns error", func(t *testing.T) {
		expectedErr := errors.New("producer error")
		p := ProducerFunc[msgResponse](func(ctx context.Context) (*msgResponse, error) {
			return nil, expectedErr
		})

		h := &producerHandler[msgResponse]{
			p:      p,
			tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
		}

		r := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
		w := httptest.NewRecorder()

		assert.Panics(t, func() {
			h.ServeHTTP(w, r)
		})
	})
}
