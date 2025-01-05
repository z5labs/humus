// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package rpc helps users implement [http.Handler]s using a RPC style interface.
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

// OperationOptions
type OperationOptions struct {
	openapiDef openapi3.Operation
	errHandler ErrorHandler
}

// OperationOption
type OperationOption interface {
	ApplyOperationOption(*OperationOptions)
}

// RequestReader
type RequestReader[T any] interface {
	*T

	ReadRequest(context.Context, *http.Request) error
}

// TypedRequest
type TypedRequest[T any] interface {
	RequestReader[T]

	Type() (*openapi3.RequestBody, error)
}

// ResponseWriter
type ResponseWriter[T any] interface {
	*T

	WriteResponse(context.Context, http.ResponseWriter) error
}

// TypedResponse
type TypedResponse[T any] interface {
	ResponseWriter[T]

	Type() (int, *openapi3.Response, error)
}

// Operation
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

// Operation implements the [rest.Operation] interface.
func (op *Operation[I, O, Req, Resp]) Operation() (openapi3.Operation, error) {
	openpapiDef := op.openapiDef

	var i I
	reqDef, err := Req(&i).Type()
	if err != nil {
		return openpapiDef, err
	}

	openpapiDef.RequestBody = &openapi3.RequestBodyOrRef{
		RequestBody: reqDef,
	}

	var o O
	statusCode, respDef, err := Resp(&o).Type()
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
