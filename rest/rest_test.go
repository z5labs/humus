// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	bedrockconfig "github.com/z5labs/bedrock/config"
	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/stretchr/testify/require"
)

type testResponse struct {
	Message string `json:"message"`
}

type testError struct {
	Message string `json:"message"`
}

func (e testError) Error() string { return e.Message }

// freePort returns an available TCP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// insecureClient returns an HTTP client that skips TLS certificate verification.
func insecureClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
}

func TestRun(t *testing.T) {
	t.Run("serves a registered route over HTTPS", func(t *testing.T) {
		port := freePort(t)

		ep := bedrockrest.GET("/hello", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) (testResponse, error) {
			return testResponse{Message: "hello"}, nil
		})
		ep = bedrockrest.WriteJSON[testResponse](http.StatusOK, ep)
		route := bedrockrest.CatchAll(http.StatusInternalServerError, func(err error) testError {
			return testError{Message: err.Error()}
		}, ep)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(
				ctx,
				Port(bedrockconfig.ReaderOf(port)),
				Handle(route),
			)
		}()

		client := insecureClient()
		url := fmt.Sprintf("https://localhost:%d/hello", port)

		var resp *http.Response
		require.Eventually(t, func() bool {
			var err error
			resp, err = client.Get(url)
			return err == nil
		}, 5*time.Second, 50*time.Millisecond)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		var body testResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		require.Equal(t, "hello", body.Message)

		cancel()
		require.NoError(t, <-errCh)
	})

	t.Run("serves the OpenAPI spec", func(t *testing.T) {
		port := freePort(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(
				ctx,
				Title("Test API"),
				Version("1.2.3"),
				Port(bedrockconfig.ReaderOf(port)),
			)
		}()

		client := insecureClient()
		url := fmt.Sprintf("https://localhost:%d/openapi.json", port)

		var resp *http.Response
		require.Eventually(t, func() bool {
			var err error
			resp, err = client.Get(url)
			return err == nil
		}, 5*time.Second, 50*time.Millisecond)
		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		var spec map[string]any
		require.NoError(t, json.Unmarshal(body, &spec))

		info, ok := spec["info"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "Test API", info["title"])
		require.Equal(t, "1.2.3", info["version"])

		cancel()
		require.NoError(t, <-errCh)
	})
}

func TestBuildComponents(t *testing.T) {
	t.Run("buildHandler", func(t *testing.T) {
		o := defaultOptions()
		_, err := buildHandler(o).Build(context.Background())
		require.NoError(t, err)
	})

	t.Run("buildListener", func(t *testing.T) {
		o := defaultOptions()
		o.port = bedrockconfig.ReaderOf(freePort(t))
		_, err := buildListener(o).Build(context.Background())
		require.NoError(t, err)
	})

	t.Run("buildRuntime", func(t *testing.T) {
		o := defaultOptions()
		o.port = bedrockconfig.ReaderOf(freePort(t))
		_, err := buildRuntime(o).Build(context.Background())
		require.NoError(t, err)
	})
}
