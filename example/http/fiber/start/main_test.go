// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/fiber/cases"
)

func TestServer_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			u := serve(t, newServer(newStore()))

			c.Run(t, cases.Target{URL: u, Client: &http.Client{}})
		})
	}
}

func serve(t *testing.T, app *fiber.App) *url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() {
		require.NoError(t, app.Shutdown())
		require.NoError(t, <-served)
	})
	return &url.URL{Scheme: "http", Host: ln.Addr().String()}
}
