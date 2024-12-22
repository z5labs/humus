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

// MuxOptions
type MuxOptions struct {
	readiness health.Monitor
	liveness  health.Monitor
}

// MuxOption
type MuxOption interface {
	ApplyMuxOption(*MuxOptions)
}

type muxOptionFunc func(*MuxOptions)

func (f muxOptionFunc) ApplyMuxOption(mo *MuxOptions) {
	f(mo)
}

// Readiness
func Readiness(m health.Monitor) MuxOption {
	return muxOptionFunc(func(mo *MuxOptions) {
		mo.readiness = m
	})
}

// Liveness
func Liveness(m health.Monitor) MuxOption {
	return muxOptionFunc(func(mo *MuxOptions) {
		mo.liveness = m
	})
}

type router interface {
	http.Handler

	Method(string, string, http.Handler)
}

var _ Api = (*Mux)(nil)

// Mux
type Mux struct {
	embedded.Api

	router router
	spec   *openapi3.Spec
}

// NewMux
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

type Operation interface {
	http.Handler

	Operation() openapi3.Operation
}

func (m *Mux) Handle(method, pattern string, op Operation) {
	m.spec.AddOperation(method, pattern, op.Operation())

	m.router.Method(method, pattern, op)
}

func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.router.ServeHTTP(w, r)
}
