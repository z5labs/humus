//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/z5labs/bedrock/fit"

	"github.com/z5labs/humus/example/wallet/ledger/store"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
)

const postgresImage = "docker.io/library/postgres:18.0"

var shared struct {
	once      sync.Once
	container *tcpostgres.PostgresContainer
	err       error
}

func TestMain(m *testing.M) {
	m.Run()
	if shared.container != nil {
		_ = shared.container.Terminate(context.Background())
	}
}

func sharedPostgres(t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()
	shared.once.Do(func() {
		shared.container, shared.err = tcpostgres.Run(context.Background(), postgresImage,
			tcpostgres.WithDatabase("postgres"),
			tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword(rand.Text()),
			tcpostgres.BasicWaitStrategies(),
		)
	})
	require.NoError(t, shared.err, "starting PostgreSQL in a container")
	return shared.container
}

type ledger struct {
	pool  *pgxpool.Pool
	store *store.Store
	codec jsonscenario.Codec
}

func newLedger(t *testing.T) ledger {
	t.Helper()
	ctx := t.Context()
	c := sharedPostgres(t)

	adminURL, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	admin, err := pgx.Connect(ctx, adminURL)
	require.NoError(t, err)
	defer func() { _ = admin.Close(context.Background()) }()

	name := "ledger_" + strings.ToLower(rand.Text())
	password := rand.Text()
	ident := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, ident, password))
	require.NoError(t, err)
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, ident, ident))
	require.NoError(t, err)

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", name, password, net.JoinHostPort(host, port.Port()), name))
	require.NoError(t, err)
	cfg.MaxConns = 32

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(schema))
	require.NoError(t, err)

	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	fs := fault.DefaultSettings()
	fs.Decoder = codec
	faults, err := fault.New(fs)
	require.NoError(t, err)
	return ledger{pool: pool, store: store.New(pool, faults), codec: codec}
}

func (l ledger) open(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		_, err := l.store.OpenAccount(t.Context(), id)
		require.NoError(t, err)
	}
}

func (l ledger) fund(t *testing.T, id string, amountCents int64) {
	t.Helper()
	_, err := l.store.Deposit(t.Context(), "fund-"+rand.Text(), id, amountCents)
	require.NoError(t, err)
}

func (l ledger) balance(t *testing.T, id string) int64 {
	t.Helper()
	a, err := l.store.Account(t.Context(), id)
	require.NoError(t, err)
	return a.BalanceCents
}

func (l ledger) requireBalancesSumToZero(t *testing.T) {
	t.Helper()
	var sum int64
	err := l.pool.QueryRow(t.Context(), `SELECT coalesce(sum(balance_cents), 0)::bigint FROM accounts`).Scan(&sum)
	require.NoError(t, err)
	require.Zero(t, sum, "the ledger's balances sum to zero")
}

func (l ledger) transfers(t *testing.T, id string) int {
	t.Helper()
	var n int
	err := l.pool.QueryRow(t.Context(), `SELECT count(*) FROM transfers WHERE id = $1`, id).Scan(&n)
	require.NoError(t, err)
	return n
}

func (l ledger) waitingOnLocks(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		err := l.pool.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`,
		).Scan(&waiting)
		return err == nil && waiting == n
	}, 30*time.Second, 10*time.Millisecond, "%d transactions waiting on a lock", n)
}

func requireShort(t *testing.T, err error, accountID string) {
	t.Helper()
	var short *store.ShortBalanceError
	require.ErrorAs(t, err, &short)
	require.Equal(t, accountID, short.AccountID)
	require.NotErrorIs(t, err, store.ErrUnavailable)
}

func TestStore_opens_an_account_once_with_a_zero_balance(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()

	opened, err := l.store.OpenAccount(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, "alice", opened.ID)
	require.Zero(t, opened.BalanceCents)
	require.False(t, opened.BalanceChangedAt.IsZero())

	l.fund(t, "alice", 700)
	funded, err := l.store.Account(ctx, "alice")
	require.NoError(t, err)

	again, err := l.store.OpenAccount(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, funded, again, "an account already open is answered as it is")
	require.EqualValues(t, 700, again.BalanceCents)

	_, err = l.store.Account(ctx, "bob")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NotErrorIs(t, err, store.ErrUnavailable)
}

func TestStore_deposits_as_a_transfer_from_external(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()
	l.open(t, "alice")

	deposit, err := l.store.Deposit(ctx, "d-1", "alice", 1200)
	require.NoError(t, err)
	require.Equal(t, "d-1", deposit.ID)
	require.Equal(t, store.External, deposit.FromAccountID)
	require.Equal(t, "alice", deposit.ToAccountID)
	require.EqualValues(t, 1200, deposit.AmountCents)

	alice, err := l.store.Account(ctx, "alice")
	require.NoError(t, err)
	require.EqualValues(t, 1200, alice.BalanceCents)
	require.True(t, alice.BalanceChangedAt.Equal(deposit.CreatedAt))
	require.EqualValues(t, -1200, l.balance(t, store.External))

	again, err := l.store.Deposit(ctx, "d-1", "alice", 1200)
	require.NoError(t, err)
	require.Equal(t, deposit, again)
	require.EqualValues(t, 1200, l.balance(t, "alice"))

	_, err = l.store.Deposit(ctx, "d-2", "bob", 1200)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.Zero(t, l.transfers(t, "d-2"), "a deposit refused isn't kept")
	l.requireBalancesSumToZero(t)
}

func TestStore_transfers_between_two_accounts(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()
	l.open(t, "alice", "bob")
	l.fund(t, "alice", 1000)

	sent, err := l.store.Transfer(ctx, "t-1", "alice", "bob", 400)
	require.NoError(t, err)
	require.Equal(t, store.Transfer{ID: "t-1", FromAccountID: "alice", ToAccountID: "bob", AmountCents: 400, CreatedAt: sent.CreatedAt}, sent)
	require.EqualValues(t, 600, l.balance(t, "alice"))
	require.EqualValues(t, 400, l.balance(t, "bob"))

	bob, err := l.store.Account(ctx, "bob")
	require.NoError(t, err)
	require.True(t, bob.BalanceChangedAt.Equal(sent.CreatedAt))
	l.requireBalancesSumToZero(t)
}

func TestStore_answers_a_transfer_already_made_as_it_was_made(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()
	l.open(t, "alice", "bob", "carol")
	l.fund(t, "alice", 1000)

	sent, err := l.store.Transfer(ctx, "t-1", "alice", "bob", 400)
	require.NoError(t, err)

	again, err := l.store.Transfer(ctx, "t-1", "alice", "bob", 400)
	require.NoError(t, err)
	require.Equal(t, sent, again)
	require.EqualValues(t, 600, l.balance(t, "alice"))
	require.EqualValues(t, 400, l.balance(t, "bob"))

	testCases := []struct {
		name     string
		from, to string
		amount   int64
	}{
		{name: "another amount", from: "alice", to: "bob", amount: 300},
		{name: "another destination", from: "alice", to: "carol", amount: 400},
		{name: "another source", from: "bob", to: "carol", amount: 400},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := l.store.Transfer(ctx, "t-1", tc.from, tc.to, tc.amount)
			require.ErrorIs(t, err, store.ErrReusedID)
			require.NotErrorIs(t, err, store.ErrUnavailable)
		})
	}

	_, err = l.store.Deposit(ctx, "t-1", "bob", 400)
	require.ErrorIs(t, err, store.ErrReusedID)

	require.EqualValues(t, 600, l.balance(t, "alice"))
	require.EqualValues(t, 400, l.balance(t, "bob"))
	require.Zero(t, l.balance(t, "carol"))
	l.requireBalancesSumToZero(t)
}

func TestStore_refuses_a_transfer_and_keeps_nothing(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()
	l.open(t, "alice", "bob")
	l.fund(t, "alice", 300)

	testCases := []struct {
		name     string
		from, to string
		amount   int64
		check    func(t *testing.T, err error)
	}{
		{
			name: "a short balance", from: "alice", to: "bob", amount: 301,
			check: func(t *testing.T, err error) { requireShort(t, err, "alice") },
		},
		{
			name: "from external", from: store.External, to: "bob", amount: 1,
			check: func(t *testing.T, err error) { requireShort(t, err, store.External) },
		},
		{
			name: "no such source", from: "dave", to: "bob", amount: 1,
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, store.ErrNotFound) },
		},
		{
			name: "no such destination", from: "alice", to: "dave", amount: 1,
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, store.ErrNotFound) },
		},
		{
			name: "nothing", from: "alice", to: "bob", amount: 0,
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, store.ErrNotPositive) },
		},
		{
			name: "to itself", from: "alice", to: "alice", amount: 1,
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, store.ErrSameAccount) },
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			id := "t-" + rand.Text()
			_, err := l.store.Transfer(ctx, id, tc.from, tc.to, tc.amount)
			tc.check(t, err)
			require.Zero(t, l.transfers(t, id), "a refused transfer isn't kept")
		})
	}
	require.EqualValues(t, 300, l.balance(t, "alice"))
	require.Zero(t, l.balance(t, "bob"))
	l.requireBalancesSumToZero(t)
}

func TestStore_a_transfer_refused_for_its_balance_can_be_sent_again_once_the_money_is_there(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()
	l.open(t, "alice", "bob")

	_, err := l.store.Transfer(ctx, "t-1", "alice", "bob", 500)
	requireShort(t, err, "alice")

	l.fund(t, "alice", 500)
	sent, err := l.store.Transfer(ctx, "t-1", "alice", "bob", 500)
	require.NoError(t, err)
	require.Equal(t, "t-1", sent.ID)
	require.Zero(t, l.balance(t, "alice"))
	require.EqualValues(t, 500, l.balance(t, "bob"))
}

func TestStore_concurrent_transfers_from_one_account_never_overdraw_it(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()
	l.open(t, "alice")
	l.fund(t, "alice", 500)

	const sends = 20
	errs := make(chan error, sends)
	for i := range sends {
		to := fmt.Sprintf("payee-%d", i)
		l.open(t, to)
		go func() {
			_, err := l.store.Transfer(ctx, "t-"+rand.Text(), "alice", to, 100)
			errs <- err
		}()
	}

	var sent, short int
	for range sends {
		err := <-errs
		if err == nil {
			sent++
			continue
		}
		requireShort(t, err, "alice")
		short++
	}
	require.Equal(t, 5, sent)
	require.Equal(t, 15, short)
	require.Zero(t, l.balance(t, "alice"))

	var kept int
	err := l.pool.QueryRow(ctx, `SELECT count(*) FROM transfers WHERE from_account_id = 'alice'`).Scan(&kept)
	require.NoError(t, err)
	require.Equal(t, 5, kept)
	l.requireBalancesSumToZero(t)
}

func TestStore_a_repeated_ID_moves_money_once(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()
	l.open(t, "alice", "bob")
	l.fund(t, "alice", 1000)

	const repeats = 10
	type answer struct {
		transfer store.Transfer
		err      error
	}
	answers := make(chan answer, repeats)
	for range repeats {
		go func() {
			sent, err := l.store.Transfer(ctx, "t-1", "alice", "bob", 300)
			answers <- answer{sent, err}
		}()
	}

	var first store.Transfer
	for i := range repeats {
		a := <-answers
		require.NoError(t, a.err)
		if i == 0 {
			first = a.transfer
		}
		require.Equal(t, first, a.transfer)
	}
	require.EqualValues(t, 700, l.balance(t, "alice"))
	require.EqualValues(t, 300, l.balance(t, "bob"))
	require.Equal(t, 1, l.transfers(t, "t-1"))
	l.requireBalancesSumToZero(t)
}

func TestStore_two_transfers_each_way_between_two_accounts_dont_deadlock(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()
	l.open(t, "alice", "bob")
	l.fund(t, "alice", 1000)
	l.fund(t, "bob", 1000)

	for round := range 5 {
		holder, err := l.pool.Begin(ctx)
		require.NoError(t, err)
		_, err = holder.Exec(ctx, `SELECT 1 FROM accounts WHERE id IN ('alice', 'bob') FOR UPDATE`)
		require.NoError(t, err)

		errs := make(chan error, 4)
		for i, way := range [][2]string{{"alice", "bob"}, {"bob", "alice"}, {"alice", "bob"}, {"bob", "alice"}} {
			go func() {
				_, err := l.store.Transfer(ctx, fmt.Sprintf("t-%d-%d", round, i), way[0], way[1], 10)
				errs <- err
			}()
		}
		l.waitingOnLocks(t, 4)
		require.NoError(t, holder.Rollback(ctx))

		for range 4 {
			err := <-errs
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				require.NotEqual(t, "40P01", pgErr.Code, "a deadlock")
			}
			require.NoError(t, err)
		}
	}
	require.EqualValues(t, 1000, l.balance(t, "alice"))
	require.EqualValues(t, 1000, l.balance(t, "bob"))
	l.requireBalancesSumToZero(t)
}

func TestStore_the_check_refuses_a_negative_balance(t *testing.T) {
	l := newLedger(t)
	ctx := t.Context()
	l.open(t, "alice")
	l.fund(t, "alice", 100)

	requireCheck := func(t *testing.T, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "23514", pgErr.Code, "a check violation")
	}

	_, err := l.pool.Exec(ctx, `UPDATE accounts SET balance_cents = -1 WHERE id = 'alice'`)
	requireCheck(t, err)
	_, err = l.pool.Exec(ctx, `INSERT INTO accounts (id, balance_cents) VALUES ('bob', -1)`)
	requireCheck(t, err)
	require.EqualValues(t, 100, l.balance(t, "alice"))

	_, err = l.pool.Exec(ctx, `UPDATE accounts SET balance_cents = balance_cents - 1 WHERE id = 'external'`)
	require.NoError(t, err, "external goes below zero")
}

func TestStore_an_abort_at_postgresql_ledger_changes_nothing(t *testing.T) {
	l := newLedger(t)
	l.open(t, "alice", "bob")
	l.fund(t, "alice", 500)
	ctx, err := fit.ContextWithScenario(t.Context(), l.codec, fit.Scenario{Rules: []fit.Rule{
		{Point: "postgresql:ledger", Fault: fit.Fault{Abort: true}},
	}})
	require.NoError(t, err)

	_, err = l.store.Transfer(ctx, "t-1", "alice", "bob", 200)
	requireInjected(t, err)
	_, err = l.store.Deposit(ctx, "d-1", "bob", 200)
	requireInjected(t, err)
	_, err = l.store.OpenAccount(ctx, "carol")
	requireInjected(t, err)

	require.Zero(t, l.transfers(t, "t-1"))
	require.Zero(t, l.transfers(t, "d-1"))
	require.EqualValues(t, 500, l.balance(t, "alice"))
	require.Zero(t, l.balance(t, "bob"))
	_, err = l.store.Account(t.Context(), "carol")
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = l.store.Transfer(t.Context(), "t-1", "alice", "bob", 200)
	require.NoError(t, err)
}
