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

	"github.com/z5labs/humus/example/shoppingcart/catalog/database"
	"github.com/z5labs/humus/example/shoppingcart/catalog/endpoint"
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
		s := database.New(pool)

		_, err = s.GetProduct(t.Context(), "sku-1")
		require.ErrorIs(t, err, endpoint.ErrUnavailable)
		require.NotErrorIs(t, err, endpoint.ErrProductNotFound)

		_, err = s.PutProduct(t.Context(), endpoint.Product{ProductID: "sku-1", UnitPrice: 1})
		require.ErrorIs(t, err, endpoint.ErrUnavailable)
	})
}
