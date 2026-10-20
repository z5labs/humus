// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store_test

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"

	"github.com/z5labs/humus/example/wallet/ledger/store"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
)

var errUnreachable = errors.New("unreachable")

type unreachable struct {
	store *store.Store
	codec jsonscenario.Codec
	dials *atomic.Int64
}

func newUnreachable(t *testing.T) unreachable {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://ledger@127.0.0.1:5432/ledger?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	dials := new(atomic.Int64)
	cfg.ConnConfig.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errUnreachable
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	fs := fault.DefaultSettings()
	fs.Decoder = codec
	faults, err := fault.New(fs)
	require.NoError(t, err)
	return unreachable{store: store.New(pool, faults), codec: codec, dials: dials}
}

func (u unreachable) under(t *testing.T, ctx context.Context, rules ...fit.Rule) context.Context {
	t.Helper()
	ctx, err := fit.ContextWithScenario(ctx, u.codec, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	return ctx
}

var methods = []struct {
	name string
	call func(context.Context, *store.Store) error
}{
	{
		name: "OpenAccount",
		call: func(ctx context.Context, s *store.Store) error {
			_, err := s.OpenAccount(ctx, "alice")
			return err
		},
	},
	{
		name: "Account",
		call: func(ctx context.Context, s *store.Store) error {
			_, err := s.Account(ctx, "alice")
			return err
		},
	},
	{
		name: "Deposit",
		call: func(ctx context.Context, s *store.Store) error {
			_, err := s.Deposit(ctx, "d-1", "alice", 500)
			return err
		},
	},
	{
		name: "Transfer",
		call: func(ctx context.Context, s *store.Store) error {
			_, err := s.Transfer(ctx, "t-1", "alice", "bob", 200)
			return err
		},
	},
}

func requireInjected(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, store.ErrUnavailable)
	var injected *fault.InjectedError
	require.ErrorAs(t, err, &injected)
	require.Equal(t, "postgresql:ledger", injected.Point)
}

func TestStore_an_abort_at_postgresql_ledger_fails_each_method_as_an_unreachable_database_does(t *testing.T) {
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			u := newUnreachable(t)

			err := m.call(u.under(t, t.Context(), fit.Rule{Point: "postgresql:ledger", Fault: fit.Fault{Abort: true}}), u.store)

			requireInjected(t, err)
			require.ErrorIs(t, err, fit.ErrInjected)
			require.Zero(t, u.dials.Load(), "an abort is evaluated before the transaction")

			err = m.call(t.Context(), u.store)
			require.ErrorIs(t, err, store.ErrUnavailable)
			require.ErrorIs(t, err, errUnreachable)
			var injected *fault.InjectedError
			require.False(t, errors.As(err, &injected))
		})
	}
}

func TestStore_a_scenario_for_another_point_reaches_the_database(t *testing.T) {
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			u := newUnreachable(t)

			err := m.call(u.under(t, t.Context(), fit.Rule{Point: "postgresql:other", Fault: fit.Fault{Abort: true}}), u.store)

			require.ErrorIs(t, err, store.ErrUnavailable)
			require.ErrorIs(t, err, errUnreachable)
			require.NotErrorIs(t, err, fit.ErrInjected)
			require.NotZero(t, u.dials.Load())
		})
	}
}

func TestStore_a_delay_at_postgresql_ledger_comes_before_the_transaction(t *testing.T) {
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			var (
				err       error
				waited    time.Duration
				dialsLeft int64
			)
			synctest.Test(t, func(t *testing.T) {
				u := newUnreachable(t)
				ctx := u.under(t, t.Context(), fit.Rule{Point: "postgresql:ledger", Fault: fit.Fault{Delay: 2 * time.Second}})
				start := time.Now()
				during := make(chan int64, 1)
				go func() {
					time.Sleep(2*time.Second - time.Millisecond)
					during <- u.dials.Load()
				}()

				err = m.call(ctx, u.store)

				waited = time.Since(start)
				dialsLeft = <-during
			})
			require.Zero(t, dialsLeft, "nothing is dialed during the delay")
			require.Equal(t, 2*time.Second, waited)
			require.ErrorIs(t, err, store.ErrUnavailable)
			require.ErrorIs(t, err, errUnreachable)
		})
	}
}

func TestStore_a_delay_at_postgresql_ledger_cut_short_fails_as_an_unreachable_database_does(t *testing.T) {
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			var (
				err    error
				waited time.Duration
				dials  int64
			)
			synctest.Test(t, func(t *testing.T) {
				u := newUnreachable(t)
				ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				defer cancel()
				ctx = u.under(t, ctx, fit.Rule{Point: "postgresql:ledger", Fault: fit.Fault{Delay: 5 * time.Second}})
				start := time.Now()

				err = m.call(ctx, u.store)

				waited = time.Since(start)
				dials = u.dials.Load()
			})
			requireInjected(t, err)
			var injected *fault.InjectedError
			require.ErrorAs(t, err, &injected)
			require.ErrorIs(t, injected.Err, context.DeadlineExceeded)
			require.NotErrorIs(t, err, fit.ErrInjected)
			require.Equal(t, 500*time.Millisecond, waited)
			require.Zero(t, dials)
		})
	}
}

func TestStore_refuses_an_amount_that_isnt_positive_or_an_account_sent_to_itself_before_the_database(t *testing.T) {
	testCases := []struct {
		name string
		call func(context.Context, *store.Store) error
		want error
	}{
		{
			name: "a transfer of nothing",
			call: func(ctx context.Context, s *store.Store) error {
				_, err := s.Transfer(ctx, "t-1", "alice", "bob", 0)
				return err
			},
			want: store.ErrNotPositive,
		},
		{
			name: "a negative transfer",
			call: func(ctx context.Context, s *store.Store) error {
				_, err := s.Transfer(ctx, "t-1", "alice", "bob", -100)
				return err
			},
			want: store.ErrNotPositive,
		},
		{
			name: "a negative deposit",
			call: func(ctx context.Context, s *store.Store) error {
				_, err := s.Deposit(ctx, "d-1", "alice", -100)
				return err
			},
			want: store.ErrNotPositive,
		},
		{
			name: "a transfer to itself",
			call: func(ctx context.Context, s *store.Store) error {
				_, err := s.Transfer(ctx, "t-1", "alice", "alice", 100)
				return err
			},
			want: store.ErrSameAccount,
		},
		{
			name: "a deposit into external",
			call: func(ctx context.Context, s *store.Store) error {
				_, err := s.Deposit(ctx, "d-1", store.External, 100)
				return err
			},
			want: store.ErrSameAccount,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUnreachable(t)

			err := tc.call(u.under(t, t.Context(), fit.Rule{Point: "postgresql:ledger", Fault: fit.Fault{Abort: true}}), u.store)

			require.ErrorIs(t, err, tc.want)
			require.NotErrorIs(t, err, store.ErrUnavailable)
			require.Zero(t, u.dials.Load())
		})
	}
}
