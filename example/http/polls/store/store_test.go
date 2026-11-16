// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store_test

import (
	"context"
	"crypto/rand"
	"net"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/humustest"
)

func unreachable(t *testing.T) *store.Store {
	t.Helper()
	h := humustest.NewHost(t)
	cfg, err := pgxpool.ParseConfig("postgres://polls:" + rand.Text() + "@db.internal:5432/polls?sslmode=disable")
	require.NoError(t, err)
	cfg.ConnConfig.DialFunc = h.Dial
	cfg.ConnConfig.LookupFunc = func(_ context.Context, host string) ([]string, error) {
		return []string{host}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return store.New(pool)
}

func requireUnreachable(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, store.ErrUnavailable)
	var opErr *net.OpError
	require.ErrorAs(t, err, &opErr, "the dial's own error is kept")
	for _, other := range []error{store.ErrNoSuchPoll, store.ErrNotAnOption, store.ErrAlreadyVoted, store.ErrInvalidPoll} {
		require.NotErrorIs(t, err, other)
	}
}

func TestStore_on_a_host_where_nothing_listens_wraps_every_error_as_ErrUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		call func(context.Context, *store.Store) error
	}{
		{
			name: "CreatePoll",
			call: func(ctx context.Context, s *store.Store) error {
				_, err := s.CreatePoll(ctx, "Tabs or spaces?", []string{"Tabs", "Spaces"})
				return err
			},
		},
		{
			name: "Poll",
			call: func(ctx context.Context, s *store.Store) error {
				_, err := s.Poll(ctx, uuid.New())
				return err
			},
		},
		{
			name: "Vote",
			call: func(ctx context.Context, s *store.Store) error {
				return s.Vote(ctx, uuid.New(), 1, "voter")
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			synctest.Test(t, func(t *testing.T) {
				err = tc.call(t.Context(), unreachable(t))
			})
			requireUnreachable(t, err)
		})
	}
}

func TestStore_refuses_a_poll_that_breaks_its_rules_before_the_database(t *testing.T) {
	testCases := []struct {
		name     string
		question string
		options  []string
	}{
		{name: "no question", question: "", options: []string{"Tabs", "Spaces"}},
		{name: "no options", question: "Tabs or spaces?"},
		{name: "one option", question: "Tabs or spaces?", options: []string{"Tabs"}},
		{name: "an option given twice", question: "Tabs or spaces?", options: []string{"Tabs", "Spaces", "Tabs"}},
		{name: "an option with no text", question: "Tabs or spaces?", options: []string{"Tabs", ""}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				id  uuid.UUID
				err error
			)
			synctest.Test(t, func(t *testing.T) {
				id, err = unreachable(t).CreatePoll(t.Context(), tc.question, tc.options)
			})
			require.ErrorIs(t, err, store.ErrInvalidPoll)
			require.NotErrorIs(t, err, store.ErrUnavailable)
			require.Equal(t, uuid.Nil, id)
		})
	}
}
