//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store_test

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/polls/store"
)

const wait = 30 * time.Second

type logRecords struct {
	mu sync.Mutex
	rs []slog.Record
}

func (r *logRecords) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecords) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rs = append(r.rs, rec.Clone())
	return nil
}

func (r *logRecords) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *logRecords) WithGroup(string) slog.Handler { return r }

func (r *logRecords) all() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]slog.Record(nil), r.rs...)
}

type listener struct {
	*store.Listener
	name string
	logs *logRecords
	hold atomic.Pointer[chan struct{}]
}

func (p polls) listener(t *testing.T) *listener {
	t.Helper()
	l := &listener{name: "listener_" + strings.ToLower(rand.Text()), logs: &logRecords{}}
	cfg := p.pool.Config().ConnConfig
	cfg.RuntimeParams["application_name"] = l.name
	dial := cfg.DialFunc
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if hold := l.hold.Load(); hold != nil {
			select {
			case <-*hold:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return dial(ctx, network, addr)
	}
	l.Listener = store.NewListener(cfg, p.store, slog.New(l.logs))
	return l
}

func (l *listener) run(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()
	stop = func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return stop
}

func (p polls) backends(t *testing.T, name string) int {
	t.Helper()
	n, err := p.countBackends(t.Context(), name)
	require.NoError(t, err)
	return n
}

func (p polls) countBackends(ctx context.Context, name string) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, name).Scan(&n)
	return n, err
}

func (p polls) requireNoBackend(t *testing.T, name, msg string) {
	t.Helper()
	require.Eventually(t, func() bool {
		n, err := p.countBackends(t.Context(), name)
		return err == nil && n == 0
	}, wait, 10*time.Millisecond, msg)
}

func receive(t *testing.T, ch <-chan store.Poll) []int64 {
	t.Helper()
	select {
	case poll, ok := <-ch:
		require.True(t, ok, "the subscription is open")
		counts := make([]int64, len(poll.Options))
		for i, o := range poll.Options {
			counts[i] = o.Votes
		}
		return counts
	case <-time.After(wait):
		t.Fatal("no results arrived")
		return nil
	}
}

func TestListener_two_listeners_on_one_database_each_get_a_votes_results(t *testing.T) {
	p := newPolls(t)
	poll := p.create(t, "Tabs or spaces?", "Tabs", "Spaces")

	first, second := p.listener(t), p.listener(t)
	firstResults, _ := first.Subscribe(poll.ID)
	secondResults, _ := second.Subscribe(poll.ID)
	stopFirst := first.run(t)
	second.run(t)
	require.Equal(t, []int64{0, 0}, receive(t, firstResults))
	require.Equal(t, []int64{0, 0}, receive(t, secondResults))

	require.NoError(t, p.store.Vote(t.Context(), poll.ID, poll.Options[1].ID, "alice"))
	require.Equal(t, []int64{0, 1}, receive(t, firstResults))
	require.Equal(t, []int64{0, 1}, receive(t, secondResults))

	require.Equal(t, 1, p.backends(t, first.name), "a listener is one connection")
	stopFirst()
	p.requireNoBackend(t, first.name, "a listener closes its connection when its context is done")
	require.Empty(t, first.logs.all())
	require.Empty(t, second.logs.all())
}

func TestListener_a_rolled_back_or_refused_vote_reaches_no_listener(t *testing.T) {
	p := newPolls(t)
	ctx := t.Context()
	poll := p.create(t, "Tabs or spaces?", "Tabs", "Spaces")
	other := p.create(t, "Vim or Emacs?", "Vim", "Emacs")

	l := p.listener(t)
	results, _ := l.Subscribe(poll.ID)
	otherResults, _ := l.Subscribe(other.ID)
	l.run(t)
	require.Equal(t, []int64{0, 0}, receive(t, results))
	require.Equal(t, []int64{0, 0}, receive(t, otherResults))

	require.NoError(t, p.store.Vote(ctx, poll.ID, poll.Options[0].ID, "alice"))
	require.Equal(t, []int64{1, 0}, receive(t, results))

	tx, err := p.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO votes (poll_id, option_id, voter) VALUES ($1, $2, 'bob')`, poll.ID, poll.Options[1].ID)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))

	err = p.store.Vote(ctx, poll.ID, poll.Options[1].ID, "alice")
	require.ErrorIs(t, err, store.ErrAlreadyVoted)

	require.NoError(t, p.store.Vote(ctx, other.ID, other.Options[0].ID, "alice"))
	require.Equal(t, []int64{1, 0}, receive(t, otherResults))

	select {
	case got := <-results:
		t.Fatalf("a vote that didn't commit sent results: %+v", got)
	default:
	}
	require.Equal(t, []int64{1, 0}, p.counts(t, poll.ID))
}

func TestListener_whose_backend_is_terminated_connects_again_and_misses_no_vote(t *testing.T) {
	p := newPolls(t)
	ctx := t.Context()
	poll := p.create(t, "Tabs or spaces?", "Tabs", "Spaces")

	l := p.listener(t)
	results, _ := l.Subscribe(poll.ID)
	l.run(t)
	require.Equal(t, []int64{0, 0}, receive(t, results))

	hold := make(chan struct{})
	l.hold.Store(&hold)
	var terminated bool
	err := p.pool.QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1`, l.name).Scan(&terminated)
	require.NoError(t, err)
	require.True(t, terminated)

	require.Eventually(t, func() bool { return len(l.logs.all()) > 0 }, wait, 10*time.Millisecond, "the loss is logged")
	p.requireNoBackend(t, l.name, "the listener is down")
	require.NoError(t, p.store.Vote(ctx, poll.ID, poll.Options[0].ID, "alice"))

	l.hold.Store(nil)
	close(hold)
	require.Equal(t, []int64{1, 0}, receive(t, results), "a vote made while it was down isn't missed")
	require.Equal(t, 1, p.backends(t, l.name))

	rs := l.logs.all()
	require.Len(t, rs, 1, "the loss is one log record")
	require.Equal(t, slog.LevelError, rs[0].Level)
	var logged error
	rs[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			logged, _ = a.Value.Any().(error)
		}
		return true
	})
	require.Error(t, logged)

	require.NoError(t, p.store.Vote(ctx, poll.ID, poll.Options[1].ID, "bob"))
	require.Equal(t, []int64{1, 1}, receive(t, results))
}

