// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package store keeps the order status projection's events and statuses in PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrApplied = errors.New("event applied already")

type Event struct {
	Partition int32
	Offset    int64
	OrderID   string
	Type      string
	At        time.Time
}

type Status struct {
	Status string
	At     time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Applied(ctx context.Context, partition int32, offset int64) (bool, error) {
	var applied bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM order_events WHERE source_partition = $1 AND source_offset = $2)`,
		partition, offset,
	).Scan(&applied)
	if err != nil {
		return false, fmt.Errorf("finding partition %d, offset %d: %w", partition, offset, err)
	}
	return applied, nil
}

func (s *Store) Events(ctx context.Context, orderID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source_partition, source_offset, order_id, type, at
		FROM order_events
		WHERE order_id = $1
		ORDER BY at, source_offset`,
		orderID,
	)
	if err != nil {
		return nil, fmt.Errorf("reading order %s: %w", orderID, err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.Partition, &e.Offset, &e.OrderID, &e.Type, &e.At)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading order %s: %w", orderID, err)
	}
	return events, nil
}

func (s *Store) Apply(ctx context.Context, e Event, status Status) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO order_status (order_id, status, at) VALUES ($1, $2, $3)
			ON CONFLICT (order_id) DO NOTHING`,
			e.OrderID, status.Status, status.At,
		)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT 1 FROM order_status WHERE order_id = $1 FOR UPDATE`, e.OrderID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO order_events (source_partition, source_offset, order_id, type, at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (source_partition, source_offset) DO NOTHING`,
			e.Partition, e.Offset, e.OrderID, e.Type, e.At,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrApplied
		}
		_, err = tx.Exec(ctx,
			`UPDATE order_status SET status = $2, at = $3 WHERE order_id = $1`,
			e.OrderID, status.Status, status.At,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("storing partition %d, offset %d of order %s: %w", e.Partition, e.Offset, e.OrderID, err)
	}
	return nil
}
