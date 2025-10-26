// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"net/http"

	"github.com/swaggest/openapi-go/openapi3"
	"go.opentelemetry.io/otel/trace"
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
type consumerHandler[T any] struct {
	c      Consumer[T]
	tracer trace.Tracer
}

// emptyResponse is a [ResponseWriter] for a empty response body.
type emptyResponse struct {
	tracer trace.Tracer
}

// WriteResponse implements the [ResponseWriter] interface.
func (er *emptyResponse) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	_, span := er.tracer.Start(ctx, "emptyResponse.WriteResponse")
	defer span.End()

	w.WriteHeader(http.StatusOK)
	return nil
}

// Handle implements the [Handler] interface.
func (h *consumerHandler[T]) Handle(ctx context.Context, req *T) (*emptyResponse, error) {
	spanCtx, span := h.tracer.Start(ctx, "consumerHandler.Handle")
	defer span.End()

	err := h.c.Consume(spanCtx, req)
	if err != nil {
		return nil, err
	}
	return &emptyResponse{tracer: h.tracer}, nil
}

// RequestBody implements the rest.Handler interface.
func (h *consumerHandler[T]) RequestBody() openapi3.RequestBodyOrRef {
	return openapi3.RequestBodyOrRef{}
}

// Responses implements the rest.Handler interface.
func (h *consumerHandler[T]) Responses() openapi3.Responses {
	return openapi3.Responses{
		MapOfResponseOrRefValues: map[string]openapi3.ResponseOrRef{
			"200": {
				Response: &openapi3.Response{},
			},
		},
	}
}

// ServeHTTP implements the [http.Handler] interface.
func (h *consumerHandler[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spanCtx, span := h.tracer.Start(r.Context(), "consumerHandler.ServeHTTP")
	defer span.End()

	var req T
	// If T implements RequestReader, call ReadRequest
	if rr, ok := any(&req).(interface {
		ReadRequest(context.Context, *http.Request) error
	}); ok {
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

// producerHandler is a [Handler] that does not consume a request body.
//
// This is a very handy helper for implementing HTTP GET endpoints.
type producerHandler[T any] struct {
	p      Producer[T]
	tracer trace.Tracer
}

// emptyRequest is a [RequestReader] for a empty request body.
type emptyRequest struct{}

// ReadRequest implements the [RequestReader] interface.
func (*emptyRequest) ReadRequest(ctx context.Context, r *http.Request) error {
	return nil
}

// Handle implements the [Handler] interface.
func (h *producerHandler[T]) Handle(ctx context.Context, req *emptyRequest) (*T, error) {
	spanCtx, span := h.tracer.Start(ctx, "producerHandler.Handle")
	defer span.End()

	return h.p.Produce(spanCtx)
}

// RequestBody implements the rest.Handler interface.
func (h *producerHandler[T]) RequestBody() openapi3.RequestBodyOrRef {
	return openapi3.RequestBodyOrRef{}
}

// Responses implements the rest.Handler interface.
func (h *producerHandler[T]) Responses() openapi3.Responses {
	return openapi3.Responses{}
}

// ServeHTTP implements the [http.Handler] interface.
func (h *producerHandler[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spanCtx, span := h.tracer.Start(r.Context(), "producerHandler.ServeHTTP")
	defer span.End()

	var req emptyRequest
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
	if rw, ok := any(resp).(interface {
		WriteResponse(context.Context, http.ResponseWriter) error
	}); ok {
		err = rw.WriteResponse(spanCtx, w)
		if err != nil {
			span.RecordError(err)
			panic(err)
		}
	}
}
