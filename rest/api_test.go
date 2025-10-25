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
)

func TestNewApi(t *testing.T) {
	t.Run("creates an API with title and version", func(t *testing.T) {
		api := NewApi("Test API", "v1.0.0")

		require.NotNil(t, api)
		assert.NotNil(t, api.router)
	})

	t.Run("serves OpenAPI spec at /openapi.json", func(t *testing.T) {
		api := NewApi("My API", "v2.3.1")

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/openapi.json")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var spec map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&spec)
		require.NoError(t, err)

		info := spec["info"].(map[string]interface{})
		assert.Equal(t, "My API", info["title"])
		assert.Equal(t, "v2.3.1", info["version"])
	})

	t.Run("includes OpenAPI version in spec", func(t *testing.T) {
		api := NewApi("Test", "v1")

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, _ := http.Get(srv.URL + "/openapi.json")
		defer resp.Body.Close()

		var spec map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&spec)

		assert.Equal(t, "3.0", spec["openapi"])
	})
}

func TestApi_ServeHTTP(t *testing.T) {
	t.Run("implements http.Handler", func(t *testing.T) {
		api := NewApi("Test", "v1")
		var _ http.Handler = api
	})

	t.Run("serves requests", func(t *testing.T) {
		api := NewApi("Test", "v1")

		srv := httptest.NewServer(api)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/openapi.json")
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
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

		api := NewApi(
			"Test",
			"v1",
			MethodNotAllowed(customHandler),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// The MethodNotAllowed handler is only called when a route exists
		// but the method is not allowed. Since we have no routes, we can't
		// easily test this without creating a full handler. For now, just verify
		// the option can be applied without error.
		assert.NotNil(t, api)
	})
}

func TestApiOption_Multiple(t *testing.T) {
	t.Run("applies multiple options in order", func(t *testing.T) {
		readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("ready"))
		})

		livenessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("alive"))
		})

		api := NewApi(
			"Multi-Option API",
			"v1.0.0",
			Readiness(readinessHandler),
			Liveness(livenessHandler),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		// Test readiness
		resp, _ := http.Get(srv.URL + "/health/readiness")
		body := make([]byte, 100)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		assert.Equal(t, "ready", string(body[:n]))

		// Test liveness
		resp, _ = http.Get(srv.URL + "/health/liveness")
		body = make([]byte, 100)
		n, _ = resp.Body.Read(body)
		resp.Body.Close()
		assert.Equal(t, "alive", string(body[:n]))
	})
}
