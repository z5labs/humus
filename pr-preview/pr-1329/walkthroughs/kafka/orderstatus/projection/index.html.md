# The projection

The projection keeps every event it applied, and each order's status, folded
from its events. The `store` package reads and writes it, and the
`projector` package folds it.

## The schema

`store/schema.sql`:

```sql
CREATE TABLE order_events (
    source_partition integer     NOT NULL,
    source_offset    bigint      NOT NULL,
    order_id         text        NOT NULL,
    type             text        NOT NULL,
    at               timestamptz NOT NULL,
    PRIMARY KEY (source_partition, source_offset)
);

CREATE INDEX order_events_by_order ON order_events (order_id, at);

CREATE TABLE order_status (
    order_id text        PRIMARY KEY,
    status   text        NOT NULL,
    at       timestamptz NOT NULL
);
```

An event is keyed by the partition and offset it was consumed from on
`orders.events`, so a redelivery is found by them.

The processor applies no schema. The kind overlay mounts it into
PostgreSQL's `docker-entrypoint-initdb.d`. In production, apply it with your
own migration tool.

The processor's role needs three privileges on the two tables:

```sh
CREATE ROLE orderstatus LOGIN PASSWORD '$orderstatus_password';
GRANT SELECT, INSERT, UPDATE ON order_events, order_status TO orderstatus;
```

## The events

An event, and a status, in Go:

```go
type Event struct {
	Partition int32
	Offset    int64
	OrderID   string
	Type      string
	At        time.Time
}

type Status struct {
	Status string
	At     time.Time
}
```

The `Store` is a pool:

```go
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}
```

`Applied` finds an event by its partition and offset:

```go
func (s *Store) Applied(ctx context.Context, partition int32, offset int64) (bool, error) {
	var applied bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM order_events WHERE source_partition = $1 AND source_offset = $2)`,
		partition, offset,
	).Scan(&applied)
	if err != nil {
		return false, fmt.Errorf("finding partition %d, offset %d: %w", partition, offset, err)
	}
	return applied, nil
}
```

`Events` reads an order's history, by `at`, then by offset:

```go
func (s *Store) Events(ctx context.Context, orderID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source_partition, source_offset, order_id, type, at
		FROM order_events
		WHERE order_id = $1
		ORDER BY at, source_offset`,
		orderID,
	)
	if err != nil {
		return nil, fmt.Errorf("reading order %s: %w", orderID, err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.Partition, &e.Offset, &e.OrderID, &e.Type, &e.At)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading order %s: %w", orderID, err)
	}
	return events, nil
}
```

## The folded status

The `projector` package's state machine. Each status names the ones it can
follow, and `placed` follows none:

```go
var transitions = map[string][]string{
	Placed:    {""},
	Paid:      {Placed},
	Shipped:   {Paid},
	Delivered: {Shipped},
	Cancelled: {Placed, Paid},
}
```

`Fold` puts the new event among the order's history, by `at`, then by
offset, and runs them all through the state machine. An event that repeats
the status before it changes nothing. It returns the status, and the changes
from the new event on:

```go
func Fold(history []store.Event, e store.Event) (store.Status, []Change, error) {
	type entry struct {
		store.Event
		isNew bool
	}
	events := make([]entry, 0, len(history)+1)
	for _, ev := range history {
		events = append(events, entry{Event: ev})
	}
	events = append(events, entry{Event: e, isNew: true})
	slices.SortStableFunc(events, func(a, b entry) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Offset, b.Offset))
	})

	var status store.Status
	var changes []Change
	reached := false
	for _, ev := range events {
		reached = reached || ev.isNew
		if status.Status != "" && ev.Type == status.Status {
			continue
		}
		if !slices.Contains(transitions[ev.Type], status.Status) {
			return store.Status{}, nil, &TransitionError{Event: ev.Event, Previous: status.Status}
		}
		if reached {
			changes = append(changes, Change{Status: ev.Type, Previous: status.Status, At: ev.At})
		}
		status = store.Status{Status: ev.Type, At: ev.At}
	}
	return status, changes, nil
}
```

```go
type Change struct {
	Status   string
	Previous string
	At       time.Time
}
```

An event at the end of the history makes one change. A late one makes its
own, then each later change again: `paid`, arriving after `cancelled` with
an `at` between `placed` and `cancelled`, makes `paid` from `placed`, then
`cancelled` from `paid`.

A history the state machine refuses is a `TransitionError`, naming the event
it refused:

```go
type TransitionError struct {
	Event    store.Event
	Previous string
}

func (e *TransitionError) Error() string {
	ev := e.Event
	if e.Previous == "" {
		return fmt.Sprintf("order %s: %s at partition %d, offset %d (%s) can't come first",
			ev.OrderID, ev.Type, ev.Partition, ev.Offset, ev.At.Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("order %s: %s at partition %d, offset %d (%s) can't follow %s",
		ev.OrderID, ev.Type, ev.Partition, ev.Offset, ev.At.Format(time.RFC3339Nano), e.Previous)
}
```

## Storing an event and its status

`Apply` stores the event and the folded status in one transaction. It
inserts the order's status row if it has none, then locks it, so two
replicas storing one order take turns. An event already in `order_events`
conflicts on its partition and offset, inserts nothing, and is `ErrApplied`,
so two replicas racing for a partition during a rebalance can't both apply
it:

```go
var ErrApplied = errors.New("event applied already")
```

```go
func (s *Store) Apply(ctx context.Context, e Event, status Status) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO order_status (order_id, status, at) VALUES ($1, $2, $3)
			ON CONFLICT (order_id) DO NOTHING`,
			e.OrderID, status.Status, status.At,
		)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT 1 FROM order_status WHERE order_id = $1 FOR UPDATE`, e.OrderID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO order_events (source_partition, source_offset, order_id, type, at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (source_partition, source_offset) DO NOTHING`,
			e.Partition, e.Offset, e.OrderID, e.Type, e.At,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrApplied
		}
		_, err = tx.Exec(ctx,
			`UPDATE order_status SET status = $2, at = $3 WHERE order_id = $1`,
			e.OrderID, status.Status, status.At,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("storing partition %d, offset %d of order %s: %w", e.Partition, e.Offset, e.OrderID, err)
	}
	return nil
}
```

`order_events` grows by a row per event, and none is deleted, since an
order's status is folded from all of them.

Next: [Projecting an event](/humus/pr-preview/pr-1329/walkthroughs/kafka/orderstatus/projecting/)
