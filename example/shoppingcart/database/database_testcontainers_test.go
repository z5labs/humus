//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package database_test

// The tests in this file run the Store against Postgres itself, in a
// container started with testcontainers-go, with the schema applied from
// schema.sql. They open real sockets, so they sit behind the testcontainers
// build tag and run in a CI job of their own, outside the coverage gate:
//
//	go -C example/shoppingcart test -race -tags testcontainers ./...

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/z5labs/humus/example/shoppingcart/database"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
)

// postgresImage is the Postgres the tests run, the official image pinned to
// an exact release.
const postgresImage = "docker.io/library/postgres:18.0"

// postgres is the one Postgres every test in the package shares, started by
// the first test that needs it and stopped by TestMain.
var postgres struct {
	once      sync.Once
	url       string
	container *tcpostgres.PostgresContainer
	err       error
}

func TestMain(m *testing.M) {
	m.Run()
	if postgres.container != nil {
		// The tests have finished; a container left behind is reaped by
		// testcontainers' own reaper.
		_ = postgres.container.Terminate(context.Background())
	}
}

// store returns a Store on a pool of the shared Postgres, which the test
// closes when it ends.
func store(t *testing.T) *database.Store {
	t.Helper()
	postgres.once.Do(func() {
		postgres.err = errors.New("starting Postgres did not finish")
		postgres.err = startPostgres()
	})
	require.NoError(t, postgres.err, "starting Postgres in a container")

	pool, err := pgxpool.New(t.Context(), postgres.url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return database.New(pool)
}

// startPostgres starts Postgres with a password new for each run, applies
// schema.sql, and fills in postgres.
func startPostgres() error {
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("carts"),
		tcpostgres.WithUsername("carts"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.WithInitScripts("schema.sql"),
		tcpostgres.BasicWaitStrategies(),
	)
	if c != nil {
		postgres.container = c
	}
	if err != nil {
		return err
	}
	// The URL sets every field pgx uses, so pgx reads nothing from the
	// real PG* variables or ~/.pgpass.
	postgres.url, err = c.ConnectionString(ctx, "sslmode=disable")
	return err
}

func TestStore_keeps_a_cart_and_its_items(t *testing.T) {
	s := store(t)
	ctx := t.Context()

	created, err := s.CreateCart(ctx)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, created.CartID)
	require.Empty(t, created.Items)
	require.NotNil(t, created.Items)
	require.WithinDuration(t, time.Now(), created.CreatedAt, time.Minute)

	got, err := s.GetCart(ctx, created.CartID)
	require.NoError(t, err)
	require.Equal(t, created.CartID, got.CartID)
	require.Empty(t, got.Items)

	first, err := s.AddItem(ctx, created.CartID, endpoint.AddItemRequest{ProductID: "sku-1", Quantity: 2, UnitPrice: 9.99})
	require.NoError(t, err)
	require.Equal(t, created.CartID, first.CartID)
	require.Equal(t, "sku-1", first.ProductID)
	require.Equal(t, 2, first.Quantity)
	require.InDelta(t, 9.99, first.UnitPrice, 1e-9)
	second, err := s.AddItem(ctx, created.CartID, endpoint.AddItemRequest{ProductID: "sku-2", Quantity: 1, UnitPrice: 0})
	require.NoError(t, err)

	updated, err := s.UpdateItem(ctx, created.CartID, first.ItemID, 5)
	require.NoError(t, err)
	require.Equal(t, first.ItemID, updated.ItemID)
	require.Equal(t, 5, updated.Quantity)
	require.False(t, updated.UpdatedAt.Before(first.UpdatedAt))

	got, err = s.GetCart(ctx, created.CartID)
	require.NoError(t, err)
	require.Len(t, got.Items, 2)
	require.Equal(t, first.ItemID, got.Items[0].ItemID, "oldest first")
	require.Equal(t, 5, got.Items[0].Quantity)
	require.Equal(t, second.ItemID, got.Items[1].ItemID)
	require.False(t, got.UpdatedAt.Before(updated.UpdatedAt), "a change to an item updates its cart")

	require.NoError(t, s.RemoveItem(ctx, created.CartID, first.ItemID))
	got, err = s.GetCart(ctx, created.CartID)
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	require.Equal(t, second.ItemID, got.Items[0].ItemID)

	require.NoError(t, s.DeleteCart(ctx, created.CartID))
	_, err = s.GetCart(ctx, created.CartID)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
}

func TestStore_reports_what_doesnt_exist(t *testing.T) {
	s := store(t)
	ctx := t.Context()
	missing := uuid.New()

	_, err := s.GetCart(ctx, missing)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
	require.ErrorIs(t, s.DeleteCart(ctx, missing), endpoint.ErrCartNotFound)
	_, err = s.AddItem(ctx, missing, endpoint.AddItemRequest{ProductID: "sku-1", Quantity: 1})
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
	_, err = s.UpdateItem(ctx, missing, uuid.New(), 1)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
	require.ErrorIs(t, s.RemoveItem(ctx, missing, uuid.New()), endpoint.ErrCartNotFound)

	// A cart that exists, without the item.
	created, err := s.CreateCart(ctx)
	require.NoError(t, err)
	_, err = s.UpdateItem(ctx, created.CartID, uuid.New(), 1)
	require.ErrorIs(t, err, endpoint.ErrItemNotFound)
	require.ErrorIs(t, s.RemoveItem(ctx, created.CartID, uuid.New()), endpoint.ErrItemNotFound)

	// An item of another cart is not this cart's.
	other, err := s.CreateCart(ctx)
	require.NoError(t, err)
	it, err := s.AddItem(ctx, other.CartID, endpoint.AddItemRequest{ProductID: "sku-1", Quantity: 1})
	require.NoError(t, err)
	_, err = s.UpdateItem(ctx, created.CartID, it.ItemID, 2)
	require.ErrorIs(t, err, endpoint.ErrItemNotFound)
	require.ErrorIs(t, s.RemoveItem(ctx, created.CartID, it.ItemID), endpoint.ErrItemNotFound)
}

func TestStore_deleting_a_cart_deletes_its_items(t *testing.T) {
	s := store(t)
	ctx := t.Context()

	created, err := s.CreateCart(ctx)
	require.NoError(t, err)
	it, err := s.AddItem(ctx, created.CartID, endpoint.AddItemRequest{ProductID: "sku-1", Quantity: 1})
	require.NoError(t, err)
	require.NoError(t, s.DeleteCart(ctx, created.CartID))

	err = s.RemoveItem(ctx, created.CartID, it.ItemID)
	require.ErrorIs(t, err, endpoint.ErrCartNotFound)
}

func TestStore_wraps_what_Postgres_rejects(t *testing.T) {
	// The schema's CHECK rejects a quantity the API answers with a 422
	// before the Store sees it. The Store passes Postgres's error on,
	// wrapped, for the catch-all to answer.
	s := store(t)
	created, err := s.CreateCart(t.Context())
	require.NoError(t, err)
	_, err = s.AddItem(t.Context(), created.CartID, endpoint.AddItemRequest{ProductID: "sku-1", Quantity: 0})
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "adding an item to cart "+created.CartID.String()), err.Error())
	require.NotErrorIs(t, err, endpoint.ErrCartNotFound)
}
