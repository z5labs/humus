// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"net/http"
	"strconv"

	"github.com/swaggest/openapi-go/openapi3"
	"go.opentelemetry.io/otel"
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

// ConsumerHandler is a [Handler] that returns a HTTP status code
// along with a empty response body.
//
// This is a very handy helper for implementing HTTP POST or PUT webhook
// style endpoints that just consume a payload and return a status code.
type ConsumerHandler[T any] struct {
	c Consumer[T]
}

// ReturnNothing initializes a [ConsumerHandler] given a [Consumer].
func ReturnNothing[T any](c Consumer[T]) *ConsumerHandler[T] {
	return &ConsumerHandler[T]{
		c: c,
	}
}

// EmptyResponse is a [TypedResponse] for a empty response body.
type EmptyResponse struct{}

// Spec implements the [TypedResponse] interface.
func (*EmptyResponse) Spec() (int, *openapi3.Response, error) {
	return http.StatusOK, &openapi3.Response{}, nil
}

// WriteResponse implements the [ResponseWriter] interface.
func (*EmptyResponse) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	_, span := otel.Tracer("rpc").Start(ctx, "EmptyResponse.WriteResponse")
	defer span.End()

	w.WriteHeader(http.StatusOK)
	return nil
}

// Handle implements the [Handler] interface.
func (h *ConsumerHandler[T]) Handle(ctx context.Context, req *T) (*EmptyResponse, error) {
	spanCtx, span := otel.Tracer("rpc").Start(ctx, "ConsumerHandler.Handle")
	defer span.End()

	err := h.c.Consume(spanCtx, req)
	if err != nil {
		return nil, err
	}
	return &EmptyResponse{}, nil
}

// RequestBody implements the rest.Handler interface.
func (h *ConsumerHandler[T]) RequestBody() openapi3.RequestBodyOrRef {
	return openapi3.RequestBodyOrRef{}
}

// Responses implements the rest.Handler interface.
func (h *ConsumerHandler[T]) Responses() openapi3.Responses {
	var resp EmptyResponse
	statusCode, responseDef, err := resp.Spec()
	if err != nil {
		// Return empty responses if spec generation fails
		return openapi3.Responses{}
	}

	return openapi3.Responses{
		MapOfResponseOrRefValues: map[string]openapi3.ResponseOrRef{
			strconv.Itoa(statusCode): {
				Response: responseDef,
			},
		},
	}
}

// ServeHTTP implements the [http.Handler] interface.
func (h *ConsumerHandler[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spanCtx, span := otel.Tracer("rpc").Start(r.Context(), "ConsumerHandler.ServeHTTP")
	defer span.End()

	var req T
	// If T implements RequestReader, call ReadRequest
	if rr, ok := any(&req).(interface{ ReadRequest(context.Context, *http.Request) error }); ok {
		err := rr.ReadRequest(spanCtx, r)
		if err != nil {
			span.RecordError(err)
			panic(err)
		}
	}

	resp, err := h.Handle(spanCtx, &req)
	if err != nil {
		span.RecordError(err)
		panic(err)
	}

	err = resp.WriteResponse(spanCtx, w)
	if err != nil {
		span.RecordError(err)
		panic(err)
	}
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

// ProducerHandler is a [Handler] that does not consume a request body.
//
// This is a very handy helper for implementing HTTP GET endpoints.
type ProducerHandler[T any] struct {
	p Producer[T]
}

// ConsumeNothing initializes a [ProducerHandler] given a [Producer].
func ConsumeNothing[T any](p Producer[T]) *ProducerHandler[T] {
	return &ProducerHandler[T]{
		p: p,
	}
}

// EmptyRequest is a [TypedRequest] for a empty request body.
type EmptyRequest struct{}

// Spec implements the [TypedRequest] interface.
func (*EmptyRequest) Spec() (*openapi3.RequestBody, error) {
	return &openapi3.RequestBody{}, nil
}

// ReadRequest implements the [RequestReader] interface.
func (*EmptyRequest) ReadRequest(ctx context.Context, r *http.Request) error {
	return nil
}

// Handle implements the [Handler] interface.
func (h *ProducerHandler[T]) Handle(ctx context.Context, req *EmptyRequest) (*T, error) {
	spanCtx, span := otel.Tracer("rpc").Start(ctx, "ProducerHandler.Handle")
	defer span.End()

	return h.p.Produce(spanCtx)
}

// RequestBody implements the rest.Handler interface.
func (h *ProducerHandler[T]) RequestBody() openapi3.RequestBodyOrRef {
	var req EmptyRequest
	reqBody, err := req.Spec()
	if err != nil {
		// Return empty request body if spec generation fails
		return openapi3.RequestBodyOrRef{}
	}

	return openapi3.RequestBodyOrRef{
		RequestBody: reqBody,
	}
}

// Responses implements the rest.Handler interface.
func (h *ProducerHandler[T]) Responses() openapi3.Responses {
	return openapi3.Responses{}
}

// ServeHTTP implements the [http.Handler] interface.
func (h *ProducerHandler[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spanCtx, span := otel.Tracer("rpc").Start(r.Context(), "ProducerHandler.ServeHTTP")
	defer span.End()

	var req EmptyRequest
	err := req.ReadRequest(spanCtx, r)
	if err != nil {
		span.RecordError(err)
		panic(err)
	}

	resp, err := h.Handle(spanCtx, &req)
	if err != nil {
		span.RecordError(err)
		panic(err)
	}

	// The response is of type *T, which could be a TypedResponse
	// If T implements ResponseWriter, call WriteResponse
	if rw, ok := any(resp).(interface{ WriteResponse(context.Context, http.ResponseWriter) error }); ok {
		err = rw.WriteResponse(spanCtx, w)
		if err != nil {
			span.RecordError(err)
			panic(err)
		}
	}
}
