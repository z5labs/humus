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

	"github.com/go-chi/chi/v5"
	"github.com/swaggest/openapi-go/openapi3"
)

// ApiOptions holds configuration values used when constructing an [Api].
// This struct is passed to [ApiOption] implementations to configure the API's
// router and OpenAPI specification.
type ApiOptions struct {
	mux *chi.Mux
	def *openapi3.Spec
}

// ApiOption is an interface for configuring an [Api].
// Implementations can modify the API's router or OpenAPI specification.
//
// Common implementations include:
//   - [Handle] - registers HTTP operations
//   - [Readiness] - configures readiness probe endpoint
//   - [Liveness] - configures liveness probe endpoint
//   - [NotFound] - customizes 404 handling
//   - [MethodNotAllowed] - customizes 405 handling
type ApiOption interface {
	ApplyApiOption(*ApiOptions)
}

type apiOptionFunc func(*ApiOptions)

func (f apiOptionFunc) ApplyApiOption(mo *ApiOptions) {
	f(mo)
}

// Readiness configures a custom readiness probe endpoint at GET /health/readiness.
// Readiness probes indicate whether the application is ready to serve traffic.
//
// See [Liveness, Readiness, and Startup Probes] for more details.
//
// [Liveness, Readiness, and Startup Probes]: https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/
func Readiness(h http.Handler) ApiOption {
	return apiOptionFunc(func(ro *ApiOptions) {
		ro.mux.Method(http.MethodGet, "/health/readiness", h)
	})
}

// Liveness configures a custom liveness probe endpoint at GET /health/liveness.
// Liveness probes indicate whether the application is running and should be restarted
// if it becomes unresponsive.
//
// See [Liveness, Readiness, and Startup Probes] for more details.
//
// [Liveness, Readiness, and Startup Probes]: https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/
func Liveness(h http.Handler) ApiOption {
	return apiOptionFunc(func(ro *ApiOptions) {
		ro.mux.Method(http.MethodGet, "/health/liveness", h)
	})
}

// NotFound configures a custom handler for requests that don't match any registered routes.
// This overrides the default 404 Not Found behavior.
//
// Example:
//
//	notFoundHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	    w.WriteHeader(http.StatusNotFound)
//	    json.NewEncoder(w).Encode(map[string]string{"error": "route not found"})
//	})
//	api := rest.NewApi("API", "v1", rest.NotFound(notFoundHandler))
func NotFound(h http.Handler) ApiOption {
	return apiOptionFunc(func(ro *ApiOptions) {
		ro.mux.NotFound(h.ServeHTTP)
	})
}

// MethodNotAllowed configures a custom handler for requests to valid routes
// with unsupported HTTP methods. This overrides the default 405 Method Not Allowed behavior.
//
// Example:
//
//	methodNotAllowedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	    w.WriteHeader(http.StatusMethodNotAllowed)
//	    json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed"})
//	})
//	api := rest.NewApi("API", "v1", rest.MethodNotAllowed(methodNotAllowedHandler))
func MethodNotAllowed(h http.Handler) ApiOption {
	return apiOptionFunc(func(ro *ApiOptions) {
		ro.mux.MethodNotAllowed(h.ServeHTTP)
	})
}

// Api is an OpenAPI-compliant [http.Handler] that serves as the foundation
// for building REST APIs.
//
// # Standard Features
//
// Every Api automatically provides:
//   - OpenAPI 3.0 schema available at GET /openapi.json
//   - Default liveness probe at GET /health/liveness (returns 200 OK)
//   - Default readiness probe at GET /health/readiness (returns 200 OK)
//   - Standard 404 Not Found handling
//   - Standard 405 Method Not Allowed handling
//
// # Usage
//
// Create an Api using [NewApi], passing operation handlers created with [Handle]:
//
//	getUserOp := rest.Handle(http.MethodGet, rest.BasePath("/users").Param("id"), getUserHandler)
//	api := rest.NewApi("User Service", "v1.0.0", getUserOp)
//	http.ListenAndServe(":8080", api)
type Api struct {
	router *chi.Mux
}

// NewApi creates a new [Api] with the specified title and version.
//
// The title and version are included in the OpenAPI specification served at /openapi.json.
// Additional operations and configuration can be added via [ApiOption] parameters.
//
// Example:
//
//	api := rest.NewApi(
//	    "Bookstore API",
//	    "v2.1.0",
//	    rest.Handle(http.MethodGet, rest.BasePath("/books"), listBooksHandler),
//	    rest.Handle(http.MethodPost, rest.BasePath("/books"), createBookHandler),
//	    rest.Readiness(customReadinessHandler),
//	)
func NewApi(title, version string, opts ...ApiOption) *Api {
	log := humus.Logger("github.com/z5labs/humus/rest")

	ao := &ApiOptions{
		mux: chi.NewMux(),
		def: &openapi3.Spec{
			Openapi: "3.0",
			Info: openapi3.Info{
				Title:   title,
				Version: version,
			},
		},
	}
	for _, opt := range opts {
		opt.ApplyApiOption(ao)
	}

	ao.mux.Get("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		err := enc.Encode(ao.def)
		if err == nil {
			return
		}
		log.ErrorContext(
			r.Context(),
			"failed to encode openapi schema to json",
			slog.Any("error", err),
		)
	})

	return &Api{
		router: ao.mux,
	}
}

// ServeHTTP implements the [http.Handler] interface.
// It delegates request handling to the configured router, which dispatches
// requests to the appropriate operation handlers based on method and path.
func (api *Api) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	api.router.ServeHTTP(w, req)
}
