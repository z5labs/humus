---
title: Every replica
linkTitle: Every replica
description: >
  How a vote reaches the feeds on every replica: the notification a trigger
  sends in the vote's transaction, the Listener that LISTENs on a connection
  of its own and hands each feed the newest results, and what it does when
  that connection is lost.
type: docs
weight: 30
---

## The notification

A trigger on `votes` notifies the `votes` channel with the poll's id, in
the vote's own transaction. PostgreSQL delivers a notification only when its
transaction commits, so a vote that rolls back tells no one:

```sql
CREATE FUNCTION notify_vote() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('votes', NEW.poll_id::text);
    RETURN NULL;
END
$$;

CREATE TRIGGER votes_notify AFTER INSERT ON votes FOR EACH ROW EXECUTE FUNCTION notify_vote();
```

## The listener

Each replica has one `Listener`, which every feed on the replica subscribes
to:

```go
type Listener struct {
	connect func(context.Context) (listenConn, error)
	poll    func(context.Context, uuid.UUID) (Poll, error)
	log     log.Logger

	listening atomic.Bool

	mu   sync.Mutex
	subs map[uuid.UUID]map[chan Poll]struct{}
}
```

It connects on a connection of its own, outside the pool, since a `LISTEN`
belongs to the session that ran it:

```go
func NewListener(cfg *pgx.ConnConfig, s *Store, logger log.Logger) *Listener {
	return newListener(func(ctx context.Context) (listenConn, error) {
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}, s.Poll, logger)
}
```

```go
const (
	votesChannel   = "votes"
	reconnectDelay = time.Second
	closeTimeout   = 5 * time.Second
)
```

## Subscribing

A feed subscribes to one poll, and gets a channel that holds one result,
and a function that unsubscribes:

```go
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
```

## Listening

`listen` connects, runs `LISTEN votes`, refreshes every subscribed poll,
then refreshes each poll a notification names:

```go
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
```

```go
func (l *Listener) subscribed() []uuid.UUID {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(l.subs))
	for id := range l.subs {
		ids = append(ids, id)
	}
	return ids
}
```

## Refreshing a poll

A refresh reads the poll once, however many feeds watch it, and none when
none do. It replaces a result a feed hasn't taken yet, so a slow feed gets
the newest results and never blocks the others:

```go
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
```

## When its connection is lost

`Run` listens until its context ends. When the connection is lost, it logs
one error for the outage, waits a second, and listens again:

```go
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
```

PostgreSQL keeps no notification for a session that has gone, so the
refresh of every subscribed poll in `listen` is what catches the feeds up
on the votes they missed.

`Listening` says whether it is listening now, for the readiness probe on
[Init]({{< relref "/walkthroughs/http/polls/init#readiness" >}}):

```go
var errNotListening = errors.New("not listening for votes")
```

```go
func (l *Listener) Listening(context.Context) error {
	if !l.listening.Load() {
		return errNotListening
	}
	return nil
}
```
