// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"net/http"
	"strconv"

	"github.com/z5labs/humus"

	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/sdk-go/try"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type securityScheme struct {
	name   string
	scheme openapi3.SecurityScheme
}

// OperationOptions holds configuration for an HTTP operation registered with [Handle].
// This includes security schemes, parameter definitions, request transformations,
// and error handling.
type OperationOptions struct {
	securityScheme *securityScheme
	parameters     []openapi3.ParameterOrRef
	transforms     []func(*http.Request) (*http.Request, error)
	errHandler     ErrorHandler
}

// OperationOption configures an operation created by [Handle].
// Common implementations include parameter validators ([Header], [QueryParam], etc.)
// and [OnError] for custom error handling.
type OperationOption func(*OperationOptions)

// OnError configures a custom [ErrorHandler] for an operation.
// If not specified, operations use a default error handler that logs errors
// and returns appropriate HTTP status codes.
//
// Example:
//
//	customErrorHandler := rest.ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
//	    log.Printf("Error: %v", err)
//	    w.WriteHeader(http.StatusInternalServerError)
//	    json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
//	})
//	rest.Handle(http.MethodGet, rest.BasePath("/users"), handler, rest.OnError(customErrorHandler))
func OnError(eh ErrorHandler) OperationOption {
	return func(oo *OperationOptions) {
		oo.errHandler = eh
	}
}

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

	Spec() (openapi3.RequestBodyOrRef, error)
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

	Spec() (int, openapi3.ResponseOrRef, error)
}

type operation[I, O any, Req TypedRequest[I], Resp TypedResponse[O]] struct {
	tracer     trace.Tracer
	errHandler ErrorHandler
	transforms []func(*http.Request) (*http.Request, error)
	handler    Handler[I, O]
}

func Operation[I, O any, Req TypedRequest[I], Resp TypedResponse[O]](method string, path Path, h Handler[I, O], opts ...OperationOption) ApiOption {
	return apiOptionFunc(func(ao *ApiOptions) {
		for _, el := range path {
			v, ok := el.(pathParam)
			if !ok {
				continue
			}

			opts = append(opts, param(v.name, openapi3.ParameterInPath, v.opts...))
		}

		oo := &OperationOptions{
			errHandler: defaultErrorHandler(humus.LogHandler("rest")),
		}
		for _, opt := range opts {
			opt(oo)
		}

		var req Req
		requestBodySpec, err := req.Spec()
		if err != nil {
			panic(err)
		}

		responses := make(map[string]openapi3.ResponseOrRef)

		var resp Resp
		status, respSpec, err := resp.Spec()
		if err != nil {
			panic(err)
		}
		responses[strconv.Itoa(status)] = respSpec

		op := openapi3.Operation{
			RequestBody: &requestBodySpec,
			Responses: openapi3.Responses{
				MapOfResponseOrRefValues: responses,
			},
			Parameters: oo.parameters,
		}

		endpoint := path.String()

		err = ao.def.AddOperation(method, endpoint, op)
		if err != nil {
			panic(err)
		}

		if oo.securityScheme != nil {
			ao.def.ComponentsEns().SecuritySchemesEns().WithMapOfSecuritySchemeOrRefValuesItem(
				oo.securityScheme.name,
				openapi3.SecuritySchemeOrRef{
					SecurityScheme: &oo.securityScheme.scheme,
				},
			)

			op.WithSecurity(map[string][]string{
				oo.securityScheme.name: {}, // todo: add support for populating this
			})
		}

		ao.mux.Method(method, endpoint, otelhttp.WithRouteTag(endpoint, &operation[I, O, Req, Resp]{
			tracer:     otel.Tracer("github.com/z5labs/humus/rest"),
			errHandler: oo.errHandler,
			transforms: oo.transforms,
			handler:    h,
		}))
	})
}

func (o *operation[I, O, Req, Resp]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var err error
	defer func() {
		if err == nil {
			return
		}

		o.errHandler.OnError(ctx, w, err)
	}()
	defer try.Recover(&err)

	for _, transform := range o.transforms {
		r, err = transform(r)
		if err != nil {
			return
		}
	}

	req, err := o.readRequest(ctx, r)
	if err != nil {
		return
	}

	resp, err := o.handler.Handle(ctx, &req)
	if err != nil {
		return
	}

	err = o.writeResponse(ctx, w, resp)
}

func (o *operation[I, O, Req, Resp]) readRequest(ctx context.Context, r *http.Request) (I, error) {
	spanCtx, span := o.tracer.Start(ctx, "operation.readRequest")
	defer span.End()

	var req I
	err := Req(&req).ReadRequest(spanCtx, r)
	if err != nil {
		return req, err
	}

	return req, nil
}

func (o *operation[I, O, Req, Resp]) writeResponse(ctx context.Context, w http.ResponseWriter, resp Resp) error {
	spanCtx, span := o.tracer.Start(ctx, "operation.writeResponse")
	defer span.End()

	return resp.WriteResponse(spanCtx, w)
}
