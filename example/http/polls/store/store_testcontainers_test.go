//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/fault"
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

type polls struct {
	pool  *pgxpool.Pool
	store *store.Store
}

func newPolls(t *testing.T) polls {
	t.Helper()
	ctx := t.Context()
	c := sharedPostgres(t)

	adminURL, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	admin, err := pgx.Connect(ctx, adminURL)
	require.NoError(t, err)
	defer func() { _ = admin.Close(context.Background()) }()

	name := "polls_" + strings.ToLower(rand.Text())
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
	cfg.ConnConfig.DialFunc = humus.OSHost().DialTCP
	cfg.MaxConns = 32

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(schema))
	require.NoError(t, err)

	return polls{pool: pool, store: store.New(pool, fault.Injector{})}
}

func (p polls) create(t *testing.T, question string, options ...string) store.Poll {
	t.Helper()
	id, err := p.store.CreatePoll(t.Context(), question, options)
	require.NoError(t, err)
	poll, err := p.store.Poll(t.Context(), id)
	require.NoError(t, err)
	return poll
}

func (p polls) counts(t *testing.T, id uuid.UUID) []int64 {
	t.Helper()
	poll, err := p.store.Poll(t.Context(), id)
	require.NoError(t, err)
	counts := make([]int64, len(poll.Options))
	for i, o := range poll.Options {
		counts[i] = o.Votes
	}
	return counts
}

func (p polls) votes(t *testing.T) int {
	t.Helper()
	var n int
	err := p.pool.QueryRow(t.Context(), `SELECT count(*) FROM votes`).Scan(&n)
	require.NoError(t, err)
	return n
}

func requirePgError(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, code, pgErr.Code)
	require.Equal(t, constraint, pgErr.ConstraintName)
}

func TestStore_reads_back_a_poll_it_creates_with_no_votes(t *testing.T) {
	p := newPolls(t)
	ctx := t.Context()

	id, err := p.store.CreatePoll(ctx, "Tabs or spaces?", []string{"Tabs", "Spaces", "Both"})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, id)

	poll, err := p.store.Poll(ctx, id)
	require.NoError(t, err)
	require.Equal(t, id, poll.ID)
	require.Equal(t, "Tabs or spaces?", poll.Question)
	require.Len(t, poll.Options, 3)
	for i, text := range []string{"Tabs", "Spaces", "Both"} {
		require.Equal(t, text, poll.Options[i].Text, "the options keep their order")
		require.NotZero(t, poll.Options[i].ID)
		require.Zero(t, poll.Options[i].Votes)
	}

	_, err = p.store.Poll(ctx, uuid.New())
	require.ErrorIs(t, err, store.ErrNoSuchPoll)
	require.NotErrorIs(t, err, store.ErrUnavailable)
}

func TestStore_refuses_a_poll_that_breaks_its_rules_and_creates_nothing(t *testing.T) {
	p := newPolls(t)

	for _, options := range [][]string{nil, {"Tabs"}, {"Tabs", "Tabs"}, {"Tabs", ""}} {
		_, err := p.store.CreatePoll(t.Context(), "Tabs or spaces?", options)
		require.ErrorIs(t, err, store.ErrInvalidPoll)
	}
	_, err := p.store.CreatePoll(t.Context(), "", []string{"Tabs", "Spaces"})
	require.ErrorIs(t, err, store.ErrInvalidPoll)

	var n int
	err = p.pool.QueryRow(t.Context(), `SELECT count(*) FROM polls`).Scan(&n)
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestStore_counts_a_vote_once_and_refuses_a_second_from_the_same_voter(t *testing.T) {
	p := newPolls(t)
	ctx := t.Context()
	poll := p.create(t, "Tabs or spaces?", "Tabs", "Spaces")
	tabs, spaces := poll.Options[0].ID, poll.Options[1].ID

	require.NoError(t, p.store.Vote(ctx, poll.ID, tabs, "alice"))
	require.Equal(t, []int64{1, 0}, p.counts(t, poll.ID))

	for _, option := range []int64{tabs, spaces} {
		err := p.store.Vote(ctx, poll.ID, option, "alice")
		require.ErrorIs(t, err, store.ErrAlreadyVoted)
		require.NotErrorIs(t, err, store.ErrUnavailable)
	}
	require.Equal(t, []int64{1, 0}, p.counts(t, poll.ID))

	require.NoError(t, p.store.Vote(ctx, poll.ID, spaces, "bob"))
	require.Equal(t, []int64{1, 1}, p.counts(t, poll.ID))

	other := p.create(t, "Vim or Emacs?", "Vim", "Emacs")
	require.NoError(t, p.store.Vote(ctx, other.ID, other.Options[1].ID, "alice"), "a voter votes once on each poll")
	require.Equal(t, []int64{0, 1}, p.counts(t, other.ID))
}

func TestStore_counts_concurrent_votes_from_one_voter_once(t *testing.T) {
	p := newPolls(t)
	poll := p.create(t, "Tabs or spaces?", "Tabs", "Spaces")

	const sends = 20
	errs := make(chan error, sends)
	for i := range sends {
		option := poll.Options[i%2].ID
		go func() {
			errs <- p.store.Vote(context.Background(), poll.ID, option, "alice")
		}()
	}

	counted := 0
	for range sends {
		err := <-errs
		if err == nil {
			counted++
			continue
		}
		require.ErrorIs(t, err, store.ErrAlreadyVoted)
	}
	require.Equal(t, 1, counted)
	counts := p.counts(t, poll.ID)
	require.EqualValues(t, 1, counts[0]+counts[1])
	require.Equal(t, 1, p.votes(t))
}

func TestStore_counts_every_one_of_concurrent_votes_from_many_voters(t *testing.T) {
	p := newPolls(t)
	poll := p.create(t, "Tabs or spaces?", "Tabs", "Spaces", "Both")

	const voters = 30
	errs := make(chan error, voters)
	for i := range voters {
		option := poll.Options[i%3].ID
		voter := fmt.Sprintf("voter-%d", i)
		go func() {
			errs <- p.store.Vote(context.Background(), poll.ID, option, voter)
		}()
	}
	for range voters {
		require.NoError(t, <-errs)
	}
	require.Equal(t, []int64{10, 10, 10}, p.counts(t, poll.ID))
}

func TestStore_refuses_a_vote_for_another_polls_option_and_counts_nothing(t *testing.T) {
	p := newPolls(t)
	ctx := t.Context()
	poll := p.create(t, "Tabs or spaces?", "Tabs", "Spaces")
	other := p.create(t, "Vim or Emacs?", "Vim", "Emacs")

	err := p.store.Vote(ctx, poll.ID, other.Options[0].ID, "alice")
	require.ErrorIs(t, err, store.ErrNotAnOption)
	require.NotErrorIs(t, err, store.ErrNoSuchPoll)
	require.NotErrorIs(t, err, store.ErrUnavailable)

	err = p.store.Vote(ctx, poll.ID, 1<<40, "alice")
	require.ErrorIs(t, err, store.ErrNotAnOption)

	err = p.store.Vote(ctx, uuid.New(), poll.Options[0].ID, "alice")
	require.ErrorIs(t, err, store.ErrNoSuchPoll)
	require.NotErrorIs(t, err, store.ErrNotAnOption)

	require.Equal(t, []int64{0, 0}, p.counts(t, poll.ID))
	require.Equal(t, []int64{0, 0}, p.counts(t, other.ID))
	require.Zero(t, p.votes(t))

	require.NoError(t, p.store.Vote(ctx, poll.ID, poll.Options[0].ID, "alice"), "a refused vote isn't a vote")
}

func TestStore_the_database_refuses_another_polls_option_and_a_second_vote(t *testing.T) {
	p := newPolls(t)
	ctx := t.Context()
	poll := p.create(t, "Tabs or spaces?", "Tabs", "Spaces")
	other := p.create(t, "Vim or Emacs?", "Vim", "Emacs")

	_, err := p.pool.Exec(ctx, `INSERT INTO votes (poll_id, option_id, voter) VALUES ($1, $2, 'alice')`, poll.ID, other.Options[0].ID)
	requirePgError(t, err, "23503", "votes_option_of_poll")

	_, err = p.pool.Exec(ctx, `INSERT INTO votes (poll_id, option_id, voter) VALUES ($1, $2, 'alice')`, poll.ID, poll.Options[0].ID)
	require.NoError(t, err)
	_, err = p.pool.Exec(ctx, `INSERT INTO votes (poll_id, option_id, voter) VALUES ($1, $2, 'alice')`, poll.ID, poll.Options[1].ID)
	requirePgError(t, err, "23505", "votes_one_per_voter")

	require.Equal(t, []int64{1, 0}, p.counts(t, poll.ID))
	require.Equal(t, []int64{0, 0}, p.counts(t, other.ID))
}

func TestStore_a_second_vote_is_refused_before_its_option_is_checked(t *testing.T) {
	p := newPolls(t)
	ctx := t.Context()
	poll := p.create(t, "Tabs or spaces?", "Tabs", "Spaces")
	other := p.create(t, "Vim or Emacs?", "Vim", "Emacs")

	require.NoError(t, p.store.Vote(ctx, poll.ID, poll.Options[0].ID, "alice"))
	err := p.store.Vote(ctx, poll.ID, other.Options[0].ID, "alice")
	require.ErrorIs(t, err, store.ErrAlreadyVoted)
	require.NotErrorIs(t, err, store.ErrNotAnOption)
}
