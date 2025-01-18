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

type requestInjector interface {
	Inject(context.Context, *http.Request) context.Context
}

type requestInjectorFunc func(context.Context, *http.Request) context.Context

func (f requestInjectorFunc) Inject(ctx context.Context, r *http.Request) context.Context {
	return f(ctx, r)
}

type requestValidator interface {
	ValidateRequest(context.Context, *http.Request) error
}

type requestValidatorFunc func(context.Context, *http.Request) error

func (f requestValidatorFunc) ValidateRequest(ctx context.Context, r *http.Request) error {
	return f(ctx, r)
}

// OperationOptions
type OperationOptions struct {
	openapiDef openapi3.Operation
	errHandler ErrorHandler
	injectors  []requestInjector
	validators []requestValidator
}

// OperationOption
type OperationOption interface {
	ApplyOperationOption(*OperationOptions)
}

type operationOptionFunc func(*OperationOptions)

func (f operationOptionFunc) ApplyOperationOption(oo *OperationOptions) {
	f(oo)
}

// ParameterOptions
type ParameterOptions struct {
	name        string
	description string
	required    bool
	validator   requestValidator
}

// ParameterOption
type ParameterOption interface {
	ApplyParameterOption(*ParameterOptions)
}

type parameterOptionFunc func(*ParameterOptions)

func (f parameterOptionFunc) ApplyParameterOption(po *ParameterOptions) {
	f(po)
}

// Description
func Description(s string) ParameterOption {
	return parameterOptionFunc(func(po *ParameterOptions) {
		po.description = s
	})
}

func parameter(name string, in openapi3.ParameterIn, f requestInjectorFunc, opts ...ParameterOption) operationOptionFunc {
	po := ParameterOptions{
		name: name,
	}
	for _, opt := range opts {
		opt.ApplyParameterOption(&po)
	}

	return func(oo *OperationOptions) {
		oo.openapiDef.Parameters = append(oo.openapiDef.Parameters, openapi3.ParameterOrRef{
			Parameter: &openapi3.Parameter{
				Name:        name,
				Description: &po.description,
				In:          in,
				Required:    &po.required,
			},
		})

		oo.validators = append(oo.validators, po.validator)
		oo.injectors = append(oo.injectors, f)
	}
}

type parameterCtxKey string

// ParamFromContext
func ParamFromContext(ctx context.Context, name string) (string, bool) {
	v := ctx.Value(parameterCtxKey(name))
	if v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// ParamValidator
type ParamValidator interface {
	ValidateParam(context.Context, string) error
}

// ValidateHeader
func ValidateHeader(hv ParamValidator) ParameterOption {
	return parameterOptionFunc(func(po *ParameterOptions) {
		po.validator = requestValidatorFunc(func(ctx context.Context, r *http.Request) error {
			v := r.Header.Get(po.name)
			return hv.ValidateParam(ctx, v)
		})
	})
}

// Header
func Header(name string, opts ...ParameterOption) OperationOption {
	return parameter(name, openapi3.ParameterInHeader, func(ctx context.Context, r *http.Request) context.Context {
		v := r.Header.Get(name)
		ctx = context.WithValue(ctx, parameterCtxKey(name), v)
		return ctx
	}, opts...)
}

// QueryParam
func QueryParam(name string, opts ...ParameterOption) OperationOption {
	return parameter(name, openapi3.ParameterInQuery, func(ctx context.Context, r *http.Request) context.Context {
		v := r.URL.Query().Get(name)
		ctx = context.WithValue(ctx, parameterCtxKey(name), v)
		return ctx
	}, opts...)
}

// PathParam
func PathParam(name string, opts ...ParameterOption) OperationOption {
	return parameter(name, openapi3.ParameterInPath, func(ctx context.Context, r *http.Request) context.Context {
		v := r.PathValue(name)
		ctx = context.WithValue(ctx, parameterCtxKey(name), v)
		return ctx
	}, opts...)
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

// Definition implements the [rest.Operation] interface.
func (op *Operation[I, O, Req, Resp]) Definition() (openapi3.Operation, error) {
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
