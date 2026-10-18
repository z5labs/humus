// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package database is the shoppingcart's endpoint.Store on Postgres, in the
// tables schema.sql creates.
//
// It is handed its pool, and never opens or closes one: the end user's Init
// opens the pool through the Host, and registers its close with OnStop.
package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/z5labs/humus/example/shoppingcart/endpoint"
)

// Store is an endpoint.Store on a Postgres pool.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store on pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// itemColumns are the columns scanItem reads, in its order.
const itemColumns = `id, cart_id, product_id, quantity, unit_price::float8, created_at, updated_at`

// CreateCart creates an empty cart.
func (s *Store) CreateCart(ctx context.Context) (endpoint.Cart, error) {
	c := endpoint.Cart{Items: []endpoint.CartItem{}}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO carts DEFAULT VALUES RETURNING id, created_at, updated_at`,
	).Scan(&c.CartID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return endpoint.Cart{}, fmt.Errorf("creating a cart: %w", err)
	}
	return c, nil
}

// GetCart returns a cart and its items, oldest first, read in one
// transaction so the items are the cart's as it was read.
func (s *Store) GetCart(ctx context.Context, cartID uuid.UUID) (endpoint.Cart, error) {
	var c endpoint.Cart
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT id, created_at, updated_at FROM carts WHERE id = $1`, cartID,
		).Scan(&c.CartID, &c.CreatedAt, &c.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return endpoint.ErrCartNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx,
			`SELECT `+itemColumns+` FROM cart_items WHERE cart_id = $1 ORDER BY created_at, id`, cartID,
		)
		if err != nil {
			return err
		}
		c.Items, err = pgx.CollectRows(rows, scanItem)
		return err
	})
	if err != nil {
		return endpoint.Cart{}, fmt.Errorf("getting cart %s: %w", cartID, err)
	}
	return c, nil
}

// DeleteCart deletes a cart, and its items with it.
func (s *Store) DeleteCart(ctx context.Context, cartID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM carts WHERE id = $1`, cartID)
	if err != nil {
		return fmt.Errorf("deleting cart %s: %w", cartID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("deleting cart %s: %w", cartID, endpoint.ErrCartNotFound)
	}
	return nil
}

// AddItem adds an item to a cart, and marks the cart updated, in one
// statement.
func (s *Store) AddItem(ctx context.Context, cartID uuid.UUID, item endpoint.AddItemRequest) (endpoint.CartItem, error) {
	rows, err := s.pool.Query(ctx, `
		WITH cart AS (
			UPDATE carts SET updated_at = now() WHERE id = $1 RETURNING id
		)
		INSERT INTO cart_items (cart_id, product_id, quantity, unit_price)
		SELECT cart.id, $2, $3, $4 FROM cart
		RETURNING `+itemColumns,
		cartID, item.ProductID, item.Quantity, item.UnitPrice,
	)
	if err != nil {
		return endpoint.CartItem{}, fmt.Errorf("adding an item to cart %s: %w", cartID, err)
	}
	added, err := pgx.CollectExactlyOneRow(rows, scanItem)
	if errors.Is(err, pgx.ErrNoRows) {
		err = endpoint.ErrCartNotFound
	}
	if err != nil {
		return endpoint.CartItem{}, fmt.Errorf("adding an item to cart %s: %w", cartID, err)
	}
	return added, nil
}

// UpdateItem sets the quantity of an item in a cart, and marks the cart
// updated, in one statement.
func (s *Store) UpdateItem(ctx context.Context, cartID, itemID uuid.UUID, quantity int) (endpoint.CartItem, error) {
	rows, err := s.pool.Query(ctx, `
		WITH item AS (
			UPDATE cart_items SET quantity = $3, updated_at = now()
			WHERE cart_id = $1 AND id = $2
			RETURNING `+itemColumns+`
		), cart AS (
			UPDATE carts SET updated_at = now() WHERE id IN (SELECT cart_id FROM item)
		)
		SELECT * FROM item`,
		cartID, itemID, quantity,
	)
	if err != nil {
		return endpoint.CartItem{}, fmt.Errorf("updating item %s of cart %s: %w", itemID, cartID, err)
	}
	updated, err := pgx.CollectExactlyOneRow(rows, scanItem)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.missing(ctx, cartID)
	}
	if err != nil {
		return endpoint.CartItem{}, fmt.Errorf("updating item %s of cart %s: %w", itemID, cartID, err)
	}
	return updated, nil
}

// RemoveItem removes an item from a cart, and marks the cart updated, in
// one statement.
func (s *Store) RemoveItem(ctx context.Context, cartID, itemID uuid.UUID) error {
	var removed int
	err := s.pool.QueryRow(ctx, `
		WITH item AS (
			DELETE FROM cart_items WHERE cart_id = $1 AND id = $2 RETURNING cart_id
		), cart AS (
			UPDATE carts SET updated_at = now() WHERE id IN (SELECT cart_id FROM item)
		)
		SELECT count(*) FROM item`,
		cartID, itemID,
	).Scan(&removed)
	if err == nil && removed == 0 {
		err = s.missing(ctx, cartID)
	}
	if err != nil {
		return fmt.Errorf("removing item %s from cart %s: %w", itemID, cartID, err)
	}
	return nil
}

// missing returns why a statement on an item of cartID found no item:
// ErrCartNotFound when there is no such cart, and ErrItemNotFound when
// there is.
func (s *Store) missing(ctx context.Context, cartID uuid.UUID) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM carts WHERE id = $1)`, cartID).Scan(&exists)
	switch {
	case err != nil:
		return err
	case exists:
		return endpoint.ErrItemNotFound
	default:
		return endpoint.ErrCartNotFound
	}
}

// scanItem scans a row of itemColumns.
func scanItem(row pgx.CollectableRow) (endpoint.CartItem, error) {
	var it endpoint.CartItem
	err := row.Scan(&it.ItemID, &it.CartID, &it.ProductID, &it.Quantity, &it.UnitPrice, &it.CreatedAt, &it.UpdatedAt)
	return it, err
}
