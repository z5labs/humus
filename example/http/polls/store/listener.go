// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	votesChannel   = "votes"
	reconnectDelay = time.Second
	closeTimeout   = 5 * time.Second
)

type listenConn interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	WaitForNotification(ctx context.Context) (*pgconn.Notification, error)
	Close(ctx context.Context) error
}

type Listener struct {
	connect func(context.Context) (listenConn, error)
	poll    func(context.Context, uuid.UUID) (Poll, error)
	log     *slog.Logger

	mu   sync.Mutex
	subs map[uuid.UUID]map[chan Poll]struct{}
}

func NewListener(cfg *pgx.ConnConfig, s *Store, log *slog.Logger) *Listener {
	return newListener(func(ctx context.Context) (listenConn, error) {
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}, s.Poll, log)
}

func newListener(connect func(context.Context) (listenConn, error), poll func(context.Context, uuid.UUID) (Poll, error), log *slog.Logger) *Listener {
	return &Listener{
		connect: connect,
		poll:    poll,
		log:     log,
		subs:    make(map[uuid.UUID]map[chan Poll]struct{}),
	}
}

func (l *Listener) Subscribe(pollID uuid.UUID) (<-chan Poll, func()) {
	ch := make(chan Poll, 1)
	l.mu.Lock()
	if l.subs[pollID] == nil {
		l.subs[pollID] = make(map[chan Poll]struct{})
	}
	l.subs[pollID][ch] = struct{}{}
	l.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			delete(l.subs[pollID], ch)
			if len(l.subs[pollID]) == 0 {
				delete(l.subs, pollID)
			}
			close(ch)
		})
	}
}

func (l *Listener) Run(ctx context.Context) {
	logged := false
	for {
		err := l.listen(ctx, func() { logged = false })
		if ctx.Err() != nil {
			return
		}
		if !logged {
			l.log.ErrorContext(ctx, "listening for votes failed", slog.Any("error", err))
			logged = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func (l *Listener) listen(ctx context.Context, listening func()) error {
	conn, err := l.connect(ctx)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+votesChannel); err != nil {
		return err
	}
	for _, id := range l.subscribed() {
		if err := l.refresh(ctx, id); err != nil {
			return err
		}
	}
	listening()

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		id, err := uuid.Parse(n.Payload)
		if err != nil {
			l.log.WarnContext(ctx, "a vote's notification names no poll", slog.String("payload", n.Payload))
			continue
		}
		if err := l.refresh(ctx, id); err != nil {
			return err
		}
	}
}

func (l *Listener) subscribed() []uuid.UUID {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(l.subs))
	for id := range l.subs {
		ids = append(ids, id)
	}
	return ids
}

func (l *Listener) refresh(ctx context.Context, id uuid.UUID) error {
	l.mu.Lock()
	n := len(l.subs[id])
	l.mu.Unlock()
	if n == 0 {
		return nil
	}

	p, err := l.poll(ctx, id)
	if errors.Is(err, ErrNoSuchPoll) {
		return nil
	}
	if err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for ch := range l.subs[id] {
		select {
		case <-ch:
		default:
		}
		q := p
		q.Options = slices.Clone(p.Options)
		ch <- q
	}
	return nil
}
