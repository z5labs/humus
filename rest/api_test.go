// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/sdk-go/try"
)

type operationDef func() (openapi3.Operation, error)

func (f operationDef) Spec() (openapi3.Operation, error) {
	return f()
}

func (operationDef) ServeHTTP(_ http.ResponseWriter, _ *http.Request) {}

func TestApi_Operation(t *testing.T) {
	t.Run("will return an error", func(t *testing.T) {
		t.Run("if the Operation has already been registered with the OpenAPI schema", func(t *testing.T) {
			r := NewApi("test", "v0.0.0")

			op := operationDef(func() (def openapi3.Operation, err error) {
				return
			})

			register := func() (err error) {
				defer try.Recover(&err)
				r.Operation(http.MethodGet, StaticPath("/"), op)
				return nil
			}

			err := register()
			require.Nil(t, err)

			err = register()
			require.Error(t, err)
		})
	})
}

type noopDefinition struct {
	http.Handler
}

func (noopDefinition) Spec() (openapi3.Operation, error) {
	return openapi3.Operation{}, nil
}

func TestApi_ServeHTTP(t *testing.T) {
	t.Run("will return Not Found response", func(t *testing.T) {
		t.Run("if the request path does not match", func(t *testing.T) {
			r := NewApi(
				"",
				"",
				NotFound(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(418)
				})),
			)

			r.Operation(http.MethodGet, StaticPath("/"), noopDefinition{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			})

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/hello", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			require.Equal(t, 418, resp.StatusCode)
		})
	})

	t.Run("will return Method Not Allowed response", func(t *testing.T) {
		t.Run("if the HTTP method does not match", func(t *testing.T) {
			r := NewApi(
				"",
				"",
				MethodNotAllowed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(418)
				})),
			)

			r.Operation(http.MethodGet, StaticPath("/"), noopDefinition{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			})

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(``))

			r.ServeHTTP(w, req)

			resp := w.Result()
			require.Equal(t, 418, resp.StatusCode)
		})

	})

	t.Run("will return healthy", func(t *testing.T) {
		t.Run("if the readiness health monitor returns healthy", func(t *testing.T) {
			var readiness health.Binary
			readiness.MarkHealthy()

			r := NewApi(
				"",
				"",
				Readiness(&readiness),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health/readiness", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			require.Equal(t, http.StatusOK, resp.StatusCode)
		})

		t.Run("if the liveness health monitor returns healthy", func(t *testing.T) {
			var liveness health.Binary
			liveness.MarkHealthy()

			r := NewApi(
				"",
				"",
				Liveness(&liveness),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health/liveness", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			require.Equal(t, http.StatusOK, resp.StatusCode)
		})
	})

	t.Run("will return unhealthy", func(t *testing.T) {
		t.Run("if the readiness health monitor returns unhealthy", func(t *testing.T) {
			var readiness health.Binary

			r := NewApi(
				"",
				"",
				Readiness(&readiness),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health/readiness", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		})

		t.Run("if the liveness health monitor returns unhealthy", func(t *testing.T) {
			var liveness health.Binary

			r := NewApi(
				"",
				"",
				Liveness(&liveness),
			)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/health/liveness", nil)

			r.ServeHTTP(w, req)

			resp := w.Result()
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		})
	})
}
