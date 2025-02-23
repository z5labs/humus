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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/swaggest/openapi-go/openapi3"
)

type failRequestReader struct{}

var errFailedToReadRequest = errors.New("failed to read request")

func (failRequestReader) ReadRequest(ctx context.Context, r *http.Request) error {
	return errFailedToReadRequest
}

func (failRequestReader) Spec() (*openapi3.RequestBody, error) {
	return &openapi3.RequestBody{}, nil
}

type failResponseWriter struct{}

var errFailedToWriteResponse = errors.New("failed to write response")

func (failResponseWriter) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	return errFailedToWriteResponse
}

func (failResponseWriter) Spec() (int, *openapi3.Response, error) {
	return http.StatusInternalServerError, &openapi3.Response{}, nil
}

func TestOperation_ServeHTTP(t *testing.T) {
	t.Run("will handle error", func(t *testing.T) {
		t.Run("if the request fails to be read", func(t *testing.T) {
			h := ReturnNothing(ConsumerFunc[failRequestReader](func(_ context.Context, _ *failRequestReader) error {
				return nil
			}))

			var caughtErr error
			op := NewOperation(
				h,
				OnError(ErrorHandlerFunc(func(w http.ResponseWriter, err error) {
					caughtErr = err
				})),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://example.com", strings.NewReader(``))

			op.ServeHTTP(w, req)

			if !assert.ErrorIs(t, caughtErr, errFailedToReadRequest) {
				return
			}
		})

		t.Run("if the underlying Handler fails", func(t *testing.T) {
			handleErr := errors.New("failed to handle request")
			h := HandlerFunc[EmptyRequest, EmptyResponse](func(_ context.Context, _ *EmptyRequest) (*EmptyResponse, error) {
				return nil, handleErr
			})

			var caughtErr error
			op := NewOperation(
				h,
				OnError(ErrorHandlerFunc(func(w http.ResponseWriter, err error) {
					caughtErr = err
				})),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://example.com", strings.NewReader(``))

			op.ServeHTTP(w, req)

			if !assert.ErrorIs(t, caughtErr, handleErr) {
				return
			}
		})

		t.Run("if the response fails to be written", func(t *testing.T) {
			h := ConsumeNothing(ProducerFunc[failResponseWriter](func(_ context.Context) (*failResponseWriter, error) {
				return &failResponseWriter{}, nil
			}))

			var caughtErr error
			op := NewOperation(
				h,
				OnError(ErrorHandlerFunc(func(w http.ResponseWriter, err error) {
					caughtErr = err
				})),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)

			op.ServeHTTP(w, req)

			if !assert.ErrorIs(t, caughtErr, errFailedToWriteResponse) {
				return
			}
		})
	})
}
