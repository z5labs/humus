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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/shoppingcart/database"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
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
		s := database.New(pool)
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
