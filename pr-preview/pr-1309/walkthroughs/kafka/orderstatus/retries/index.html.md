# Retries and dead letters

## Processing a record

One `Process` handles both `orders.events` and `orders.retry`, and keeps no
state of its own:

```go
func (p *Projector) Process(ctx context.Context, m kafka.Message) error {
	attempt, from, err := p.origin(m)
	if err != nil {
		return p.deadLetter(ctx, m, from, reasonMalformed, err, 0)
	}

	e, reason, err := decode(m, from)
	if err != nil {
		return p.deadLetter(ctx, m, from, reason, err, 0)
	}

	err = p.wait(ctx, m, attempt)
	if err != nil {
		return err
	}

	work, cancel := context.WithTimeout(ctx, workDeadline)
	defer cancel()
	reason, err = p.project(work, m, e)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("projecting %s, partition %d, offset %d: %w", from.topic, from.partition, from.offset, context.Cause(ctx))
	}
	return p.retry(ctx, m, from, attempt, reason, err)
}
```

| The record | Then |
|---|---|
| malformed, or of an unknown type | a dead letter, at once |
| projected, or applied already | committed |
| `unavailable` or `invalid-transition`, below `RETRY_ATTEMPTS` | produced to `orders.retry`, one attempt higher |
| the same, at `RETRY_ATTEMPTS` | a dead letter, with its last attempt's reason |
| its context ended, at shutdown or a revoke | `Process` returns the error, and the record is processed again |
| neither `orders.retry` nor `orders.dlq` stores it | `Process` returns the error, and the App exits 1 |

```go
const (
	reasonMalformed         = "malformed"
	reasonUnknownType       = "unknown-type"
	reasonInvalidTransition = "invalid-transition"
	reasonUnavailable       = "unavailable"
)
```

## The retry headers

A retried record carries its attempt, why it failed, and where it came from
on `orders.events`:

```go
const (
	headerAttempt   = "retry.attempt"
	headerReason    = "retry.reason"
	headerError     = "retry.error"
	headerPartition = "retry.partition"
	headerOffset    = "retry.offset"
)
```

## Retrying a record

Produce it to `orders.retry` with its key, value and headers, its old
`retry.` headers replaced. Once `RETRY_ATTEMPTS` have failed, it is a dead
letter instead:

```go
func (p *Projector) retry(ctx context.Context, m kafka.Message, from place, attempt int, reason string, cause error) error {
	failed := attempt + 1
	if failed >= p.settings.Attempts {
		return p.deadLetter(ctx, m, from, reason, cause, failed)
	}

	headers := make([]kafka.Header, 0, len(m.Headers)+5)
	for _, h := range m.Headers {
		if !strings.HasPrefix(h.Key, "retry.") {
			headers = append(headers, h)
		}
	}
	headers = append(headers,
		kafka.Header{Key: headerAttempt, Value: []byte(strconv.Itoa(failed))},
		kafka.Header{Key: headerReason, Value: []byte(reason)},
		kafka.Header{Key: headerError, Value: []byte(cause.Error())},
		kafka.Header{Key: headerPartition, Value: []byte(strconv.FormatInt(int64(from.partition), 10))},
		kafka.Header{Key: headerOffset, Value: []byte(strconv.FormatInt(from.offset, 10))},
	)
	err := p.retries.Produce(ctx, kafka.Record{
		Topic:   p.settings.RetryTopic,
		Key:     m.Key,
		Value:   m.Value,
		Headers: headers,
	})
	if err != nil {
		return fmt.Errorf("retrying %s, partition %d, offset %d as %s, attempt %d: %w", from.topic, from.partition, from.offset, reason, failed, err)
	}
	p.retried.Add(ctx, 1, metric.WithAttributes(reasonKey.String(reason)))
	return nil
}
```

The record keeps its key, so an order's later events follow it into
`orders.retry` when they fail too, and the partition moves on.

## Where a record came from

A record from `orders.events` is attempt 0, from its own place. One from
`orders.retry` reads its attempt and its place on `orders.events` from its
headers. A header that's missing or bad makes it malformed:

```go
func (p *Projector) origin(m kafka.Message) (int, place, error) {
	own := place{topic: m.Topic, partition: m.Partition, offset: m.Offset}
	if m.Topic != p.settings.RetryTopic {
		return 0, own, nil
	}

	var errs []error
	attempt, err := header(m, headerAttempt, func(s string) (int, error) {
		n, err := strconv.Atoi(s)
		if err == nil && n < 1 {
			err = errors.New("it is less than 1")
		}
		return n, err
	})
	errs = append(errs, err)
	partition, err := header(m, headerPartition, func(s string) (int32, error) {
		n, err := strconv.ParseInt(s, 10, 32)
		if err == nil && n < 0 {
			err = errors.New("it is negative")
		}
		return int32(n), err
	})
	errs = append(errs, err)
	offset, err := header(m, headerOffset, func(s string) (int64, error) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil && n < 0 {
			err = errors.New("it is negative")
		}
		return n, err
	})
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return 0, own, err
	}
	return attempt, place{topic: p.settings.EventsTopic, partition: partition, offset: offset}, nil
}
```

```go
func header[T any](m kafka.Message, key string, parse func(string) (T, error)) (T, error) {
	var zero T
	value, found := "", false
	for _, h := range m.Headers {
		if h.Key == key {
			value, found = string(h.Value), true
		}
	}
	if !found {
		return zero, fmt.Errorf("the record has no %s header", key)
	}
	v, err := parse(value)
	if err != nil {
		return zero, fmt.Errorf("the %s header %q is not valid: %w", key, value, err)
	}
	return v, nil
}
```

## The backoff

`RETRY_MIN_DELAY`, doubled each attempt, up to `RETRY_MAX_DELAY`:

```go
func (p *Projector) backoff(attempt int) time.Duration {
	d := p.settings.MinDelay
	for i := 1; i < attempt && d > 0 && d < p.settings.MaxDelay; i++ {
		d *= 2
	}
	return min(d, p.settings.MaxDelay)
}
```

A retried record waits until its timestamp plus its backoff. It also waits
for the records ahead of it on its partition, so it may come later than its
backoff, but never sooner:

```go
func (p *Projector) wait(ctx context.Context, m kafka.Message, attempt int) error {
	if attempt == 0 {
		return nil
	}
	d := time.Until(m.Timestamp.Add(p.backoff(attempt)))
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("waiting to retry %s, partition %d, offset %d: %w", m.Topic, m.Partition, m.Offset, context.Cause(ctx))
	case <-t.C:
		return nil
	}
}
```

A record still waiting at SIGTERM, or through a revoke, is processed again
by its partition's next owner.

## Dead letters

A dead letter keeps the record's key, value and headers, and adds why, and
where it came from on `orders.events`, even when it was consumed from
`orders.retry`:

```go
func (p *Projector) deadLetter(ctx context.Context, m kafka.Message, from place, reason string, cause error, attempts int) error {
	headers := make([]kafka.Header, 0, len(m.Headers)+6)
	headers = append(headers, m.Headers...)
	headers = append(headers,
		kafka.Header{Key: "dlq.reason", Value: []byte(reason)},
		kafka.Header{Key: "dlq.error", Value: []byte(cause.Error())},
		kafka.Header{Key: "dlq.topic", Value: []byte(from.topic)},
		kafka.Header{Key: "dlq.partition", Value: []byte(strconv.FormatInt(int64(from.partition), 10))},
		kafka.Header{Key: "dlq.offset", Value: []byte(strconv.FormatInt(from.offset, 10))},
	)
	if attempts > 0 {
		headers = append(headers, kafka.Header{Key: "dlq.attempts", Value: []byte(strconv.Itoa(attempts))})
	}
	err := p.deadLetters.Produce(ctx, kafka.Record{
		Topic:   p.settings.DeadLetterTopic,
		Key:     m.Key,
		Value:   m.Value,
		Headers: headers,
	})
	if err != nil {
		return fmt.Errorf("dead-lettering %s, partition %d, offset %d as %s: %w", from.topic, from.partition, from.offset, reason, err)
	}
	p.dropped.Add(ctx, 1, metric.WithAttributes(reasonKey.String(reason)))
	return nil
}
```

| Header | What it holds |
|---|---|
| `dlq.reason` | `malformed`, `unknown-type`, or the last attempt's `invalid-transition` or `unavailable` |
| `dlq.error` | the error |
| `dlq.topic`, `dlq.partition`, `dlq.offset` | where the record came from |
| `dlq.attempts` | the attempts it had, for a record that ran out of them |

`Process` returns once the dead letter is stored, so the record is committed
only after it.

## What it counts

Two counters, by `orderstatus.reason`:

```go
retried, err := meter.Int64Counter("orderstatus.retries",
	metric.WithDescription("Records produced to the retry topic, by the reason their attempt failed."),
	metric.WithUnit("{record}"))
if err != nil {
	return nil, err
}
dropped, err := meter.Int64Counter("orderstatus.dead_letters",
	metric.WithDescription("Records produced to the dead-letter topic, by the reason they were given up on."),
	metric.WithUnit("{record}"))
```

```go
const reasonKey = attribute.Key("orderstatus.reason")
```

## What to do with `orders.dlq`

The processor only puts dead letters in `orders.dlq`. Plan what to do with
them, whether to produce them again, tell someone or drop them, from your
own business requirements.

Next: [Init](/humus/pr-preview/pr-1309/walkthroughs/kafka/orderstatus/init/)
