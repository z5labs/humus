// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/huma/cases"
)

func TestServer_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(newServer(newStore(), slog.New(slog.DiscardHandler)))
			t.Cleanup(srv.Close)
			u, err := url.Parse(srv.URL)
			require.NoError(t, err)

			c.Run(t, cases.Target{URL: u, Client: srv.Client()})
		})
	}
}
