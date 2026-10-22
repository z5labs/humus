// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httproute_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/internal/httproute"
)

func TestTrack(t *testing.T) {
	t.Run("nothing recorded is no route", func(t *testing.T) {
		_, recorded := httproute.Track(httptest.NewRequest(http.MethodGet, "/", nil))
		require.Empty(t, recorded())
	})

	t.Run("a route recorded on a copy of a copy", func(t *testing.T) {
		tracked, recorded := httproute.Track(httptest.NewRequest(http.MethodGet, "/carts/c-1", nil))
		copied := tracked.Clone(tracked.Context()).WithContext(tracked.Context())
		httproute.Record(copied.Context(), "/carts/{cartId}")
		require.Equal(t, "/carts/{cartId}", recorded())
	})

	t.Run("the route recorded last", func(t *testing.T) {
		tracked, recorded := httproute.Track(httptest.NewRequest(http.MethodGet, "/carts/c-1", nil))
		httproute.Record(tracked.Context(), "/carts/{cartId}")
		httproute.Record(tracked.Context(), "/")
		require.Equal(t, "/", recorded())
	})

	t.Run("a route recorded on a request nothing tracks is dropped", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		require.NotPanics(t, func() { httproute.Record(r.Context(), "/") })
	})
}
