// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/go-chi/chi/v5"
	"github.com/swaggest/openapi-go/openapi3"
)

type BadRequestHandler interface {
	Handle(context.Context, http.ResponseWriter, error)
}

type BadRequestHandlerFunc func(context.Context, http.ResponseWriter, error)

func (f BadRequestHandlerFunc) Handle(ctx context.Context, w http.ResponseWriter, err error) {
	f(ctx, w, err)
}

// ApiOptions represents configurable values for a [Mux].
type ApiOptions struct {
	readiness               health.Monitor
	liveness                health.Monitor
	notFoundHandler         http.Handler
	methodNotAllowedHandler http.Handler
	badRequestHandler       BadRequestHandler
}

// ApiOption sets values on [MuxOptions].
type ApiOption interface {
	ApplyApiOption(*ApiOptions)
}

type apiOptionFunc func(*ApiOptions)

func (f apiOptionFunc) ApplyApiOption(mo *ApiOptions) {
	f(mo)
}

// Readiness will register the given [health.Monitor] to be used
// for reporting when the application is ready for to start accepting traffic.
//
// An example usage of this is to tie the [health.Monitor] to any backend client
// circuit breakers. When one of the circuit breakers moves to an OPEN state your
// application can quickly notify upstream component(s) (e.g. load balancer) that
// no requests should be sent to it since they'll just fail anyways due to the circuit
// being OPEN.
//
// See [Liveness, Readiness, and Startup Probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/)
// for more details.
func Readiness(m health.Monitor) ApiOption {
	return apiOptionFunc(func(ro *ApiOptions) {
		ro.readiness = m
	})
}

// Liveness will register the given [health.Monitor] to be used
// for reporting when the entire application needs to be restarted.
//
// See [Liveness, Readiness, and Startup Probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/)
// for more details.
func Liveness(m health.Monitor) ApiOption {
	return apiOptionFunc(func(ro *ApiOptions) {
		ro.liveness = m
	})
}

// NotFound
func NotFound(h http.Handler) ApiOption {
	return apiOptionFunc(func(ro *ApiOptions) {
		ro.notFoundHandler = h
	})
}

// MethodNotAllowed
func MethodNotAllowed(h http.Handler) ApiOption {
	return apiOptionFunc(func(ro *ApiOptions) {
		ro.methodNotAllowedHandler = h
	})
}

// BadRequest
func BadRequest(h BadRequestHandler) ApiOption {
	return apiOptionFunc(func(ao *ApiOptions) {
		ao.badRequestHandler = h
	})
}

type router interface {
	http.Handler

	Method(string, string, http.Handler)
}

// Api is a OpenAPI compliant [http.Handler].
//
// Api provides a set of standard features:
// - OpenAPI schema as JSON at "/openapi.json"
// - Liveness endpoint at "/health/liveness"
// - Readiness endpoint at "/health/readiness"
// - Standardized NotFound behaviour
// - Standardized MethodNotAllowed behaviour
type Api struct {
	router            router
	spec              *openapi3.Spec
	badRequestHandler BadRequestHandler
}

// NewApi initializes a [Api].
func NewApi(title, version string, opts ...ApiOption) *Api {
	var defaultHealth health.Binary
	defaultHealth.MarkHealthy()

	log := humus.Logger("rest")
	ao := &ApiOptions{
		readiness: &defaultHealth,
		liveness:  &defaultHealth,
		notFoundHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)

			log.WarnContext(r.Context(), "path not found", slog.String("url.full", r.URL.String()))
		}),
		methodNotAllowedHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMethodNotAllowed)

			log.WarnContext(r.Context(), "method not allowed", slog.String("url.full", r.URL.String()))
		}),
		badRequestHandler: BadRequestHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
			w.WriteHeader(http.StatusBadRequest)

			log.WarnContext(ctx, "received bad request")
		}),
	}
	for _, opt := range opts {
		opt.ApplyApiOption(ao)
	}

	spec := &openapi3.Spec{
		Openapi: "3.0",
		Info: openapi3.Info{
			Title:   title,
			Version: version,
		},
	}

	m := chi.NewMux()

	m.Get("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		err := enc.Encode(spec)
		if err == nil {
			return
		}
		log.ErrorContext(
			r.Context(),
			"failed to encode openapi schema to json",
			slog.Any("error", err),
		)
	})

	m.Get("/health/readiness", healthHandler(ao.readiness))
	m.Get("/health/liveness", healthHandler(ao.liveness))

	m.NotFound(ao.notFoundHandler.ServeHTTP)
	m.MethodNotAllowed(ao.methodNotAllowedHandler.ServeHTTP)

	return &Api{
		router:            m,
		spec:              spec,
		badRequestHandler: ao.badRequestHandler,
	}
}

func healthHandler(m health.Monitor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		healthy, err := m.Healthy(r.Context())
		if !healthy || err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// ServeHTTP implements the [http.Handler] interface.
func (api *Api) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	api.router.ServeHTTP(w, req)
}

// OperationOptions
type OperationOptions struct {
	summary     string
	description string
	params      []parameter
}

// OperationOption
type OperationOption func(*OperationOptions)

// Summary
func Summary(s string) OperationOption {
	return func(oo *OperationOptions) {
		oo.summary = s
	}
}

// Description
func Description(s string) OperationOption {
	return func(oo *OperationOptions) {
		oo.description = s
	}
}

// Header
func Header(name string, opts ...ParameterOption) OperationOption {
	return func(oo *OperationOptions) {
		p := parameter{
			def: &openapi3.Parameter{
				Name: name,
				In:   openapi3.ParameterInHeader,
			},
		}
		for _, opt := range opts {
			opt(&p)
		}

		oo.params = append(oo.params, p)
	}
}

// QueryParam
func QueryParam(name string, opts ...ParameterOption) OperationOption {
	return func(oo *OperationOptions) {
		p := parameter{
			def: &openapi3.Parameter{
				Name: name,
				In:   openapi3.ParameterInQuery,
			},
		}
		for _, opt := range opts {
			opt(&p)
		}

		oo.params = append(oo.params, p)
	}
}

// Authorizer
type Authorizer interface {
	Authorize(*http.Request) error
}

// Authorization
func Authorization(a Authorizer) OperationOption {
	return Header(
		"Authorization",
		Required(),
		ValidateParamWith(a.Authorize),
	)
}

type operationHandler struct {
	inner             http.Handler
	validators        []func(*http.Request) error
	badRequestHandler BadRequestHandler
}

// Operation will configure any request matching method and pattern to be
// handled by the provided [Operation]. It will also register the [Operation]
// with an underlying OpenAPI 3.0 schema.
func (api *Api) Operation(method string, path PathElement, h http.Handler, opts ...OperationOption) {
	oo := &OperationOptions{}
	for _, opt := range opts {
		opt(oo)
	}

	var params []openapi3.ParameterOrRef
	var validators []func(*http.Request) error
	for _, param := range oo.params {
		params = append(params, openapi3.ParameterOrRef{
			Parameter: param.def,
		})

		validators = append(validators, param.validators...)
	}

	op := openapi3.Operation{
		Summary:     &oo.summary,
		Description: &oo.description,
		Parameters:  params,
	}

	withRequestBody, ok := h.(interface {
		RequestBody() openapi3.RequestBodyOrRef
	})
	if ok {
		reqBody := withRequestBody.RequestBody()
		op.RequestBody = &reqBody
	}

	withResponses, ok := h.(interface {
		Responses() openapi3.Responses
	})
	if ok {
		op.Responses = withResponses.Responses()
	}

	route := path.pathElement()
	err := api.spec.AddOperation(method, route, op)
	if err != nil {
		panic(err)
	}

	api.router.Method(method, route, otelhttp.WithRouteTag(route, &operationHandler{
		inner:             h,
		validators:        validators,
		badRequestHandler: api.badRequestHandler,
	}))
}

// ServeHTTP implements http.Handler.
func (o *operationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, validator := range o.validators {
		err := validator(r)
		if err != nil {
			o.badRequestHandler.Handle(r.Context(), w, err)
			return
		}
	}

	o.inner.ServeHTTP(w, r)
}
