//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package database_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/shoppingcart/database"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/example/shoppingcart/internal/postgrestest"
)

const shopper = "shopper"

func TestMain(m *testing.M) {
	postgrestest.Run(m)
}

func store(t *testing.T) *database.Store {
	t.Helper()
	db := postgrestest.NewDatabase(t)
	db.Migrated(t, "carts")

	pool, err := pgxpool.New(t.Context(), db.AppURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return database.New(pool)
}

func TestStore_keeps_a_cart_and_its_items(t *testing.T) {
	s := store(t)
	ctx := t.Context()

	created, err := s.CreateCart(ctx, shopper)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, created.CartID)
	require.Empty(t, created.Items)
	require.NotNil(t, created.Items)
	require.WithinDuration(t, time.Now(), created.CreatedAt, time.Minute)

	got, err := s.GetCart(ctx, shopper, created.CartID)
	require.NoError(t, err)
	require.Equal(t, created.CartID, got.CartID)
	require.Empty(t, got.Items)

	first, err := s.AddItem(ctx, shopper, created.CartID, endpoint.NewItem{ProductID: "sku-1", Quantity: 2, UnitPrice: 9.99})
	require.NoError(t, err)
	require.Equal(t, created.CartID, first.CartID)
	require.Equal(t, "sku-1", first.ProductID)
	require.Equal(t, 2, first.Quantity)
	require.InDelta(t, 9.99, first.UnitPrice, 1e-9)
	second, err := s.AddItem(ctx, shopper, created.CartID, endpoint.NewItem{ProductID: "sku-2", Quantity: 1, UnitPrice: 0})
	require.NoError(t, err)

	updated, err := s.UpdateItem(ctx, shopper, created.CartID, first.ItemID, 5)
	require.NoError(t, err)
	require.Equal(t, first.ItemID, updated.ItemID)
	require.Equal(t, 5, updated.Quantity)
	require.False(t, updated.UpdatedAt.Before(first.UpdatedAt))

	got, err = s.GetCart(ctx, shopper, created.CartID)
	require.NoError(t, err)
	require.Len(t, got.Items, 2)
	require.Equal(t, first.ItemID, got.Items[0].ItemID, "oldest first")
	require.Equal(t, 5, got.Items[0].Quantity)
	require.Equal(t, second.ItemID, got.Items[1].ItemID)
	require.False(t, got.UpdatedAt.Before(updated.UpdatedAt), "a change to an item updates its cart")

	require.NoError(t, s.RemoveItem(ctx, shopper, created.CartID, first.ItemID))
	got, err = s.GetCart(ctx, shopper, created.CartID)
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	require.Equal(t, second.ItemID, got.Items[0].ItemID)

	require.NoError(t, s.DeleteCart(ctx, shopper, created.CartID))
	_, err = s.GetCart(ctx, shopper, created.CartID)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
}

func TestStore_reports_what_doesnt_exist(t *testing.T) {
	s := store(t)
	ctx := t.Context()
	missing := uuid.New()

	_, err := s.GetCart(ctx, shopper, missing)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
	require.ErrorIs(t, s.DeleteCart(ctx, shopper, missing), endpoint.ErrCartNotFound)
	_, err = s.AddItem(ctx, shopper, missing, endpoint.NewItem{ProductID: "sku-1", Quantity: 1})
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
	_, err = s.UpdateItem(ctx, shopper, missing, uuid.New(), 1)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
	require.ErrorIs(t, s.RemoveItem(ctx, shopper, missing, uuid.New()), endpoint.ErrCartNotFound)

	created, err := s.CreateCart(ctx, shopper)
	require.NoError(t, err)
	_, err = s.UpdateItem(ctx, shopper, created.CartID, uuid.New(), 1)
	require.ErrorIs(t, err, endpoint.ErrItemNotFound)
	require.ErrorIs(t, s.RemoveItem(ctx, shopper, created.CartID, uuid.New()), endpoint.ErrItemNotFound)

	other, err := s.CreateCart(ctx, shopper)
	require.NoError(t, err)
	it, err := s.AddItem(ctx, shopper, other.CartID, endpoint.NewItem{ProductID: "sku-1", Quantity: 1})
	require.NoError(t, err)
	_, err = s.UpdateItem(ctx, shopper, created.CartID, it.ItemID, 2)
	require.ErrorIs(t, err, endpoint.ErrItemNotFound)
	require.ErrorIs(t, s.RemoveItem(ctx, shopper, created.CartID, it.ItemID), endpoint.ErrItemNotFound)
}

func TestStore_deleting_a_cart_deletes_its_items(t *testing.T) {
	s := store(t)
	ctx := t.Context()

	created, err := s.CreateCart(ctx, shopper)
	require.NoError(t, err)
	it, err := s.AddItem(ctx, shopper, created.CartID, endpoint.NewItem{ProductID: "sku-1", Quantity: 1})
	require.NoError(t, err)
	require.NoError(t, s.DeleteCart(ctx, shopper, created.CartID))

	err = s.RemoveItem(ctx, shopper, created.CartID, it.ItemID)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
}

func TestStore_wraps_what_Postgres_rejects(t *testing.T) {
	s := store(t)
	created, err := s.CreateCart(t.Context(), shopper)
	require.NoError(t, err)
	_, err = s.AddItem(t.Context(), shopper, created.CartID, endpoint.NewItem{ProductID: "sku-1", Quantity: 0})
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "adding an item to cart "+created.CartID.String()), err.Error())
	require.NotErrorIs(t, err, endpoint.ErrCartNotFound)
}

func TestStore_keeps_a_cart_to_its_owner(t *testing.T) {
	s := store(t)
	ctx := t.Context()
	const other = "another-shopper"

	created, err := s.CreateCart(ctx, shopper)
	require.NoError(t, err)
	it, err := s.AddItem(ctx, shopper, created.CartID, endpoint.NewItem{ProductID: "sku-1", Quantity: 1, UnitPrice: 1})
	require.NoError(t, err)
	before, err := s.GetCart(ctx, shopper, created.CartID)
	require.NoError(t, err)

	_, err = s.GetCart(ctx, other, created.CartID)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
	require.ErrorIs(t, s.DeleteCart(ctx, other, created.CartID), endpoint.ErrCartNotFound)
	_, err = s.AddItem(ctx, other, created.CartID, endpoint.NewItem{ProductID: "sku-2", Quantity: 1, UnitPrice: 1})
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
	_, err = s.UpdateItem(ctx, other, created.CartID, it.ItemID, 2)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
	require.ErrorIs(t, s.RemoveItem(ctx, other, created.CartID, it.ItemID), endpoint.ErrCartNotFound)

	after, err := s.GetCart(ctx, shopper, created.CartID)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestStore_rejects_a_cart_without_an_owner(t *testing.T) {
	s := store(t)
	_, err := s.CreateCart(t.Context(), "")
	require.Error(t, err)
}
