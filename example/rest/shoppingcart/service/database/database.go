// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package database provides a PostgreSQL-backed shopping cart store.
package database

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrCartNotFound is returned when a cart ID does not exist.
	ErrCartNotFound = errors.New("cart not found")

	// ErrItemNotFound is returned when an item ID does not exist in the cart.
	ErrItemNotFound = errors.New("item not found")
)

// Cart represents a shopping cart.
type Cart struct {
	CartID    uuid.UUID  `json:"cartId"`
	Items     []CartItem `json:"items"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// CartItem represents a product line in a cart.
type CartItem struct {
	ItemID    uuid.UUID `json:"itemId"`
	CartID    uuid.UUID `json:"cartId"`
	ProductID string    `json:"productId"`
	Quantity  int       `json:"quantity"`
	UnitPrice float64   `json:"unitPrice"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AddItemRequest carries the fields needed to add an item to a cart.
type AddItemRequest struct {
	ProductID string
	Quantity  int
	UnitPrice float64
}

// Store is the interface that wraps shopping cart persistence operations.
type Store interface {
	CreateCart(ctx context.Context) (Cart, error)
	GetCart(ctx context.Context, cartID uuid.UUID) (Cart, error)
	DeleteCart(ctx context.Context, cartID uuid.UUID) error
	AddCartItem(ctx context.Context, cartID uuid.UUID, req AddItemRequest) (CartItem, error)
	UpdateCartItem(ctx context.Context, cartID, itemID uuid.UUID, quantity int) (CartItem, error)
	RemoveCartItem(ctx context.Context, cartID, itemID uuid.UUID) error
}

// PostgresStore implements Store using a PostgreSQL connection pool.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// New creates a PostgresStore backed by the given connection pool.
func New(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// CreateCart inserts a new cart row and returns it with an empty items slice.
func (s *PostgresStore) CreateCart(ctx context.Context) (Cart, error) {
	const query = `
		INSERT INTO carts (id, created_at, updated_at)
		VALUES (gen_random_uuid(), NOW(), NOW())
		RETURNING id, created_at, updated_at`

	var c Cart
	err := s.pool.QueryRow(ctx, query).Scan(&c.CartID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Cart{}, err
	}
	c.Items = []CartItem{}
	return c, nil
}

// GetCart fetches a cart and all its items by cart ID.
func (s *PostgresStore) GetCart(ctx context.Context, cartID uuid.UUID) (Cart, error) {
	const cartQuery = `
		SELECT id, created_at, updated_at
		FROM carts
		WHERE id = $1`

	var c Cart
	err := s.pool.QueryRow(ctx, cartQuery, cartID).Scan(&c.CartID, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cart{}, ErrCartNotFound
	}
	if err != nil {
		return Cart{}, err
	}

	const itemsQuery = `
		SELECT id, cart_id, product_id, quantity, unit_price, created_at, updated_at
		FROM cart_items
		WHERE cart_id = $1`

	rows, err := s.pool.Query(ctx, itemsQuery, cartID)
	if err != nil {
		return Cart{}, err
	}
	defer rows.Close()

	c.Items = []CartItem{}
	for rows.Next() {
		var item CartItem
		if err := rows.Scan(
			&item.ItemID, &item.CartID, &item.ProductID,
			&item.Quantity, &item.UnitPrice, &item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return Cart{}, err
		}
		c.Items = append(c.Items, item)
	}
	return c, rows.Err()
}

// DeleteCart removes a cart and all its items (via ON DELETE CASCADE).
func (s *PostgresStore) DeleteCart(ctx context.Context, cartID uuid.UUID) error {
	const query = `SELECT id FROM carts WHERE id = $1`

	var id uuid.UUID
	err := s.pool.QueryRow(ctx, query, cartID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCartNotFound
	}
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `DELETE FROM carts WHERE id = $1`, cartID)
	return err
}

// AddCartItem inserts a new item into the cart and bumps the cart's updated_at.
func (s *PostgresStore) AddCartItem(ctx context.Context, cartID uuid.UUID, req AddItemRequest) (CartItem, error) {
	const cartCheck = `SELECT id FROM carts WHERE id = $1`

	var id uuid.UUID
	err := s.pool.QueryRow(ctx, cartCheck, cartID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return CartItem{}, ErrCartNotFound
	}
	if err != nil {
		return CartItem{}, err
	}

	const insertItem = `
		INSERT INTO cart_items (id, cart_id, product_id, quantity, unit_price, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, NOW(), NOW())
		RETURNING id, cart_id, product_id, quantity, unit_price, created_at, updated_at`

	var item CartItem
	err = s.pool.QueryRow(ctx, insertItem, cartID, req.ProductID, req.Quantity, req.UnitPrice).Scan(
		&item.ItemID, &item.CartID, &item.ProductID,
		&item.Quantity, &item.UnitPrice, &item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return CartItem{}, err
	}

	_, err = s.pool.Exec(ctx, `UPDATE carts SET updated_at = NOW() WHERE id = $1`, cartID)
	if err != nil {
		return CartItem{}, err
	}

	return item, nil
}

// UpdateCartItem updates the quantity of an existing cart item.
func (s *PostgresStore) UpdateCartItem(ctx context.Context, cartID, itemID uuid.UUID, quantity int) (CartItem, error) {
	const query = `
		UPDATE cart_items
		SET quantity = $1, updated_at = NOW()
		WHERE id = $2 AND cart_id = $3
		RETURNING id, cart_id, product_id, quantity, unit_price, created_at, updated_at`

	var item CartItem
	err := s.pool.QueryRow(ctx, query, quantity, itemID, cartID).Scan(
		&item.ItemID, &item.CartID, &item.ProductID,
		&item.Quantity, &item.UnitPrice, &item.CreatedAt, &item.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CartItem{}, ErrItemNotFound
	}
	if err != nil {
		return CartItem{}, err
	}
	return item, nil
}

// RemoveCartItem deletes a single item from the cart.
func (s *PostgresStore) RemoveCartItem(ctx context.Context, cartID, itemID uuid.UUID) error {
	const query = `
		DELETE FROM cart_items
		WHERE id = $1 AND cart_id = $2
		RETURNING id`

	var id uuid.UUID
	err := s.pool.QueryRow(ctx, query, itemID, cartID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrItemNotFound
	}
	return err
}
