# Projecting an event

## The projector

The `projector` package's `Projector` is the `queue.Processor` of
`kafka.Message`s that the Service runs. It takes the store and its
`Producer`s as interfaces, so its unit tests can give it their own:

```go
type Store interface {
	Applied(ctx context.Context, partition int32, offset int64) (bool, error)
	Events(ctx context.Context, orderID string) ([]store.Event, error)
	Apply(ctx context.Context, e store.Event, status store.Status) error
}

type Producer interface {
	Produce(ctx context.Context, records ...kafka.Record) error
}

type Settings struct {
	EventsTopic     string
	StatusTopic     string
	RetryTopic      string
	DeadLetterTopic string
	Attempts        int
	MinDelay        time.Duration
	MaxDelay        time.Duration
}
```

The Service consumes under `queue.AtLeastOnce`, so a record is committed
only once `Process` returns `nil`:

```go
consumer.Delivery = queue.AtLeastOnce
```

## Decoding

A record with no key, a value that isn't an event, or an event with no
`type` or `at` is malformed. An event of a type the state machine doesn't
know is `unknown-type`:

```go
func decode(m kafka.Message, from place) (store.Event, string, error) {
	if len(m.Key) == 0 {
		return store.Event{}, reasonMalformed, errors.New("the record has no key")
	}
	var v event
	if err := json.Unmarshal(m.Value, &v); err != nil {
		return store.Event{}, reasonMalformed, fmt.Errorf("the value is not an event: %w", err)
	}
	if v.Type == "" {
		return store.Event{}, reasonMalformed, errors.New("the event has no type")
	}
	if v.At.IsZero() {
		return store.Event{}, reasonMalformed, errors.New("the event has no at")
	}
	if _, ok := transitions[v.Type]; !ok {
		return store.Event{}, reasonUnknownType, fmt.Errorf("the event's type %q is not one this processor knows", v.Type)
	}
	return store.Event{
		Partition: from.partition,
		Offset:    from.offset,
		OrderID:   string(m.Key),
		Type:      v.Type,
		At:        v.At,
	}, "", nil
}
```

The event is keyed by `from`, the place it was consumed from on
`orders.events`. A record from `orders.retry` carries that place in its
headers, as [Retries and dead letters](/humus/pr-preview/pr-1210/walkthroughs/orderstatus/retries/#where-a-record-came-from)
shows.

```go
type place struct {
	topic     string
	partition int32
	offset    int64
}
```

## Retrying the store's calls

Each call to the store is retried with bedrock's `retry`: 5 attempts,
backing off from 100ms to 2s, with 2s for each attempt:

```go
const (
	workDeadline        = 15 * time.Second
	storeAttempts       = 5
	storeMinDelay       = 100 * time.Millisecond
	storeMaxDelay       = 2 * time.Second
	storeAttemptTimeout = 2 * time.Second
)
```

```go
schedule, err := retry.Exponential(storeMinDelay, storeMaxDelay)
if err != nil {
	return nil, err
}
r, err := retry.New(storeAttempts, schedule, classifyStore, nil)
if err != nil {
	return nil, err
}
```

`ErrApplied` is not retried, since another replica stored the event:

```go
func classifyStore(ctx context.Context, err error) (retry.Outcome, time.Duration) {
	switch {
	case err == nil:
		return retry.Success, 0
	case errors.Is(err, store.ErrApplied):
		return retry.Stop, 0
	default:
		return retry.Retry, 0
	}
}
```

```go
func (p *Projector) call(ctx context.Context, f func(context.Context) error) error {
	var err error
	for a := range p.retrier.Attempts(ctx) {
		err = attemptStore(ctx, f)
		a.Done(err)
	}
	return err
}

func attemptStore(ctx context.Context, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, storeAttemptTimeout)
	defer cancel()
	return f(ctx)
}
```

Producing is not retried: the `Producer` already retries within its
`DeliveryTimeout`.

## The deadline

A record's work has 15s, under `humus.Main`'s grace period of 20s, so a
rollout's SIGTERM finds no record it can't finish:

```go
work, cancel := context.WithTimeout(ctx, workDeadline)
defer cancel()
reason, err = p.project(work, m, e)
```

## Skipping what was applied

An event already in `order_events` was applied, by this replica or another,
and changes nothing:

```go
func (p *Projector) project(ctx context.Context, m kafka.Message, e store.Event) (string, error) {
	var applied bool
	err := p.call(ctx, func(ctx context.Context) error {
		var err error
		applied, err = p.store.Applied(ctx, e.Partition, e.Offset)
		return err
	})
	if err != nil {
		return reasonUnavailable, err
	}
	if applied {
		return "", nil
	}
```

A store that stays down is `unavailable`, and the record is retried through
`orders.retry`.

## Folding the history

Read the order's events, and fold the new one in. A history the state
machine refuses is `invalid-transition`, and is retried too, since an event
before it may still be coming:

```go
var history []store.Event
err = p.call(ctx, func(ctx context.Context) error {
	var err error
	history, err = p.store.Events(ctx, e.OrderID)
	return err
})
if err != nil {
	return reasonUnavailable, err
}
status, changes, err := Fold(history, e)
if err != nil {
	return reasonInvalidTransition, err
}
```

## Producing before storing

Produce the changes to `orders.status`, then store the event and its
status. A crash between the two produces the changes again on redelivery,
rather than losing them:

```go
	if len(changes) > 0 {
		records := make([]kafka.Record, 0, len(changes))
		for _, c := range changes {
			records = append(records, kafka.Record{
				Topic: p.settings.StatusTopic,
				Key:   m.Key,
				Value: encodeChange(e.OrderID, c),
			})
		}
		err := p.status.Produce(ctx, records...)
		if err != nil {
			return reasonUnavailable, fmt.Errorf("producing order %s's changes from partition %d, offset %d: %w", e.OrderID, e.Partition, e.Offset, err)
		}
	}

	err = p.call(ctx, func(ctx context.Context) error {
		return p.store.Apply(ctx, e, status)
	})
	if err != nil && !errors.Is(err, store.ErrApplied) {
		return reasonUnavailable, err
	}
	return "", nil
}
```

A consumer of `orders.status` tells a duplicate apart by the order and the
status, as any consumer under at-least-once must.

Each change is a JSON value:

```go
func encodeChange(orderID string, c Change) []byte {
	b, err := json.Marshal(change{OrderID: orderID, Status: c.Status, Previous: c.Previous, At: c.At})
	if err != nil {
		panic(err)
	}
	return b
}
```
