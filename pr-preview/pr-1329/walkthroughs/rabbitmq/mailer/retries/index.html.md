# Retries and dead letters

## Parking a request

A request that failed for now is published to `mail.retry`, with its
attempt one higher. A returned error would stop the Service instead, and
every replica would crash-loop through a provider's outage:

```go
func (w *Worker) park(ctx context.Context, m rabbitmq.Message, attempt int) error {
	p := request(m, mail.RetryExchange, map[string]any{
		mail.AttemptHeader: int64(attempt),
	})
	p.UserID = w.user
	if err := w.retries.Publish(ctx, p); err != nil {
		return fmt.Errorf("worker: parking the request in %s: %w", mail.RetryExchange, err)
	}
	return nil
}
```

The parked request is the request as it was published, plus its headers:

```go
func request(m rabbitmq.Message, exchange string, headers map[string]any) rabbitmq.Publishing {
	return rabbitmq.Publishing{
		Exchange:      exchange,
		RoutingKey:    mail.SendKey,
		Headers:       headers,
		ContentType:   m.ContentType,
		MessageID:     m.MessageID,
		CorrelationID: m.CorrelationID,
		Body:          m.Body,
	}
}
```

## The delay queue

`mail.send.retry` holds it for the policy's `message-ttl`, then
dead-letters it back to `mail` by `mail.send`, and the worker tries it
again:

```json
"definition": {
  "message-ttl": 300000,
  "dead-letter-exchange": "mail",
  "dead-letter-routing-key": "mail.send",
  "dead-letter-strategy": "at-least-once",
  "overflow": "reject-publish"
}
```

Every attempt waits the same delay, since a quorum queue expires messages
only from its head.

## The attempt header

The header names:

```go
const (
	AttemptHeader      = "x-mail-attempt"
	DeadReasonHeader   = "x-mail-dead-reason"
	DeadAttemptsHeader = "x-mail-dead-attempts"
	DeadErrorHeader    = "x-mail-dead-error"
)
```

A request without `x-mail-attempt` is on its first attempt. One whose
header isn't a positive integer is malformed:

```go
func attemptOf(m rabbitmq.Message) (int, error) {
	v, ok := m.Headers[mail.AttemptHeader]
	if !ok {
		return 1, nil
	}
	var n int64
	switch v := v.(type) {
	case int8:
		n = int64(v)
	case int16:
		n = int64(v)
	case int32:
		n = int64(v)
	case int64:
		n = v
	case int:
		n = int64(v)
	case uint8:
		n = int64(v)
	case uint16:
		n = int64(v)
	case uint32:
		n = int64(v)
	default:
		return 0, fmt.Errorf("%w: its %s is a %T, not an integer", mail.ErrMalformed, mail.AttemptHeader, v)
	}
	if n < 1 {
		return 0, fmt.Errorf("%w: its %s is %d, not positive", mail.ErrMalformed, mail.AttemptHeader, n)
	}
	return int(n), nil
}
```

## Dead letters

A malformed request, and one that failed `MAIL_MAX_ATTEMPTS` times, is
published to `mail.dead`, with why:

```go
const (
	malformed = "malformed"
	gaveUp    = "gave-up"
)
```

```go
func (w *Worker) deadLetter(ctx context.Context, m rabbitmq.Message, reason string, attempts int, cause error) error {
	p := request(m, mail.DeadExchange, map[string]any{
		mail.DeadReasonHeader:   reason,
		mail.DeadAttemptsHeader: int64(attempts),
		mail.DeadErrorHeader:    cause.Error(),
	})
	if err := w.deadLetters.Publish(ctx, p); err != nil {
		return fmt.Errorf("worker: dead-lettering the request to %s: %w", mail.DeadExchange, err)
	}
	return nil
}
```

| Header | What it holds |
|---|---|
| `x-mail-dead-reason` | `malformed` or `gave-up` |
| `x-mail-dead-attempts` | the attempts it had |
| `x-mail-dead-error` | the error of its last attempt |

A dead letter has no `x-mail-attempt`. `mail.dead` is a fanout, so it
reaches `mail.send.dead` and any queue of yours bound to it.

## The delivery limit

A request that can go to neither `mail.retry` nor `mail.dead` makes
`Process` return the error. The Service stops, the pod restarts, and the
delivery is redelivered. `mail.send`'s policy dead-letters it to `mail.dead`
after 5 deliveries, with `x-first-death-reason` `delivery_limit`:

```json
"definition": {
  "delivery-limit": 5,
  "dead-letter-exchange": "mail.dead",
  "dead-letter-strategy": "at-least-once",
  "overflow": "reject-publish"
}
```

## What to do with `mail.send.dead`

The mailer only puts dead letters in `mail.send.dead`. Plan what to do with
them, whether to send them again, tell someone or drop them, from your own
business requirements.

Next: [Init](/humus/pr-preview/pr-1329/walkthroughs/rabbitmq/mailer/init/)
