// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package store keeps the ledger's accounts and transfers in PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/z5labs/humus/fault"
)

const External = "external"

const point = "postgresql:ledger"

var (
	ErrNotFound    = errors.New("no such account")
	ErrNotPositive = errors.New("amount isn't positive")
	ErrSameAccount = errors.New("account sent to itself")
	ErrReusedID    = errors.New("transfer ID reused for another transfer")
	ErrUnavailable = errors.New("database unavailable")
)

type ShortBalanceError struct {
	AccountID    string
	BalanceCents int64
	AmountCents  int64
}

func (e *ShortBalanceError) Error() string {
	return fmt.Sprintf("account %s has %d cents, short of %d", e.AccountID, e.BalanceCents, e.AmountCents)
}

type Account struct {
	ID               string
	BalanceCents     int64
	BalanceChangedAt time.Time
}

type Transfer struct {
	ID            string
	FromAccountID string
	ToAccountID   string
	AmountCents   int64
	CreatedAt     time.Time
	Replayed      bool
}

type Store struct {
	pool   *pgxpool.Pool
	faults fault.Injector
}

func New(pool *pgxpool.Pool, faults fault.Injector) *Store {
	return &Store{pool: pool, faults: faults}
}

func (s *Store) inject(ctx context.Context) error {
	f := s.faults.Eval(ctx, point)
	if err := f.Wait(ctx); err != nil {
		return &fault.InjectedError{Point: point, Err: err}
	}
	if f.Abort {
		return &fault.InjectedError{Point: point}
	}
	return nil
}

func unavailable(err error) error {
	var short *ShortBalanceError
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrReusedID), errors.As(err, &short), errors.Is(err, ErrUnavailable):
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func (s *Store) OpenAccount(ctx context.Context, id string) (Account, error) {
	if err := s.inject(ctx); err != nil {
		return Account{}, fmt.Errorf("opening account %s: %w", id, unavailable(err))
	}
	var a Account
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO accounts (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`, id)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT id, balance_cents, balance_changed_at FROM accounts WHERE id = $1`, id,
		).Scan(&a.ID, &a.BalanceCents, &a.BalanceChangedAt)
	})
	if err != nil {
		return Account{}, fmt.Errorf("opening account %s: %w", id, unavailable(err))
	}
	return a, nil
}

func (s *Store) Account(ctx context.Context, id string) (Account, error) {
	if err := s.inject(ctx); err != nil {
		return Account{}, fmt.Errorf("reading account %s: %w", id, unavailable(err))
	}
	var a Account
	err := s.pool.QueryRow(ctx,
		`SELECT id, balance_cents, balance_changed_at FROM accounts WHERE id = $1`, id,
	).Scan(&a.ID, &a.BalanceCents, &a.BalanceChangedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("reading account %s: %w", id, unavailable(err))
	}
	return a, nil
}

func (s *Store) Deposit(ctx context.Context, transferID, accountID string, amountCents int64) (Transfer, error) {
	t, err := s.move(ctx, Transfer{ID: transferID, FromAccountID: External, ToAccountID: accountID, AmountCents: amountCents}, false)
	if err != nil {
		return Transfer{}, fmt.Errorf("depositing %s into account %s: %w", transferID, accountID, err)
	}
	return t, nil
}

func (s *Store) Transfer(ctx context.Context, transferID, fromAccountID, toAccountID string, amountCents int64) (Transfer, error) {
	t, err := s.move(ctx, Transfer{ID: transferID, FromAccountID: fromAccountID, ToAccountID: toAccountID, AmountCents: amountCents}, true)
	if err != nil {
		return Transfer{}, fmt.Errorf("transferring %s from account %s to account %s: %w", transferID, fromAccountID, toAccountID, err)
	}
	return t, nil
}

func (s *Store) move(ctx context.Context, want Transfer, checkBalance bool) (Transfer, error) {
	if want.AmountCents <= 0 {
		return Transfer{}, ErrNotPositive
	}
	if want.FromAccountID == want.ToAccountID {
		return Transfer{}, ErrSameAccount
	}
	if err := s.inject(ctx); err != nil {
		return Transfer{}, unavailable(err)
	}
	var t Transfer
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO transfers (id, from_account_id, to_account_id, amount_cents) VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO NOTHING
			RETURNING id, from_account_id, to_account_id, amount_cents, created_at`,
			want.ID, want.FromAccountID, want.ToAccountID, want.AmountCents,
		).Scan(&t.ID, &t.FromAccountID, &t.ToAccountID, &t.AmountCents, &t.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return replay(ctx, tx, want, &t)
		}
		if err != nil {
			return err
		}
		return post(ctx, tx, t, checkBalance)
	})
	if err != nil {
		return Transfer{}, unavailable(err)
	}
	return t, nil
}

func replay(ctx context.Context, tx pgx.Tx, want Transfer, t *Transfer) error {
	err := tx.QueryRow(ctx,
		`SELECT id, from_account_id, to_account_id, amount_cents, created_at FROM transfers WHERE id = $1`, want.ID,
	).Scan(&t.ID, &t.FromAccountID, &t.ToAccountID, &t.AmountCents, &t.CreatedAt)
	if err != nil {
		return err
	}
	if t.FromAccountID != want.FromAccountID || t.ToAccountID != want.ToAccountID || t.AmountCents != want.AmountCents {
		return ErrReusedID
	}
	t.Replayed = true
	return nil
}

func post(ctx context.Context, tx pgx.Tx, t Transfer, checkBalance bool) error {
	rows, err := tx.Query(ctx,
		`SELECT id, balance_cents FROM accounts WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`,
		t.FromAccountID, t.ToAccountID,
	)
	if err != nil {
		return err
	}
	balances := make(map[string]int64, 2)
	var (
		id      string
		balance int64
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &balance}, func() error {
		balances[id] = balance
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range []string{t.FromAccountID, t.ToAccountID} {
		if _, ok := balances[id]; !ok {
			return fmt.Errorf("account %s: %w", id, ErrNotFound)
		}
	}
	if from := balances[t.FromAccountID]; checkBalance && from < t.AmountCents {
		return &ShortBalanceError{AccountID: t.FromAccountID, BalanceCents: from, AmountCents: t.AmountCents}
	}
	_, err = tx.Exec(ctx, `
		UPDATE accounts
		SET balance_cents = balance_cents + CASE WHEN id = $1 THEN -$3::bigint ELSE $3::bigint END,
		    balance_changed_at = $4
		WHERE id IN ($1, $2)`,
		t.FromAccountID, t.ToAccountID, t.AmountCents, t.CreatedAt,
	)
	return err
}
