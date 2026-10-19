// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package postgres keeps the billing job's invoices in PostgreSQL.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Invoice struct {
	ID          uuid.UUID
	AccountID   string
	Period      time.Time
	AmountCents int64
	Currency    string
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Issue(ctx context.Context, period time.Time) (issued int64, err error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO invoices (account_id, period, amount_cents, currency)
		SELECT account_id, $1::date, amount_cents, currency FROM subscriptions WHERE active
		ON CONFLICT (account_id, period) DO NOTHING`,
		period,
	)
	if err != nil {
		return 0, fmt.Errorf("issuing %s: %w", period.Format("2006-01"), err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) Pending(ctx context.Context) ([]Invoice, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, period, amount_cents, currency FROM invoices
		WHERE state = 'pending'
		ORDER BY period, issued_at, account_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("reading pending invoices: %w", err)
	}
	invoices, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invoice, error) {
		var inv Invoice
		err := row.Scan(&inv.ID, &inv.AccountID, &inv.Period, &inv.AmountCents, &inv.Currency)
		return inv, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading pending invoices: %w", err)
	}
	return invoices, nil
}

func (s *Store) SettleCharged(ctx context.Context, id uuid.UUID, chargeID string) (settled bool, err error) {
	return s.settle(ctx, id, "charged", `charge_id`, chargeID)
}

func (s *Store) SettleDeclined(ctx context.Context, id uuid.UUID, declineCode string) (settled bool, err error) {
	return s.settle(ctx, id, "declined", `decline_code`, declineCode)
}

func (s *Store) SettleFailed(ctx context.Context, id uuid.UUID, answer string) (settled bool, err error) {
	return s.settle(ctx, id, "failed", `answer`, answer)
}

func (s *Store) settle(ctx context.Context, id uuid.UUID, state, column, value string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE invoices SET state = $2, `+column+` = $3, settled_at = now() WHERE id = $1 AND state = 'pending'`,
		id, state, value,
	)
	if err != nil {
		return false, fmt.Errorf("settling invoice %s as %s: %w", id, state, err)
	}
	return tag.RowsAffected() == 1, nil
}
