// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
)

func rehearsing(t *testing.T, ctx context.Context, f fit.Fault) (context.Context, fault.Injector) {
	t.Helper()
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	d := fault.DefaultSettings()
	d.Decoder = codec
	faults, err := fault.New(d)
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{{Point: store.Point, Fault: f}}})
	require.NoError(t, err)
	return baggage.ContextWithBaggage(ctx, b), faults
}

func unreachable(t *testing.T) *store.Store {
	t.Helper()
	return unreachableWith(t, fault.Injector{})
}

func unreachableWith(t *testing.T, faults fault.Injector) *store.Store {
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
	return store.New(pool, faults)
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

var calls = []struct {
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

func TestStore_on_a_host_where_nothing_listens_wraps_every_error_as_ErrUnavailable(t *testing.T) {
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			synctest.Test(t, func(t *testing.T) {
				err = tc.call(t.Context(), unreachable(t))
			})
			requireUnreachable(t, err)
		})
	}
}

func TestStore_fails_every_query_a_scenario_aborts_at_its_point_as_ErrUnavailable_before_the_database(t *testing.T) {
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			synctest.Test(t, func(t *testing.T) {
				ctx, faults := rehearsing(t, t.Context(), fit.Fault{Abort: true})
				err = tc.call(ctx, unreachableWith(t, faults))
			})
			require.ErrorIs(t, err, store.ErrUnavailable)
			require.ErrorIs(t, err, fit.ErrInjected)
			injected, ok := errors.AsType[*fault.InjectedError](err)
			require.True(t, ok, "%v", err)
			require.Equal(t, store.Point, injected.Point)
			var opErr *net.OpError
			require.NotErrorAs(t, err, &opErr, "the database isn't dialed")
		})
	}
}

func TestStore_waits_out_a_delay_at_its_point_before_every_query(t *testing.T) {
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			var (
				err     error
				elapsed time.Duration
			)
			synctest.Test(t, func(t *testing.T) {
				ctx, faults := rehearsing(t, t.Context(), fit.Fault{Delay: 3 * time.Second})
				s := unreachableWith(t, faults)
				start := time.Now()
				err = tc.call(ctx, s)
				elapsed = time.Since(start)
			})
			require.GreaterOrEqual(t, elapsed, 3*time.Second)
			requireUnreachable(t, err)
		})
	}
}

func TestStore_fails_a_query_whose_delay_its_context_cuts_short_as_ErrUnavailable(t *testing.T) {
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				ctx, faults := rehearsing(t, ctx, fit.Fault{Delay: time.Minute})
				err = tc.call(ctx, unreachableWith(t, faults))
			})
			require.ErrorIs(t, err, store.ErrUnavailable)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			injected, ok := errors.AsType[*fault.InjectedError](err)
			require.True(t, ok, "%v", err)
			require.Equal(t, store.Point, injected.Point)
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
