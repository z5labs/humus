//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package database_test

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/shoppingcart/catalog/database"
	"github.com/z5labs/humus/example/shoppingcart/catalog/endpoint"
	"github.com/z5labs/humus/example/shoppingcart/internal/postgrestest"
	"github.com/z5labs/humus/fault"
)

func TestMain(m *testing.M) {
	postgrestest.Run(m)
}

func store(t *testing.T) *database.Store {
	t.Helper()
	db := postgrestest.NewDatabase(t)
	db.Migrated(t, "catalog")

	pool, err := pgxpool.New(t.Context(), db.AppURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return database.New(pool, fault.Injector{})
}

func TestStore_gets_the_product_it_puts(t *testing.T) {
	s := store(t)
	ctx := t.Context()
	id := "sku-" + rand.Text()

	put, err := s.PutProduct(ctx, endpoint.Product{ProductID: id, UnitPrice: 9.99})
	require.NoError(t, err)
	require.Equal(t, id, put.ProductID)
	require.InDelta(t, 9.99, put.UnitPrice, 1e-9)

	got, err := s.GetProduct(ctx, id)
	require.NoError(t, err)
	require.Equal(t, put, got)
}

func TestStore_putting_a_product_again_replaces_its_price(t *testing.T) {
	s := store(t)
	ctx := t.Context()
	id := "sku-" + rand.Text()

	_, err := s.PutProduct(ctx, endpoint.Product{ProductID: id, UnitPrice: 9.99})
	require.NoError(t, err)
	replaced, err := s.PutProduct(ctx, endpoint.Product{ProductID: id, UnitPrice: 0})
	require.NoError(t, err)
	require.Zero(t, replaced.UnitPrice)

	got, err := s.GetProduct(ctx, id)
	require.NoError(t, err)
	require.Equal(t, endpoint.Product{ProductID: id, UnitPrice: 0}, got)
}

func TestStore_reports_a_product_it_doesnt_have(t *testing.T) {
	s := store(t)
	id := "sku-" + rand.Text()

	_, err := s.GetProduct(t.Context(), id)
	require.ErrorIs(t, err, endpoint.ErrProductNotFound)
	require.NotErrorIs(t, err, endpoint.ErrUnavailable)
}

func TestStore_wraps_what_Postgres_rejects(t *testing.T) {
	s := store(t)
	id := "sku-" + rand.Text()

	_, err := s.PutProduct(t.Context(), endpoint.Product{ProductID: id, UnitPrice: -1})
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "putting product "+id), err.Error())
	require.NotErrorIs(t, err, endpoint.ErrUnavailable)
}
