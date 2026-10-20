// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package database keeps the shoppingcart's carts in PostgreSQL.
package database

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/z5labs/humus/example/shoppingcart/endpoint"
)

//go:embed migrations/*.sql
var Migrations embed.FS

var ErrUnavailable = endpoint.ErrDatabaseUnavailable

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const itemColumns = `id, cart_id, product_id, quantity, unit_price::float8, created_at, updated_at`

func (s *Store) CreateCart(ctx context.Context, owner string) (endpoint.Cart, error) {
	c := endpoint.Cart{Items: []endpoint.CartItem{}}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO carts (owner) VALUES ($1) RETURNING id, created_at, updated_at`, owner,
	).Scan(&c.CartID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return endpoint.Cart{}, wrap("creating a cart", err)
	}
	return c, nil
}

func (s *Store) GetCart(ctx context.Context, owner string, cartID uuid.UUID) (endpoint.Cart, error) {
	var c endpoint.Cart
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT id, created_at, updated_at FROM carts WHERE id = $1 AND owner = $2`, cartID, owner,
		).Scan(&c.CartID, &c.CreatedAt, &c.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return endpoint.ErrCartNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT `+itemColumns+` FROM cart_items
			WHERE cart_id IN (SELECT id FROM carts WHERE id = $1 AND owner = $2)
			ORDER BY created_at, id`,
			cartID, owner,
		)
		if err != nil {
			return err
		}
		c.Items, err = pgx.CollectRows(rows, scanItem)
		return err
	})
	if err != nil {
		return endpoint.Cart{}, wrap(fmt.Sprintf("getting cart %s", cartID), err)
	}
	return c, nil
}

func (s *Store) DeleteCart(ctx context.Context, owner string, cartID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM carts WHERE id = $1 AND owner = $2`, cartID, owner)
	if err != nil {
		return wrap(fmt.Sprintf("deleting cart %s", cartID), err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("deleting cart %s: %w", cartID, endpoint.ErrCartNotFound)
	}
	return nil
}

func (s *Store) AddItem(ctx context.Context, owner string, cartID uuid.UUID, item endpoint.NewItem) (endpoint.CartItem, error) {
	rows, err := s.pool.Query(ctx, `
		WITH cart AS (
			UPDATE carts SET updated_at = now() WHERE id = $1 AND owner = $2 RETURNING id
		)
		INSERT INTO cart_items (cart_id, product_id, quantity, unit_price)
		SELECT cart.id, $3, $4, $5 FROM cart
		RETURNING `+itemColumns,
		cartID, owner, item.ProductID, item.Quantity, item.UnitPrice,
	)
	if err != nil {
		return endpoint.CartItem{}, wrap(fmt.Sprintf("adding an item to cart %s", cartID), err)
	}
	added, err := pgx.CollectExactlyOneRow(rows, scanItem)
	if errors.Is(err, pgx.ErrNoRows) {
		err = endpoint.ErrCartNotFound
	}
	if err != nil {
		return endpoint.CartItem{}, wrap(fmt.Sprintf("adding an item to cart %s", cartID), err)
	}
	return added, nil
}

func (s *Store) UpdateItem(ctx context.Context, owner string, cartID, itemID uuid.UUID, quantity int) (endpoint.CartItem, error) {
	rows, err := s.pool.Query(ctx, `
		WITH item AS (
			UPDATE cart_items SET quantity = $4, updated_at = now()
			WHERE cart_id IN (SELECT id FROM carts WHERE id = $1 AND owner = $2) AND id = $3
			RETURNING `+itemColumns+`
		), cart AS (
			UPDATE carts SET updated_at = now() WHERE id IN (SELECT cart_id FROM item) AND owner = $2
		)
		SELECT * FROM item`,
		cartID, owner, itemID, quantity,
	)
	if err != nil {
		return endpoint.CartItem{}, wrap(fmt.Sprintf("updating item %s of cart %s", itemID, cartID), err)
	}
	updated, err := pgx.CollectExactlyOneRow(rows, scanItem)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.missing(ctx, owner, cartID)
	}
	if err != nil {
		return endpoint.CartItem{}, wrap(fmt.Sprintf("updating item %s of cart %s", itemID, cartID), err)
	}
	return updated, nil
}

func (s *Store) RemoveItem(ctx context.Context, owner string, cartID, itemID uuid.UUID) error {
	var removed int
	err := s.pool.QueryRow(ctx, `
		WITH item AS (
			DELETE FROM cart_items
			WHERE cart_id IN (SELECT id FROM carts WHERE id = $1 AND owner = $2) AND id = $3
			RETURNING cart_id
		), cart AS (
			UPDATE carts SET updated_at = now() WHERE id IN (SELECT cart_id FROM item) AND owner = $2
		)
		SELECT count(*) FROM item`,
		cartID, owner, itemID,
	).Scan(&removed)
	if err == nil && removed == 0 {
		err = s.missing(ctx, owner, cartID)
	}
	if err != nil {
		return wrap(fmt.Sprintf("removing item %s from cart %s", itemID, cartID), err)
	}
	return nil
}

func (s *Store) missing(ctx context.Context, owner string, cartID uuid.UUID) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM carts WHERE id = $1 AND owner = $2)`, cartID, owner).Scan(&exists)
	switch {
	case err != nil:
		return err
	case exists:
		return endpoint.ErrItemNotFound
	default:
		return endpoint.ErrCartNotFound
	}
}

func wrap(op string, err error) error {
	var connect *pgconn.ConnectError
	var network net.Error
	if errors.As(err, &connect) || errors.As(err, &network) {
		return fmt.Errorf("%s: %w: %w", op, ErrUnavailable, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func scanItem(row pgx.CollectableRow) (endpoint.CartItem, error) {
	var it endpoint.CartItem
	err := row.Scan(&it.ItemID, &it.CartID, &it.ProductID, &it.Quantity, &it.UnitPrice, &it.CreatedAt, &it.UpdatedAt)
	return it, err
}
