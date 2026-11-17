// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
)

const (
	votesChannel   = "votes"
	reconnectDelay = time.Second
	closeTimeout   = 5 * time.Second
)

var errNotListening = errors.New("not listening for votes")

type listenConn interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	WaitForNotification(ctx context.Context) (*pgconn.Notification, error)
	Close(ctx context.Context) error
}

type Listener struct {
	connect func(context.Context) (listenConn, error)
	poll    func(context.Context, uuid.UUID) (Poll, error)
	log     log.Logger

	listening atomic.Bool

	mu   sync.Mutex
	subs map[uuid.UUID]map[chan Poll]struct{}
}

func NewListener(cfg *pgx.ConnConfig, s *Store, logger log.Logger) *Listener {
	return newListener(func(ctx context.Context) (listenConn, error) {
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}, s.Poll, logger)
}

func newListener(connect func(context.Context) (listenConn, error), poll func(context.Context, uuid.UUID) (Poll, error), logger log.Logger) *Listener {
	return &Listener{
		connect: connect,
		poll:    poll,
		log:     logger,
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
		l.listening.Store(false)
		if ctx.Err() != nil {
			return
		}
		if !logged {
			l.emit(ctx, log.SeverityError, "ERROR", "listening for votes failed", err)
			logged = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func (l *Listener) Listening(context.Context) error {
	if !l.listening.Load() {
		return errNotListening
	}
	return nil
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
	l.listening.Store(true)
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
			l.emit(ctx, log.SeverityWarn, "WARN", "a vote's notification names no poll", nil, attribute.String("payload", n.Payload))
			continue
		}
		if err := l.refresh(ctx, id); err != nil {
			return err
		}
	}
}

func (l *Listener) emit(ctx context.Context, severity log.Severity, text, body string, err error, attrs ...attribute.KeyValue) {
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(severity)
	r.SetSeverityText(text)
	r.SetBody(attribute.StringValue(body))
	r.SetErr(err)
	r.AddAttributes(attrs...)
	l.log.Emit(context.WithoutCancel(ctx), r)
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
