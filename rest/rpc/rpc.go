// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/swaggest/openapi-go/openapi3"
	"go.opentelemetry.io/otel"
)

// Handler represents a RPC style implementation of the core
// logic for your [http.Handler].
type Handler[Req, Resp any] interface {
	Handle(context.Context, *Req) (*Resp, error)
}

// HandlerFunc is an adapter to allow the use of ordinary functions
// as [Handler]s.
type HandlerFunc[Req, Resp any] func(context.Context, *Req) (*Resp, error)

// Handle implements the [Handler] interface.
func (f HandlerFunc[Req, Resp]) Handle(ctx context.Context, req *Req) (*Resp, error) {
	return f(ctx, req)
}

// OperationOptions are used for configuring a [Operation].
type OperationOptions struct {
	openapiDef openapi3.Operation
	errHandler ErrorHandler
}

// OperationOption sets a value on [OperationOptions].
type OperationOption interface {
	ApplyOperationOption(*OperationOptions)
}

type operationOptionFunc func(*OperationOptions)

func (f operationOptionFunc) ApplyOperationOption(oo *OperationOptions) {
	f(oo)
}

// RequestReader is meant to be implemented by any type which knows how
// unmarshal itself from a [http.Request].
type RequestReader[T any] interface {
	*T

	ReadRequest(context.Context, *http.Request) error
}

// TypedRequest is a [RequestReader] which also provides a OpenAPI 3.0
// spec for itself.
type TypedRequest[T any] interface {
	RequestReader[T]

	Spec() (*openapi3.RequestBody, error)
}

// ResponseWriter is meant to be implemented by any type which knows how
// to marshal itself into a HTTP response.
type ResponseWriter[T any] interface {
	*T

	WriteResponse(context.Context, http.ResponseWriter) error
}

// TypedResponse is a [ResponseWriter] which also provides a OpenAPI 3.0
// spec for itself.
type TypedResponse[T any] interface {
	ResponseWriter[T]

	Spec() (int, *openapi3.Response, error)
}

// Operation is a [http.Handler] which also provides a OpenAPI 3.0 spec for itself.
type Operation[I, O any, Req TypedRequest[I], Resp TypedResponse[O]] struct {
	openapiDef openapi3.Operation
	handler    Handler[I, O]
	errHandler ErrorHandler
}

// NewOperation initializes a [Operation].
func NewOperation[I, O any, Req TypedRequest[I], Resp TypedResponse[O]](h Handler[I, O], opts ...OperationOption) *Operation[I, O, Req, Resp] {
	oo := &OperationOptions{
		openapiDef: openapi3.Operation{
			Responses: openapi3.Responses{
				MapOfResponseOrRefValues: make(map[string]openapi3.ResponseOrRef),
			},
		},
		errHandler: ErrorHandlerFunc(func(w http.ResponseWriter, err error) {
			w.WriteHeader(http.StatusInternalServerError)
		}),
	}
	for _, opt := range opts {
		opt.ApplyOperationOption(oo)
	}
	return &Operation[I, O, Req, Resp]{
		openapiDef: oo.openapiDef,
		handler:    h,
		errHandler: oo.errHandler,
	}
}

// Spec implements the [rest.Operation] interface.
func (op *Operation[I, O, Req, Resp]) Spec() (openapi3.Operation, error) {
	openpapiDef := op.openapiDef

	var i I
	reqDef, err := Req(&i).Spec()
	if err != nil {
		return openpapiDef, err
	}

	openpapiDef.RequestBody = &openapi3.RequestBodyOrRef{
		RequestBody: reqDef,
	}

	var o O
	statusCode, respDef, err := Resp(&o).Spec()
	if err != nil {
		return openpapiDef, err
	}

	s := strconv.Itoa(statusCode)
	_, exists := openpapiDef.Responses.MapOfResponseOrRefValues[s]
	if exists {
		return openpapiDef, errors.New("response for status code is already defined")
	}

	openpapiDef.Responses.MapOfResponseOrRefValues[s] = openapi3.ResponseOrRef{
		Response: respDef,
	}
	return openpapiDef, nil
}

// ServeHTTP implements the [http.Handler] interface.
func (op *Operation[I, O, Req, Resp]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spanCtx, span := otel.Tracer("endpoint").Start(r.Context(), "Operation.ServeHTTP")
	defer span.End()

	var i I
	err := Req(&i).ReadRequest(spanCtx, r)
	if err != nil {
		op.errHandler.Handle(w, err)
		return
	}

	resp, err := op.handler.Handle(spanCtx, &i)
	if err != nil {
		op.errHandler.Handle(w, err)
		return
	}

	err = Resp(resp).WriteResponse(spanCtx, w)
	if err == nil {
		return
	}
	op.errHandler.Handle(w, err)
}
