// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package mux provides a [rest.Api] implementation which provides client access to an OpenAPI schema.
package mux

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/embedded"

	"github.com/go-chi/chi/v5"
	"github.com/swaggest/openapi-go/openapi3"
)

// RouterOptions represents configurable values for a [Mux].
type RouterOptions struct {
	readiness               health.Monitor
	liveness                health.Monitor
	notFoundHandler         http.Handler
	methodNotAllowedHandler http.Handler
}

// RouterOption sets values on [MuxOptions].
type RouterOption interface {
	ApplyRouterOption(*RouterOptions)
}

type routerOptionFunc func(*RouterOptions)

func (f routerOptionFunc) ApplyRouterOption(mo *RouterOptions) {
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
func Readiness(m health.Monitor) RouterOption {
	return routerOptionFunc(func(ro *RouterOptions) {
		ro.readiness = m
	})
}

// Liveness will register the given [health.Monitor] to be used
// for reporting when the entire application needs to be restarted.
//
// See [Liveness, Readiness, and Startup Probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/)
// for more details.
func Liveness(m health.Monitor) RouterOption {
	return routerOptionFunc(func(ro *RouterOptions) {
		ro.liveness = m
	})
}

// NotFound
func NotFound(h http.Handler) RouterOption {
	return routerOptionFunc(func(ro *RouterOptions) {
		ro.notFoundHandler = h
	})
}

// MethodNotAllowed
func MethodNotAllowed(h http.Handler) RouterOption {
	return routerOptionFunc(func(ro *RouterOptions) {
		ro.methodNotAllowedHandler = h
	})
}

type router interface {
	http.Handler

	Method(string, string, http.Handler)
}

// always ensure [Router] implements the [rest.Api] interface.
// if [Api] is ever changed this will lead to compilation error here.
var _ rest.Api = (*Router)(nil)

// Router is a HTTP request multiplexer which implements the [rest.Api] interface.
//
// Router provides a set of standard features:
// - OpenAPI schema as JSON at "/openapi.json"
// - Liveness endpoint at "/health/liveness"
// - Readiness endpoint at "/health/readiness"
// - Standardized NotFound behaviour
// - Standardized MethodNotAllowed behaviour
type Router struct {
	embedded.Api

	router router
	spec   *openapi3.Spec
}

// New initializes a [Router].
func New(title, version string, opts ...RouterOption) *Router {
	var defaultHealth health.Binary
	defaultHealth.MarkHealthy()

	ro := &RouterOptions{
		readiness: &defaultHealth,
		liveness:  &defaultHealth,
		notFoundHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			// TODO
		}),
		methodNotAllowedHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			// TODO
		}),
	}
	for _, opt := range opts {
		opt.ApplyRouterOption(ro)
	}

	spec := &openapi3.Spec{
		Openapi: "3.0",
		Info: openapi3.Info{
			Title:   title,
			Version: version,
		},
	}

	m := chi.NewMux()

	log := humus.Logger("rest")
	m.Get("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		err := enc.Encode(spec)
		if err == nil {
			return
		}
		log.ErrorContext(
			r.Context(),
			"failed to encode openapi schema to json",
			slog.String("error", err.Error()),
		)
	})

	m.Get("/health/readiness", healthHandler(ro.readiness))
	m.Get("/health/liveness", healthHandler(ro.liveness))

	m.NotFound(ro.notFoundHandler.ServeHTTP)
	m.MethodNotAllowed(ro.methodNotAllowedHandler.ServeHTTP)

	return &Router{
		router: m,
		spec:   spec,
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

// Operation extends the [http.Handler] interface by forcing
// any implementation to also provided a OpenAPI 3.0 representation
// of its operation.
type Operation interface {
	http.Handler

	Definition() (openapi3.Operation, error)
}

// Muxer
type Muxer interface {
	Route(method, pattern string, op Operation) error
}

// MustRoute
func MustRoute(m Muxer, method, pattern string, op Operation) {
	err := m.Route(method, pattern, op)
	if err == nil {
		return
	}
	panic(err)
}

// Route will configure any request matching method and pattern to be
// handled by the provided [Operation]. It will also register the [Operation]
// with an underlying OpenAPI 3.0 schema.
func (r *Router) Route(method, pattern string, op Operation) error {
	opDef, err := op.Definition()
	if err != nil {
		return err
	}

	err = r.spec.AddOperation(method, pattern, opDef)
	if err != nil {
		return err
	}

	r.router.Method(method, pattern, op)
	return nil
}

// ServeHTTP implements the [http.Handler] interface.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.router.ServeHTTP(w, req)
}
