// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/example/shoppingcart/catalog/database"
	"github.com/z5labs/humus/example/shoppingcart/catalog/endpoint"
	"github.com/z5labs/humus/fault/jsonscenario"
)

func rehearse(t *testing.T, client *http.Client, method, path, token, body string, rules ...fit.Rule) response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, r)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	codec, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	req.Header.Set("Baggage", b.String())
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{code: resp.StatusCode, body: got}
}

var abortQueries = fit.Rule{Point: database.Point, Fault: fit.Fault{Abort: true}}

func TestInit_trusts_a_scenario_from_a_caller_whose_certificate_the_CA_issued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t, onFake(newFakeDatabase(false)))
		r, err := do(t, c.anonymous, http.MethodPut, "/products/sku-1", c.idp.token(t, endpoint.ScopeWrite), `{"unitPrice":12.5}`)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, r.code, "%s", r.body)

		r = rehearse(t, c.cart, http.MethodGet, "/products/sku-1", "", "", abortQueries)
		require.Equal(t, http.StatusServiceUnavailable, r.code, "%s", r.body)

		require.Equal(t, http.StatusOK, get(t, c.cart, "/products/sku-1").code)

		c.stop(t)
	})
}

func TestInit_strips_a_scenario_from_a_caller_without_a_client_certificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t, onFake(newFakeDatabase(false)))

		r := rehearse(t, c.anonymous, http.MethodPut, "/products/sku-1", c.idp.token(t, endpoint.ScopeWrite), `{"unitPrice":12.5}`, abortQueries)
		require.Equal(t, http.StatusOK, r.code, "%s", r.body)

		c.stop(t)
	})
}
