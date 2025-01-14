// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package mux

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/humus/health"
)

type operationDef func() (openapi3.Operation, error)

func (f operationDef) Definition() (openapi3.Operation, error) {
	return f()
}

func (operationDef) ServeHTTP(_ http.ResponseWriter, _ *http.Request) {}

func TestRouter_Handle(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if the Operation fails to return its OpenAPI definition", func(t *testing.T) {
			r := New("test", "v0.0.0")

			defErr := errors.New("failed to create operation definition")
			op := operationDef(func() (def openapi3.Operation, err error) {
				err = defErr
				return
			})

			err := r.Route(http.MethodGet, "/", op)
			if !assert.ErrorIs(t, err, defErr) {
				return
			}
		})

		t.Run("if the Operation has already been registered with the OpenAPI schema", func(t *testing.T) {
			r := New("test", "v0.0.0")

			op := operationDef(func() (def openapi3.Operation, err error) {
				return
			})

			err := r.Route(http.MethodGet, "/", op)
			if !assert.Nil(t, err) {
				return
			}

			err = r.Route(http.MethodGet, "/", op)
			if !assert.Error(t, err) {
				return
			}
		})
	})
}

type noopDefinition struct {
	http.Handler
}

func (noopDefinition) Definition() (openapi3.Operation, error) {
	return openapi3.Operation{}, nil
}

func TestRouter_ServeHTTP(t *testing.T) {
	t.Run("will return Not Found response", func(t *testing.T) {
		t.Run("if the request path does not match", func(t *testing.T) {
			r := New(
				"",
				"",
				NotFound(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(418)
				})),
			)

			r.Route(http.MethodGet, "/", noopDefinition{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			})

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/hello", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			if !assert.Equal(t, 418, resp.StatusCode) {
				return
			}
		})
	})

	t.Run("will return Method Not Allowed response", func(t *testing.T) {
		t.Run("if the HTTP method does not match", func(t *testing.T) {
			r := New(
				"",
				"",
				MethodNotAllowed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(418)
				})),
			)

			r.Route(http.MethodGet, "/", noopDefinition{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			})

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(``))

			r.ServeHTTP(w, req)

			resp := w.Result()
			if !assert.Equal(t, 418, resp.StatusCode) {
				return
			}
		})

	})

	t.Run("will return healthy", func(t *testing.T) {
		t.Run("if the readiness health monitor returns healthy", func(t *testing.T) {
			var readiness health.Binary
			readiness.MarkHealthy()

			r := New(
				"",
				"",
				Readiness(&readiness),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health/readiness", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
				return
			}
		})

		t.Run("if the liveness health monitor returns healthy", func(t *testing.T) {
			var liveness health.Binary
			liveness.MarkHealthy()

			r := New(
				"",
				"",
				Liveness(&liveness),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health/liveness", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			if !assert.Equal(t, http.StatusOK, resp.StatusCode) {
				return
			}
		})
	})

	t.Run("will return unhealthy", func(t *testing.T) {
		t.Run("if the readiness health monitor returns unhealthy", func(t *testing.T) {
			var readiness health.Binary

			r := New(
				"",
				"",
				Readiness(&readiness),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health/readiness", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			if !assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode) {
				return
			}
		})

		t.Run("if the liveness health monitor returns unhealthy", func(t *testing.T) {
			var liveness health.Binary

			r := New(
				"",
				"",
				Liveness(&liveness),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health/liveness", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			if !assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode) {
				return
			}
		})
	})
}
