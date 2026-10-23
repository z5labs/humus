// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package database keeps the catalog's products in PostgreSQL.
package database

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/z5labs/humus/example/http/shoppingcart/catalog/endpoint"
	"github.com/z5labs/humus/fault"
)

//go:embed migrations/*.sql
var Migrations embed.FS

const Point = "postgresql:catalog"

type Store struct {
	pool   *pgxpool.Pool
	faults fault.Injector
}

func New(pool *pgxpool.Pool, faults fault.Injector) *Store {
	return &Store{pool: pool, faults: faults}
}

func (s *Store) inject(ctx context.Context) error {
	f := s.faults.Eval(ctx, Point)
	if err := f.Wait(ctx); err != nil {
		return &fault.InjectedError{Point: Point, Err: err}
	}
	if f.Abort {
		return fmt.Errorf("%w: %w", endpoint.ErrUnavailable, &fault.InjectedError{Point: Point})
	}
	return nil
}

func (s *Store) GetProduct(ctx context.Context, productID string) (endpoint.Product, error) {
	if err := s.inject(ctx); err != nil {
		return endpoint.Product{}, wrap("getting product "+productID, err)
	}
	p := endpoint.Product{ProductID: productID}
	err := s.pool.QueryRow(ctx, `SELECT unit_price::float8 FROM products WHERE id = $1`, productID).Scan(&p.UnitPrice)
	if errors.Is(err, pgx.ErrNoRows) {
		err = endpoint.ErrProductNotFound
	}
	if err != nil {
		return endpoint.Product{}, wrap("getting product "+productID, err)
	}
	return p, nil
}

func (s *Store) PutProduct(ctx context.Context, product endpoint.Product) (endpoint.Product, error) {
	if err := s.inject(ctx); err != nil {
		return endpoint.Product{}, wrap("putting product "+product.ProductID, err)
	}
	p := endpoint.Product{ProductID: product.ProductID}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO products (id, unit_price) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET unit_price = EXCLUDED.unit_price
		RETURNING unit_price::float8`,
		product.ProductID, product.UnitPrice,
	).Scan(&p.UnitPrice)
	if err != nil {
		return endpoint.Product{}, wrap("putting product "+product.ProductID, err)
	}
	return p, nil
}

func wrap(op string, err error) error {
	var connect *pgconn.ConnectError
	var network net.Error
	if errors.As(err, &connect) || errors.As(err, &network) {
		return fmt.Errorf("%s: %w: %w", op, endpoint.ErrUnavailable, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}
