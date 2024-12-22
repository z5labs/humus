// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/rest/embedded"

	"github.com/go-chi/chi/v5"
	"github.com/swaggest/openapi-go/openapi3"
)

// MuxOptions represents configurable values for a [Mux].
type MuxOptions struct {
	readiness health.Monitor
	liveness  health.Monitor
}

// MuxOption sets values on [MuxOptions].
type MuxOption interface {
	ApplyMuxOption(*MuxOptions)
}

type muxOptionFunc func(*MuxOptions)

func (f muxOptionFunc) ApplyMuxOption(mo *MuxOptions) {
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
func Readiness(m health.Monitor) MuxOption {
	return muxOptionFunc(func(mo *MuxOptions) {
		mo.readiness = m
	})
}

// Liveness will register the given [health.Monitor] to be used
// for reporting when the entire application needs to be restarted.
//
// See [Liveness, Readiness, and Startup Probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/)
// for more details.
func Liveness(m health.Monitor) MuxOption {
	return muxOptionFunc(func(mo *MuxOptions) {
		mo.liveness = m
	})
}

type router interface {
	http.Handler

	Method(string, string, http.Handler)
}

// always ensure [Mux] implements the [Api] interface.
// if [Api] is ever changed this will lead to compilation error here.
var _ Api = (*Mux)(nil)

// Mux is a HTTP request multiplexer which implements the [Api] interface.
//
// Mux provides a set of standard features:
// - OpenAPI schema as JSON at "/openapi.json"
// - Liveness endpoint at "/health/liveness"
// - Readiness endpoint at "/health/readiness"
// - Standardized NotFound behaviour
// - Standardized MethodNotAllowed behaviour
type Mux struct {
	embedded.Api

	router router
	spec   *openapi3.Spec
}

// NewMux initializes a [Mux].
func NewMux(title, version string, opts ...MuxOption) *Mux {
	var defaultHealth health.Binary
	defaultHealth.MarkHealthy()

	mo := &MuxOptions{
		readiness: &defaultHealth,
		liveness:  &defaultHealth,
	}
	for _, opt := range opts {
		opt.ApplyMuxOption(mo)
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

	m.Get("/health/readiness", healthHandler(mo.readiness))
	m.Get("/health/liveness", healthHandler(mo.liveness))

	m.NotFound(func(w http.ResponseWriter, r *http.Request) {
		// TODO
	})

	m.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		// TODO
	})

	return &Mux{
		router: m,
		spec:   spec,
	}
}

func healthHandler(m health.Monitor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		healthy, err := m.Healthy(r.Context())
		if !healthy || err != nil {
			w.WriteHeader(http.StatusInternalServerError)
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

	Operation() openapi3.Operation
}

// Handle will configure any request matching method and pattern to be
// handled by the provided [Operation]. It will also register the [Operation]
// with an underlying OpenAPI 3.0 schema.
func (m *Mux) Handle(method, pattern string, op Operation) {
	err := m.spec.AddOperation(method, pattern, op.Operation())
	if err != nil {
		panic(err)
	}

	m.router.Method(method, pattern, op)
}

// ServeHTTP implements the [http.Handler] interface.
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.router.ServeHTTP(w, r)
}
