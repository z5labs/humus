// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/gin/cases"
)

func TestServer_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(newServer(newStore()))
			t.Cleanup(srv.Close)
			u, err := url.Parse(srv.URL)
			require.NoError(t, err)

			c.Run(t, cases.Target{URL: u, Client: srv.Client()})
		})
	}
}
