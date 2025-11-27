// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"net/http"

	"github.com/swaggest/openapi-go/openapi3"
)

// Consumer consumes a request value without returning a response value.
type Consumer[T any] interface {
	Consume(context.Context, *T) error
}

// ConsumerFunc is an adapter to allow the use of ordinary functions
// as [Consumer]s.
type ConsumerFunc[T any] func(context.Context, *T) error

// Consume implements the [Consumer] interface.
func (c ConsumerFunc[T]) Consume(ctx context.Context, req *T) error {
	return c(ctx, req)
}

// consumerHandler is a [Handler] that returns a HTTP status code
// along with a empty response body.
//
// This is a very handy helper for implementing HTTP POST or PUT webhook
// style endpoints that just consume a payload and return a status code.
type ConsumerHandler[T any] struct {
	c Consumer[T]
}

func ProduceNothing[T any](c Consumer[T]) *ConsumerHandler[T] {
	return &ConsumerHandler[T]{
		c: c,
	}
}

// emptyResponse is a [TypedResponse] for a empty response body.
type EmptyResponse struct{}

// WriteResponse implements the [ResponseWriter] interface.
func (er *EmptyResponse) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	w.WriteHeader(http.StatusOK)
	return nil
}

// Spec implements the [TypedResponse] interface.
func (*EmptyResponse) Spec() (int, openapi3.ResponseOrRef, error) {
	return http.StatusOK, openapi3.ResponseOrRef{}, nil
}

// Handle implements the [Handler] interface.
func (h *ConsumerHandler[T]) Handle(ctx context.Context, req *T) (*EmptyResponse, error) {
	err := h.c.Consume(ctx, req)
	if err != nil {
		return nil, err
	}
	return &EmptyResponse{}, nil
}

// Producer returns a response value without consuming a request value.
type Producer[T any] interface {
	Produce(context.Context) (*T, error)
}

// ProducerFunc is an adapter to allow the use of ordinary functions
// as [Producer]s.
type ProducerFunc[T any] func(context.Context) (*T, error)

// Produce implements the [Producer] interface.
func (f ProducerFunc[T]) Produce(ctx context.Context) (*T, error) {
	return f(ctx)
}

// producerHandler is a [Handler] that does not consume a request body.
//
// This is a very handy helper for implementing HTTP GET endpoints.
type ProducerHandler[T any] struct {
	p Producer[T]
}

func ConsumeNothing[T any](p Producer[T]) *ProducerHandler[T] {
	return &ProducerHandler[T]{
		p: p,
	}
}

// emptyRequest is a [TypedRequest] for a empty request body.
type EmptyRequest struct{}

// ReadRequest implements the [RequestReader] interface.
func (*EmptyRequest) ReadRequest(ctx context.Context, r *http.Request) error {
	return nil
}

// Spec implements the [TypedRequest] interface.
func (*EmptyRequest) Spec() (openapi3.RequestBodyOrRef, error) {
	return openapi3.RequestBodyOrRef{}, nil
}

// Handle implements the [Handler] interface.
func (h *ProducerHandler[T]) Handle(ctx context.Context, req *EmptyRequest) (*T, error) {
	return h.p.Produce(ctx)
}
