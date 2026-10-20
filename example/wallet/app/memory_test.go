// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/wallet/ledger/server"
	"github.com/z5labs/humus/example/wallet/ledger/store"
	"github.com/z5labs/humus/fault"
)

const ledgerPoint = "postgresql:ledger"

type memoryLedger struct {
	mu        sync.Mutex
	accounts  map[string]store.Account
	transfers map[string]store.Transfer
}

func newMemoryLedger() *memoryLedger {
	return &memoryLedger{
		accounts:  map[string]store.Account{store.External: {ID: store.External, BalanceChangedAt: time.Now()}},
		transfers: map[string]store.Transfer{},
	}
}

func (m *memoryLedger) open(_ context.Context, s *humus.Setup, _ *pgxpool.Config) (server.Store, func(context.Context) error, error) {
	return &memoryStore{ledger: m, faults: s.Faults}, func(context.Context) error { return nil }, nil
}

type memoryStore struct {
	ledger *memoryLedger
	faults fault.Injector
}

func (s *memoryStore) inject(ctx context.Context) error {
	f := s.faults.Eval(ctx, ledgerPoint)
	if err := f.Wait(ctx); err != nil {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, &fault.InjectedError{Point: ledgerPoint, Err: err})
	}
	if f.Abort {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, &fault.InjectedError{Point: ledgerPoint})
	}
	return nil
}

func (s *memoryStore) OpenAccount(ctx context.Context, id string) (store.Account, error) {
	if err := s.inject(ctx); err != nil {
		return store.Account{}, fmt.Errorf("opening account %s: %w", id, err)
	}
	m := s.ledger
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		a = store.Account{ID: id, BalanceChangedAt: time.Now()}
		m.accounts[id] = a
	}
	return a, nil
}

func (s *memoryStore) Account(ctx context.Context, id string) (store.Account, error) {
	if err := s.inject(ctx); err != nil {
		return store.Account{}, fmt.Errorf("reading account %s: %w", id, err)
	}
	m := s.ledger
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return store.Account{}, fmt.Errorf("reading account %s: %w", id, store.ErrNotFound)
	}
	return a, nil
}

func (s *memoryStore) Deposit(ctx context.Context, transferID, accountID string, amountCents int64) (store.Transfer, error) {
	t, err := s.move(ctx, store.Transfer{ID: transferID, FromAccountID: store.External, ToAccountID: accountID, AmountCents: amountCents}, false)
	if err != nil {
		return store.Transfer{}, fmt.Errorf("depositing %s into account %s: %w", transferID, accountID, err)
	}
	return t, nil
}

func (s *memoryStore) Transfer(ctx context.Context, transferID, fromAccountID, toAccountID string, amountCents int64) (store.Transfer, error) {
	t, err := s.move(ctx, store.Transfer{ID: transferID, FromAccountID: fromAccountID, ToAccountID: toAccountID, AmountCents: amountCents}, true)
	if err != nil {
		return store.Transfer{}, fmt.Errorf("transferring %s from account %s to account %s: %w", transferID, fromAccountID, toAccountID, err)
	}
	return t, nil
}

func (s *memoryStore) move(ctx context.Context, want store.Transfer, checkBalance bool) (store.Transfer, error) {
	if want.AmountCents <= 0 {
		return store.Transfer{}, store.ErrNotPositive
	}
	if want.FromAccountID == want.ToAccountID {
		return store.Transfer{}, store.ErrSameAccount
	}
	if err := s.inject(ctx); err != nil {
		return store.Transfer{}, err
	}
	m := s.ledger
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.transfers[want.ID]; ok {
		if t.FromAccountID != want.FromAccountID || t.ToAccountID != want.ToAccountID || t.AmountCents != want.AmountCents {
			return store.Transfer{}, store.ErrReusedID
		}
		return t, nil
	}
	from, ok := m.accounts[want.FromAccountID]
	if !ok {
		return store.Transfer{}, fmt.Errorf("account %s: %w", want.FromAccountID, store.ErrNotFound)
	}
	to, ok := m.accounts[want.ToAccountID]
	if !ok {
		return store.Transfer{}, fmt.Errorf("account %s: %w", want.ToAccountID, store.ErrNotFound)
	}
	if checkBalance && from.BalanceCents < want.AmountCents {
		return store.Transfer{}, &store.ShortBalanceError{AccountID: from.ID, BalanceCents: from.BalanceCents, AmountCents: want.AmountCents}
	}
	want.CreatedAt = time.Now()
	from.BalanceCents -= want.AmountCents
	from.BalanceChangedAt = want.CreatedAt
	to.BalanceCents += want.AmountCents
	to.BalanceChangedAt = want.CreatedAt
	m.accounts[from.ID] = from
	m.accounts[to.ID] = to
	m.transfers[want.ID] = want
	return want, nil
}
