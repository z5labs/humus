// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

type records struct {
	mu sync.Mutex
	rs []slog.Record
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }

func (r *records) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rs = append(r.rs, rec.Clone())
	return nil
}

func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *records) WithGroup(string) slog.Handler { return r }

func (r *records) all() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]slog.Record(nil), r.rs...)
}

type fakeConn struct {
	notes chan *pgconn.Notification
	lost  chan error

	mu     sync.Mutex
	execs  []string
	closed bool
}

func (c *fakeConn) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execs = append(c.execs, sql)
	return pgconn.CommandTag{}, nil
}

func (c *fakeConn) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	select {
	case n := <-c.notes:
		return n, nil
	case err := <-c.lost:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *fakeConn) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *fakeConn) executed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.execs...)
}

type database struct {
	mu       sync.Mutex
	down     error
	dials    int
	conns    []*fakeConn
	live     *fakeConn
	votes    map[uuid.UUID]int64
	reads    map[uuid.UUID]int
	failRead error
}

func newDatabase() *database {
	return &database{votes: make(map[uuid.UUID]int64), reads: make(map[uuid.UUID]int)}
}

func (d *database) connect(context.Context) (listenConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	if d.down != nil {
		return nil, d.down
	}
	c := &fakeConn{notes: make(chan *pgconn.Notification, 64), lost: make(chan error, 1)}
	d.conns = append(d.conns, c)
	d.live = c
	return c, nil
}

func (d *database) poll(_ context.Context, id uuid.UUID) (Poll, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failRead != nil {
		return Poll{}, d.failRead
	}
	votes, ok := d.votes[id]
	if !ok {
		return Poll{}, ErrNoSuchPoll
	}
	d.reads[id]++
	return Poll{ID: id, Question: "Tabs or spaces?", Options: []Option{{ID: 1, Text: "Tabs", Votes: votes}}}, nil
}

func (d *database) create() uuid.UUID {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := uuid.New()
	d.votes[id] = 0
	return id
}

func (d *database) vote(id uuid.UUID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.votes[id]++
	if d.live != nil {
		d.live.notes <- &pgconn.Notification{Channel: votesChannel, Payload: id.String()}
	}
}

func (d *database) lose(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = err
	d.live.lost <- err
	d.live = nil
}

func (d *database) recover() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = nil
}

func (d *database) readsOf(id uuid.UUID) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads[id]
}

func (d *database) dialed() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

func (d *database) connections() []*fakeConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*fakeConn(nil), d.conns...)
}

func run(t *testing.T, l *Listener) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
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

func votes(t *testing.T, ch <-chan Poll) int64 {
	t.Helper()
	select {
	case p, ok := <-ch:
		require.True(t, ok, "the subscription is open")
		require.Len(t, p.Options, 1)
		return p.Options[0].Votes
	default:
		t.Fatal("no results were sent")
		return 0
	}
}

func requireNothing(t *testing.T, ch <-chan Poll) {
	t.Helper()
	select {
	case p, ok := <-ch:
		if ok {
			t.Fatalf("results were sent: %+v", p)
		}
		t.Fatal("the subscription was closed")
	default:
	}
}

func TestListener_sends_a_polls_results_to_its_subscribers_only(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newDatabase()
		tabs, vim, unwatched := db.create(), db.create(), db.create()
		l := newListener(db.connect, db.poll, slog.New(&records{}))

		first, _ := l.Subscribe(tabs)
		second, _ := l.Subscribe(tabs)
		other, _ := l.Subscribe(vim)
		run(t, l)
		synctest.Wait()

		require.Equal(t, []string{"LISTEN votes"}, db.connections()[0].executed())
		require.Zero(t, votes(t, first), "a subscriber gets the current results once its listener listens")
		require.Zero(t, votes(t, second))
		require.Zero(t, votes(t, other))

		db.vote(tabs)
		synctest.Wait()
		require.EqualValues(t, 1, votes(t, first))
		require.EqualValues(t, 1, votes(t, second))
		requireNothing(t, other)

		db.vote(unwatched)
		synctest.Wait()
		require.Zero(t, db.readsOf(unwatched), "a poll nobody here subscribes to isn't read")
		requireNothing(t, first)
		requireNothing(t, other)
	})
}

func TestListener_sends_nothing_more_once_a_subscriber_unsubscribes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newDatabase()
		tabs := db.create()
		l := newListener(db.connect, db.poll, slog.New(&records{}))
		run(t, l)

		leaving, unsubscribe := l.Subscribe(tabs)
		staying, unsubscribeStaying := l.Subscribe(tabs)
		unsubscribe()
		unsubscribe()
		_, ok := <-leaving
		require.False(t, ok, "unsubscribing closes the subscription")

		db.vote(tabs)
		synctest.Wait()
		require.EqualValues(t, 1, votes(t, staying))

		unsubscribeStaying()
		reads := db.readsOf(tabs)
		db.vote(tabs)
		synctest.Wait()
		require.Equal(t, reads, db.readsOf(tabs), "a poll with no subscribers left isn't read")
		_, ok = <-staying
		require.False(t, ok)
	})
}

func TestListener_never_waits_on_a_subscriber_that_doesnt_keep_up(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newDatabase()
		tabs := db.create()
		l := newListener(db.connect, db.poll, slog.New(&records{}))

		slow, _ := l.Subscribe(tabs)
		fast, _ := l.Subscribe(tabs)
		run(t, l)
		synctest.Wait()
		require.Zero(t, votes(t, fast))

		const sent = 10
		for i := range sent {
			db.vote(tabs)
			synctest.Wait()
			require.EqualValues(t, i+1, votes(t, fast), "another subscriber gets every result")
		}

		require.EqualValues(t, sent, votes(t, slow), "a subscriber that doesn't keep up gets the latest results")
		requireNothing(t, slow)
	})
}

func TestListener_after_a_lost_connection_connects_again_and_sends_every_subscriber_its_current_results(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newDatabase()
		tabs, vim := db.create(), db.create()
		logs := &records{}
		l := newListener(db.connect, db.poll, slog.New(logs))

		tabsResults, _ := l.Subscribe(tabs)
		vimResults, _ := l.Subscribe(vim)
		run(t, l)
		synctest.Wait()
		require.Zero(t, votes(t, tabsResults))
		require.Zero(t, votes(t, vimResults))

		reset := errors.New("connection reset by peer")
		db.lose(reset)
		db.vote(tabs)
		time.Sleep(10 * reconnectDelay)
		synctest.Wait()
		require.Greater(t, db.dialed(), 2, "it keeps trying to connect")
		requireNothing(t, tabsResults)

		db.recover()
		time.Sleep(reconnectDelay)
		synctest.Wait()

		require.EqualValues(t, 1, votes(t, tabsResults), "a vote made while it was down isn't missed")
		require.Zero(t, votes(t, vimResults))
		conns := db.connections()
		require.Len(t, conns, 2)
		require.True(t, conns[0].isClosed(), "the lost connection is closed")
		require.Equal(t, []string{"LISTEN votes"}, conns[1].executed())

		rs := logs.all()
		require.Len(t, rs, 1, "the loss is one log record, however many tries it takes to connect again")
		require.Equal(t, slog.LevelError, rs[0].Level)
		var logged error
		rs[0].Attrs(func(a slog.Attr) bool {
			if a.Key == "error" {
				logged, _ = a.Value.Any().(error)
			}
			return true
		})
		require.ErrorIs(t, logged, reset)

		db.vote(vim)
		synctest.Wait()
		require.EqualValues(t, 1, votes(t, vimResults))

		db.lose(reset)
		db.recover()
		time.Sleep(reconnectDelay)
		synctest.Wait()
		require.Len(t, logs.all(), 2, "a second loss is a record of its own")
	})
}

func TestListener_whose_results_cant_be_read_connects_again(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newDatabase()
		tabs := db.create()
		logs := &records{}
		l := newListener(db.connect, db.poll, slog.New(logs))

		results, _ := l.Subscribe(tabs)
		run(t, l)
		synctest.Wait()
		require.Zero(t, votes(t, results))

		unavailable := fmt.Errorf("reading poll %s: %w", tabs, ErrUnavailable)
		db.mu.Lock()
		db.failRead = unavailable
		db.mu.Unlock()
		db.vote(tabs)
		synctest.Wait()
		requireNothing(t, results)

		db.mu.Lock()
		db.failRead = nil
		db.mu.Unlock()
		time.Sleep(reconnectDelay)
		synctest.Wait()
		require.EqualValues(t, 1, votes(t, results))
		require.Len(t, db.connections(), 2)
		require.True(t, db.connections()[0].isClosed())
		require.Len(t, logs.all(), 1)
	})
}

func TestListener_runs_until_its_context_is_done_then_closes_its_connection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newDatabase()
		l := newListener(db.connect, db.poll, slog.New(&records{}))
		stop := run(t, l)
		synctest.Wait()
		require.False(t, db.connections()[0].isClosed())

		stop()
		require.True(t, db.connections()[0].isClosed())
	})
}

func TestListener_that_cant_connect_at_all_logs_once_and_stops_with_its_context(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newDatabase()
		db.down = errors.New("connection refused")
		logs := &records{}
		l := newListener(db.connect, db.poll, slog.New(logs))
		stop := run(t, l)

		time.Sleep(5 * reconnectDelay)
		synctest.Wait()
		require.Len(t, logs.all(), 1)
		stop()
		require.Empty(t, db.connections())
	})
}
