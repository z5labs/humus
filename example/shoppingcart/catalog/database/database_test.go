// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package database_test

import (
	"context"
	"crypto/rand"
	"testing"
	"testing/synctest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/example/shoppingcart/catalog/database"
	"github.com/z5labs/humus/example/shoppingcart/catalog/endpoint"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
)

func TestStore_wraps_a_connection_that_failed_as_ErrUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		cfg, err := pgxpool.ParseConfig("postgres://catalog:" + rand.Text() + "@db.internal:5432/catalog?sslmode=disable")
		require.NoError(t, err)
		cfg.ConnConfig.DialFunc = h.Dial
		cfg.ConnConfig.LookupFunc = func(_ context.Context, host string) ([]string, error) {
			return []string{host}, nil
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		require.NoError(t, err)
		defer pool.Close()
		s := database.New(pool, fault.Injector{})

		_, err = s.GetProduct(t.Context(), "sku-1")
		require.ErrorIs(t, err, endpoint.ErrUnavailable)
		require.NotErrorIs(t, err, endpoint.ErrProductNotFound)

		_, err = s.PutProduct(t.Context(), endpoint.Product{ProductID: "sku-1", UnitPrice: 1})
		require.ErrorIs(t, err, endpoint.ErrUnavailable)
	})
}

func TestStore_fails_every_query_a_scenario_aborts_as_ErrUnavailable(t *testing.T) {
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	settings := fault.DefaultSettings()
	settings.Decoder = codec
	faults, err := fault.New(settings)
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{{Point: database.Point, Fault: fit.Fault{Abort: true}}}})
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), b)
	s := database.New(nil, faults)

	_, err = s.GetProduct(ctx, "sku-1")
	require.ErrorIs(t, err, endpoint.ErrUnavailable)
	require.ErrorIs(t, err, fit.ErrInjected)

	_, err = s.PutProduct(ctx, endpoint.Product{ProductID: "sku-1", UnitPrice: 1})
	require.ErrorIs(t, err, endpoint.ErrUnavailable)
	require.ErrorIs(t, err, fit.ErrInjected)
}
