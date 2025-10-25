// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/swaggest/openapi-go/openapi3"
)

// mockHandler is a simple test implementation of rest.Handler
type mockHandler struct {
	handler http.HandlerFunc
}

func (m mockHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.handler(w, r)
}

func (m mockHandler) RequestBody() openapi3.RequestBodyOrRef {
	return openapi3.RequestBodyOrRef{}
}

func (m mockHandler) Responses() openapi3.Responses {
	return openapi3.Responses{}
}

func TestNewApi(t *testing.T) {
	t.Run("serves OpenAPI spec at /openapi.json", func(t *testing.T) {
		api := NewApi("My API", "v2.3.1")

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/openapi.json")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var spec map[string]any
		err = json.NewDecoder(resp.Body).Decode(&spec)
		require.NoError(t, err)

		assert.Equal(t, "3.0", spec["openapi"])

		info := spec["info"].(map[string]any)
		assert.Equal(t, "My API", info["title"])
		assert.Equal(t, "v2.3.1", info["version"])
	})
}

func TestReadiness(t *testing.T) {
	t.Run("configures custom readiness handler", func(t *testing.T) {
		customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("custom ready"))
		})

		api := NewApi("Test", "v1", Readiness(customHandler))

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/health/readiness")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		body := make([]byte, 100)
		n, _ := resp.Body.Read(body)
		assert.Equal(t, "custom ready", string(body[:n]))
	})
}

func TestLiveness(t *testing.T) {
	t.Run("configures custom liveness handler", func(t *testing.T) {
		customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("custom alive"))
		})

		api := NewApi("Test", "v1", Liveness(customHandler))

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/health/liveness")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		body := make([]byte, 100)
		n, _ := resp.Body.Read(body)
		assert.Equal(t, "custom alive", string(body[:n]))
	})
}

func TestNotFound(t *testing.T) {
	t.Run("configures custom 404 handler", func(t *testing.T) {
		customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("custom 404"))
		})

		api := NewApi("Test", "v1", NotFound(customHandler))

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/nonexistent")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)

		body := make([]byte, 100)
		n, _ := resp.Body.Read(body)
		assert.Equal(t, "custom 404", string(body[:n]))
	})
}

func TestMethodNotAllowed(t *testing.T) {
	t.Run("configures custom 405 handler", func(t *testing.T) {
		customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			w.Write([]byte("custom 405"))
		})

		// Create a simple test handler using mockHandler
		testHandler := mockHandler{
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("ok"))
			},
		}

		// Register a GET handler at /test, then try a POST request
		api := NewApi(
			"Test",
			"v1",
			Handle(http.MethodGet, BasePath("/test"), testHandler),
			MethodNotAllowed(customHandler),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// Try POST on a route that only supports GET
		resp, err := http.Post(srv.URL+"/test", "text/plain", nil)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

		body := make([]byte, 100)
		n, _ := resp.Body.Read(body)
		assert.Equal(t, "custom 405", string(body[:n]))
	})
}
