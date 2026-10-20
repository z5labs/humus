// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	catalogdatabase "github.com/z5labs/humus/example/shoppingcart/catalog/database"
	catalogendpoint "github.com/z5labs/humus/example/shoppingcart/catalog/endpoint"
	"github.com/z5labs/humus/example/shoppingcart/database"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const sku = "sku-1"

type answer struct {
	code int
	body []byte
}

func (s *shoppingcart) send(t *testing.T, method, path, token string, body any, rules ...fit.Rule) answer {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, s.target.Cart.JoinPath(path).String(), r)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if rules != nil {
		codec, err := jsonscenario.New(scenarioKey)
		require.NoError(t, err)
		b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
		require.NoError(t, err)
		req.Header.Set("Baggage", b.String())
	}
	resp, err := s.target.CartClient.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return answer{code: resp.StatusCode, body: b}
}

func (s *shoppingcart) token(t *testing.T, scopes ...string) string {
	t.Helper()
	token, err := s.target.Token(shopper, scopes...)
	require.NoError(t, err)
	return token
}

func (s *shoppingcart) createCart(t *testing.T, token string) string {
	t.Helper()
	a := s.send(t, http.MethodPost, "/carts", token, nil)
	require.Equal(t, http.StatusCreated, a.code, "%s", a.body)
	var c struct {
		CartID string `json:"cartId"`
	}
	require.NoError(t, json.Unmarshal(a.body, &c))
	return c.CartID
}

func (s *shoppingcart) addItem(t *testing.T, cartID, token string, rules ...fit.Rule) answer {
	t.Helper()
	return s.send(t, http.MethodPost, "/carts/"+cartID+"/items", token, map[string]any{"productId": sku, "quantity": 1}, rules...)
}

func (s *shoppingcart) stock(t *testing.T) {
	t.Helper()
	_, err := s.products.PutProduct(t.Context(), catalogendpoint.Product{ProductID: sku, UnitPrice: 9.99})
	require.NoError(t, err)
}

func unavailable(t *testing.T, a answer) problemDetail {
	t.Helper()
	require.Equal(t, http.StatusServiceUnavailable, a.code, "%s", a.body)
	var d problemDetail
	require.NoError(t, json.Unmarshal(a.body, &d), "%s", a.body)
	require.Equal(t, http.StatusServiceUnavailable, d.Status)
	return d
}

func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func stripped(spans []sdktrace.ReadOnlySpan) int {
	n := 0
	for _, span := range spans {
		for _, ev := range span.Events() {
			if ev.Name == "humus.fit.strip" {
				n++
			}
		}
	}
	return n
}

func TestInit_answers_503_without_calling_the_catalog_when_a_trusted_scenario_aborts_its_point(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startShoppingcart(t)
		s.stock(t)
		token := s.token(t, endpoint.ScopeWrite, trustScope)
		cartID := s.createCart(t, token)

		d := unavailable(t, s.addItem(t, cartID, token, abort(s.target.CatalogPoint)))
		require.Contains(t, d.Detail, "catalog")
		require.Zero(t, s.products.queried(), "the catalog was asked for a price")

		s.stop(t)
	})
}

func TestInit_forwards_a_trusted_scenario_that_aborts_the_catalogs_query(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startShoppingcart(t)
		s.stock(t)
		token := s.token(t, endpoint.ScopeWrite, trustScope)
		cartID := s.createCart(t, token)

		d := unavailable(t, s.addItem(t, cartID, token, abort(catalogdatabase.Point)))
		require.Contains(t, d.Detail, "catalog")
		require.Equal(t, httpclient.DefaultSettings().MaxAttempts, s.products.queried())

		s.stop(t)
	})
}

func TestInit_answers_a_read_a_trusted_scenario_fails_with_503_saying_to_try_again(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startShoppingcart(t)
		token := s.token(t, endpoint.ScopeWrite, trustScope)
		cartID := s.createCart(t, token)

		d := unavailable(t, s.send(t, http.MethodGet, "/carts/"+cartID, token, nil, abort(database.Point)))
		require.Contains(t, d.Detail, "database")
		require.Contains(t, d.Detail, "Try again")

		require.Equal(t, http.StatusOK, s.send(t, http.MethodGet, "/carts/"+cartID, token, nil).code)

		s.stop(t)
	})
}

func TestInit_strips_a_scenario_it_does_not_trust(t *testing.T) {
	t.Run("from a token without the trust scope", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			s := startShoppingcartWith(t, map[string]string{"FAULT_TRUST_SCOPE": trustScope}, tel.Builder(), telemetry.NoopBuilder)
			s.stock(t)
			token := s.token(t, endpoint.ScopeWrite)
			cartID := s.createCart(t, token)

			a := s.addItem(t, cartID, token, abort(s.target.CatalogPoint), abort(catalogdatabase.Point), abort(database.Point))
			require.Equal(t, http.StatusCreated, a.code, "%s", a.body)
			require.Equal(t, 1, s.products.queried())

			s.stop(t)
			require.Equal(t, 1, stripped(tel.Spans()))
		})
	})

	t.Run("with no token", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			s := startShoppingcartWith(t, map[string]string{"FAULT_TRUST_SCOPE": trustScope}, tel.Builder(), telemetry.NoopBuilder)
			cartID := s.createCart(t, s.token(t, endpoint.ScopeWrite, trustScope))

			a := s.addItem(t, cartID, "", abort(s.target.CatalogPoint))
			require.Equal(t, http.StatusUnauthorized, a.code, "%s", a.body)

			s.stop(t)
			require.Equal(t, 1, stripped(tel.Spans()))
		})
	})
}

func TestInit_trusts_no_scenario_without_a_trust_scope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		s := startShoppingcartWith(t, nil, tel.Builder(), telemetry.NoopBuilder)
		s.stock(t)
		token := s.token(t, endpoint.ScopeWrite, trustScope)
		cartID := s.createCart(t, token)

		a := s.addItem(t, cartID, token, abort(s.target.CatalogPoint), abort(catalogdatabase.Point), abort(database.Point))
		require.Equal(t, http.StatusCreated, a.code, "%s", a.body)
		require.Equal(t, 1, s.products.queried())

		s.stop(t)
		require.Equal(t, 1, stripped(tel.Spans()))
	})
}

func TestInit_with_a_bad_fault_setting_exits_78(t *testing.T) {
	for _, tc := range []struct{ name, value, cause string }{
		{"FAULT_TRUST_SCOPE", "faults:rehearse carts:write", "must be one scope"},
		{"FAULT_MAX_DELAY", "-1s", "positive"},
		{"FAULT_MAX_DELAY", "10", "missing unit"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				f.env[tc.name] = tc.value
				_, status := httpservertest.Start(t, Init,
					humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(Faults)),
					humustest.WithEnv(f.env),
				)

				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.ErrorContains(t, st.Cause(), "config "+tc.name+": ")
				require.ErrorContains(t, st.Cause(), tc.cause)
			})
		})
	}
}
