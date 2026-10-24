# Processing a request

## The request

The `mail` package decodes a delivery into a request:

```go
const MaxBodySize = 256 << 10

var ErrMalformed = errors.New("malformed request")

type Request struct {
	MessageID     string
	CorrelationID string
	To            string
	Subject       string
	Text          string
}

type body struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}
```

## Decoding

A request with no `MessageId`, a body over 256 KiB or not JSON, or a field
missing is malformed:

```go
func Decode(m rabbitmq.Message) (Request, error) {
	if m.MessageID == "" {
		return Request{}, fmt.Errorf("%w: it has no MessageId", ErrMalformed)
	}
	if len(m.Body) > MaxBodySize {
		return Request{}, fmt.Errorf("%w: its body is %d bytes, over %d", ErrMalformed, len(m.Body), MaxBodySize)
	}
	var b body
	if err := json.Unmarshal(m.Body, &b); err != nil {
		return Request{}, fmt.Errorf("%w: its body is not a JSON object with to, subject and text as strings: %w", ErrMalformed, err)
	}
	for _, f := range []struct{ name, value string }{
		{"to", b.To},
		{"subject", b.Subject},
		{"text", b.Text},
	} {
		if f.value == "" {
			return Request{}, fmt.Errorf("%w: its body has no %s", ErrMalformed, f.name)
		}
	}
	return Request{
		MessageID:     m.MessageID,
		CorrelationID: m.CorrelationID,
		To:            b.To,
		Subject:       b.Subject,
		Text:          b.Text,
	}, nil
}
```

## The events

Each event carries the request's `MessageId` and `CorrelationId` to
`mail.events`:

```go
type sent struct {
	ProviderID string `json:"provider_id"`
}

type refused struct {
	Reason string `json:"reason"`
}

func (r Request) Sent(providerID string) rabbitmq.Publishing {
	return r.event(SentKey, sent{ProviderID: providerID})
}

func (r Request) Refused(reason string) rabbitmq.Publishing {
	return r.event(RefusedKey, refused{Reason: reason})
}

func (r Request) event(key string, v any) rabbitmq.Publishing {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return rabbitmq.Publishing{
		Exchange:      EventsExchange,
		RoutingKey:    key,
		ContentType:   "application/json",
		MessageID:     r.MessageID,
		CorrelationID: r.CorrelationID,
		Body:          b,
	}
}
```

## The worker

The `worker` package's `Worker` is a `queue.Processor` of
`rabbitmq.Message`s. It takes the provider and its Publishers as
interfaces, so its unit tests can give it their own:

```go
type Provider interface {
	Send(ctx context.Context, r mail.Request) (provider.Result, error)
}

type Publisher interface {
	Publish(ctx context.Context, m rabbitmq.Publishing) error
}

type Settings struct {
	Provider    Provider
	Retries     Publisher
	Results     Publisher
	DeadLetters Publisher
	MaxAttempts int
	User        string
	Telemetry   telemetry.SDK
}
```

```go
var _ queue.Processor[rabbitmq.Message] = (*Worker)(nil)
```

`New` checks every setting, and creates the `mail.requests` counter:

```go
requests, err := settings.Telemetry.MeterProvider.Meter(scope).Int64Counter("mail.requests",
	metric.WithDescription("Mail requests processed, by outcome."),
	metric.WithUnit("{request}"))
```

## Deciding

`process` decides what becomes of each delivery:

```go
func (w *Worker) process(ctx context.Context, m rabbitmq.Message) (outcome string, attempt int, err error) {
	span := trace.SpanFromContext(ctx)
	attempt, err = attemptOf(m)
	if err != nil {
		span.SetAttributes(attemptKey.Int(1))
		return outcomeMalformed, 1, w.deadLetter(ctx, m, malformed, 1, err)
	}
	span.SetAttributes(attemptKey.Int(attempt))
	r, err := mail.Decode(m)
	if err != nil {
		return outcomeMalformed, attempt, w.deadLetter(ctx, m, malformed, attempt, err)
	}
	refused, err := w.send(ctx, r)
	switch {
	case err == nil && refused:
		return outcomeRefused, attempt, nil
	case err == nil:
		return outcomeSent, attempt, nil
	case attempt >= w.maxAttempts:
		return outcomeGaveUp, attempt, w.deadLetter(ctx, m, gaveUp, attempt, err)
	default:
		return outcomeRetried, attempt, w.park(ctx, m, attempt+1)
	}
}
```

| The request | The provider | The worker publishes | Then |
|---|---|---|---|
| malformed | not called | a dead letter, reason `malformed` | acked once the broker confirms it |
| valid | sent it | `mail.sent` | acked once confirmed, or at once when no queue receives it |
| valid | refused it | `mail.refused` | the same |
| valid, below `MAIL_MAX_ATTEMPTS` | failed, or its event's publish failed | the request again, to `mail.retry`, one attempt higher | acked once confirmed, and back after the delay |
| valid, at `MAIL_MAX_ATTEMPTS` | the same | a dead letter, reason `gave-up` | acked once confirmed |
| any | | a publish to `mail.retry` or `mail.dead` that fails | `Process` returns the error |

## Sending and publishing the event

The event is published once the provider has answered. Each `Publish`
waits for the broker's confirm:

```go
func (w *Worker) send(ctx context.Context, r mail.Request) (refused bool, err error) {
	res, err := w.provider.Send(ctx, r)
	if err != nil {
		return false, err
	}
	event := r.Sent(res.ID)
	if res.Refused {
		event = r.Refused(res.Reason)
	}
	err = w.results.Publish(ctx, event)
	if err != nil && !errors.Is(err, rabbitmq.ErrUnroutable) {
		return false, fmt.Errorf("worker: publishing %s: %w", event.RoutingKey, err)
	}
	return res.Refused, nil
}
```

`rabbitmq.ErrUnroutable` means no queue is bound to receive the event, which
is no error, since no service may subscribe to it yet. An event that fails
otherwise parks the request, whose next attempt sends the same key, so the
provider sends no second email.

## Acking

The Service consumes under `queue.AtLeastOnce`:

```go
consumer.Delivery = queue.AtLeastOnce
```

A delivery is acked only once `Process` returns `nil`, which is once every
publish of its result is confirmed:

```go
func (w *Worker) Process(ctx context.Context, m rabbitmq.Message) error {
	outcome, attempt, err := w.process(ctx, m)
	if err != nil {
		return err
	}
	w.observe(ctx, m, outcome, attempt)
	return nil
}
```

## What it records

The outcome on the process span, the `mail.requests` counter and a log
record, with the `MessageId` and the attempt. No address or body reaches the
telemetry:

```go
func (w *Worker) observe(ctx context.Context, m rabbitmq.Message, outcome string, attempt int) {
	trace.SpanFromContext(ctx).SetAttributes(outcomeKey.String(outcome))
	w.requests.Add(ctx, 1, metric.WithAttributes(outcomeKey.String(outcome)))

	severity, text := log.SeverityInfo, "INFO"
	if outcome == outcomeGaveUp || outcome == outcomeMalformed {
		severity, text = log.SeverityWarn, "WARN"
	}
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(severity)
	r.SetSeverityText(text)
	r.SetBody(attribute.StringValue("mail request " + outcome))
	if m.MessageID != "" {
		r.AddAttributes(semconv.MessagingMessageID(m.MessageID))
	}
	r.AddAttributes(attemptKey.Int(attempt), outcomeKey.String(outcome))
	w.logger.Emit(ctx, r)
}
```

```go
const (
	attemptKey = attribute.Key("mail.attempt")
	outcomeKey = attribute.Key("mail.outcome")
)

const (
	outcomeSent      = "sent"
	outcomeRefused   = "refused"
	outcomeRetried   = "retried"
	outcomeGaveUp    = "gave_up"
	outcomeMalformed = "malformed"
)
```

Next: [Retries and dead letters](/humus/pr-preview/pr-1329/walkthroughs/rabbitmq/mailer/retries/)
