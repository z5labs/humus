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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/example/shoppingcart/database"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
)

func TestStore_wraps_a_connection_that_failed_as_ErrUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		cfg, err := pgxpool.ParseConfig("postgres://carts:" + rand.Text() + "@db.internal:5432/carts?sslmode=disable")
		require.NoError(t, err)
		cfg.ConnConfig.DialFunc = h.Dial
		cfg.ConnConfig.LookupFunc = func(_ context.Context, host string) ([]string, error) {
			return []string{host}, nil
		}
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		require.NoError(t, err)
		defer pool.Close()
		s := database.New(pool, fault.Injector{})
		ctx := t.Context()
		cartID, itemID := uuid.New(), uuid.New()

		_, err = s.CreateCart(ctx, "shopper")
		require.ErrorIs(t, err, database.ErrUnavailable)

		_, err = s.GetCart(ctx, "shopper", cartID)
		require.ErrorIs(t, err, database.ErrUnavailable)
		require.NotErrorIs(t, err, endpoint.ErrCartNotFound)

		err = s.DeleteCart(ctx, "shopper", cartID)
		require.ErrorIs(t, err, database.ErrUnavailable)
		require.NotErrorIs(t, err, endpoint.ErrCartNotFound)

		_, err = s.AddItem(ctx, "shopper", cartID, endpoint.NewItem{ProductID: "sku-1", Quantity: 1, UnitPrice: 1})
		require.ErrorIs(t, err, database.ErrUnavailable)
		require.NotErrorIs(t, err, endpoint.ErrCartNotFound)

		_, err = s.UpdateItem(ctx, "shopper", cartID, itemID, 2)
		require.ErrorIs(t, err, database.ErrUnavailable)
		require.NotErrorIs(t, err, endpoint.ErrItemNotFound)

		err = s.RemoveItem(ctx, "shopper", cartID, itemID)
		require.ErrorIs(t, err, database.ErrUnavailable)
		require.NotErrorIs(t, err, endpoint.ErrItemNotFound)
	})
}

func rehearsing(t *testing.T, rules ...fit.Rule) (fault.Injector, context.Context) {
	t.Helper()
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	settings := fault.DefaultSettings()
	settings.Decoder = codec
	faults, err := fault.New(settings)
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	return faults, baggage.ContextWithBaggage(t.Context(), b)
}

func TestStore_fails_every_query_a_scenario_aborts_as_ErrUnavailable(t *testing.T) {
	faults, ctx := rehearsing(t, fit.Rule{Point: database.Point, Fault: fit.Fault{Abort: true}})
	s := database.New(nil, faults)
	cartID, itemID := uuid.New(), uuid.New()

	_, err := s.CreateCart(ctx, "shopper")
	require.ErrorIs(t, err, database.ErrUnavailable)
	require.ErrorIs(t, err, fit.ErrInjected)

	_, err = s.GetCart(ctx, "shopper", cartID)
	require.ErrorIs(t, err, database.ErrUnavailable)
	require.ErrorIs(t, err, fit.ErrInjected)

	err = s.DeleteCart(ctx, "shopper", cartID)
	require.ErrorIs(t, err, database.ErrUnavailable)
	require.ErrorIs(t, err, fit.ErrInjected)

	_, err = s.AddItem(ctx, "shopper", cartID, endpoint.NewItem{ProductID: "sku-1", Quantity: 1, UnitPrice: 1})
	require.ErrorIs(t, err, database.ErrUnavailable)
	require.ErrorIs(t, err, fit.ErrInjected)

	_, err = s.UpdateItem(ctx, "shopper", cartID, itemID, 2)
	require.ErrorIs(t, err, database.ErrUnavailable)
	require.ErrorIs(t, err, fit.ErrInjected)

	err = s.RemoveItem(ctx, "shopper", cartID, itemID)
	require.ErrorIs(t, err, database.ErrUnavailable)
	require.ErrorIs(t, err, fit.ErrInjected)
}

func TestStore_waits_out_a_scenarios_delay_before_its_query(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		faults, ctx := rehearsing(t, fit.Rule{Point: database.Point, Fault: fit.Fault{Delay: 3 * time.Second}})
		s := database.New(nil, faults)
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()

		start := time.Now()
		_, err := s.CreateCart(ctx, "shopper")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, time.Second, time.Since(start))
		var injected *fault.InjectedError
		require.ErrorAs(t, err, &injected)
		require.Equal(t, database.Point, injected.Point)
	})
}
