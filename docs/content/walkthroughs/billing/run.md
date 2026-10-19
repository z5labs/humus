---
title: The run
linkTitle: The run
description: >
  The billing job's run: it issues a month of invoices, charges every pending
  one with a bounded number in flight, settles each answer, stops early when
  every further charge would fail, and returns what the exit status reports.
type: docs
weight: 40
---

## The run's inputs

The `billing` package's `Run` takes the store as an interface, so its unit
tests can give it one in memory:

```go
type Invoices interface {
	Issue(ctx context.Context, period time.Time) (issued int64, err error)
	Pending(ctx context.Context) ([]postgres.Invoice, error)
	SettleCharged(ctx context.Context, id uuid.UUID, chargeID string) (settled bool, err error)
	SettleDeclined(ctx context.Context, id uuid.UUID, declineCode string) (settled bool, err error)
	SettleFailed(ctx context.Context, id uuid.UUID, answer string) (settled bool, err error)
}

type Run struct {
	Invoices    Invoices
	Payments    *payments.Client
	Period      time.Time
	Concurrency int
	Telemetry   telemetry.SDK
}
```

## Its telemetry

The run's tracer, logger and three instruments, in its own scope:

```go
const scope = "github.com/z5labs/humus/example/billing"
```

```go
type observer struct {
	tracer  trace.Tracer
	logger  log.Logger
	issued  metric.Int64Counter
	settled metric.Int64Counter
	pending metric.Int64Gauge
}

func newObserver(sdk telemetry.SDK) (*observer, error) {
	meter := sdk.MeterProvider.Meter(scope)
	issued, err := meter.Int64Counter("billing.invoices.issued",
		metric.WithDescription("Invoices issued."),
		metric.WithUnit("{invoice}"))
	if err != nil {
		return nil, err
	}
	settled, err := meter.Int64Counter("billing.invoices.settled",
		metric.WithDescription("Invoices settled, by the state they were settled in."),
		metric.WithUnit("{invoice}"))
	if err != nil {
		return nil, err
	}
	pending, err := meter.Int64Gauge("billing.invoices.pending",
		metric.WithDescription("Invoices left pending as the run ended."),
		metric.WithUnit("{invoice}"))
	if err != nil {
		return nil, err
	}
	return &observer{
		tracer:  sdk.TracerProvider.Tracer(scope),
		logger:  sdk.LoggerProvider.Logger(scope),
		issued:  issued,
		settled: settled,
		pending: pending,
	}, nil
}
```

## The period

The run's month, or last month in UTC when none is set:

```go
func (r Run) period() time.Time {
	if !r.Period.IsZero() {
		return time.Date(r.Period.Year(), r.Period.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
}
```

## Issue

The first step, in a span of its own:

```go
func (r Run) issue(ctx context.Context, o *observer) (int64, error) {
	ctx, span := o.tracer.Start(ctx, "issue")
	defer span.End()

	issued, err := r.Invoices.Issue(ctx, r.period())
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return 0, err
	}
	o.issued.Add(ctx, issued)
	return issued, nil
}
```

## Charge

Each invoice is charged in a `charge` span carrying its ID:

```go
func (r Run) charge(ctx context.Context, o *observer, inv postgres.Invoice) (state string, stop bool, err error) {
	ctx, span := o.tracer.Start(ctx, "charge", trace.WithAttributes(attribute.String("billing.invoice.id", inv.ID.String())))
	defer span.End()

	state, stop, err = r.settle(ctx, o, inv)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return state, stop, err
}
```

## Settle

The answer is recorded in the invoice's final state, with a WARN record for
an invoice that failed. An error says whether it stops the run: the payments
client's `ErrStop`, or a statement that failed, because the next charge's
statement would fail too:

```go
func (r Run) settle(ctx context.Context, o *observer, inv postgres.Invoice) (state string, stop bool, err error) {
	res, err := r.Payments.Charge(ctx, payments.Charge{
		ID:          inv.ID,
		AccountID:   inv.AccountID,
		AmountCents: inv.AmountCents,
		Currency:    inv.Currency,
	})
	if err != nil {
		return "", errors.Is(err, payments.ErrStop), err
	}
	var settled bool
	switch res.State {
	case payments.Charged:
		state = "charged"
		settled, err = r.Invoices.SettleCharged(ctx, inv.ID, res.ChargeID)
	case payments.Declined:
		state = "declined"
		settled, err = r.Invoices.SettleDeclined(ctx, inv.ID, res.DeclineCode)
	case payments.Failed:
		state = "failed"
		settled, err = r.Invoices.SettleFailed(ctx, inv.ID, res.Answer)
	default:
		return "", false, fmt.Errorf("charging invoice %s: the payments client answered an unknown state %d", inv.ID, res.State)
	}
	if err != nil {
		return "", true, err
	}
	if !settled {
		return "", false, nil
	}
	o.settled.Add(ctx, 1, metric.WithAttributes(attribute.String("billing.invoice.state", state)))
	if res.State == payments.Failed {
		o.emit(ctx, log.SeverityWarn, "WARN", "invoice failed",
			attribute.String("billing.invoice.id", inv.ID.String()),
			attribute.String("billing.invoice.answer", res.Answer))
	}
	return state, false, nil
}
```

## Counting what settled

A tally of the run's results, for the record it ends with:

```go
type tally struct {
	issued   int64
	charged  int64
	declined int64
	failed   int64
}

func (t *tally) add(state string) {
	switch state {
	case "charged":
		t.charged++
	case "declined":
		t.declined++
	case "failed":
		t.failed++
	}
}
```

## The run

`Run` is what `job.NewService` runs. It checks its concurrency, issues the
month and reads every pending invoice:

```go
func (r Run) Run(ctx context.Context) error {
	if r.Concurrency < 1 {
		return fmt.Errorf("billing with a concurrency of %d: it must be at least 1", r.Concurrency)
	}

	o, err := newObserver(r.Telemetry)
	if err != nil {
		return err
	}

	issued, err := r.issue(ctx, o)
	if err != nil {
		return err
	}

	invoices, err := r.Invoices.Pending(ctx)
	if err != nil {
		return err
	}
```

Up to `Concurrency` workers charge them. An error that stops the run halts
it, and once it is halted, or its context is done, no worker starts another
charge:

```go
var (
	mu      sync.Mutex
	started int
	why     []error
	halted  bool
	counts  = tally{issued: issued}
)
halt := make(chan struct{})
stopped := func() bool {
	select {
	case <-halt:
		return true
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

work := make(chan postgres.Invoice)
var wg sync.WaitGroup
for range min(r.Concurrency, len(invoices)) {
	wg.Go(func() {
		for inv := range work {
			mu.Lock()
			if stopped() {
				mu.Unlock()
				continue
			}
			started++
			mu.Unlock()

			state, stop, err := r.charge(ctx, o, inv)
			mu.Lock()
			counts.add(state)
			if err == nil {
				mu.Unlock()
				continue
			}
			why = append(why, err)
			if stop && !halted {
				halted = true
				close(halt)
			}
			mu.Unlock()
		}
	})
}
```

The invoices are handed out until none is left or the run stops, then the
run waits for the charges in flight:

```go
hand:
	for _, inv := range invoices {
		if stopped() {
			break
		}
		select {
		case work <- inv:
		case <-halt:
			break hand
		case <-ctx.Done():
			break hand
		}
	}
	close(work)
	wg.Wait()
```

## What it returns

An invoice that wasn't started, or whose charge failed, is left pending.
The run returns `nil` only when none is:

```go
left := len(invoices) - started + len(why)
o.ended(ctx, counts, int64(left))
if left == 0 {
	return nil
}
if ctx.Err() != nil {
	return context.Cause(ctx)
}
if halted {
	return fmt.Errorf("stopped early, leaving %d of %d invoices pending: %w", left, len(invoices), errors.Join(why...))
}
return fmt.Errorf("left %d of %d invoices pending: %w", left, len(invoices), errors.Join(why...))
```

| The run | Exit | Kubernetes |
|---|---|---|
| leaves nothing pending | 0 | completes the Job |
| leaves invoices pending | 1 | retries the pod, under `backoffLimit` |
| is stopped by SIGTERM, and returns `context.Cause(ctx)` | 1 | retries the pod, and the next run charges what it left |

## The record it ends with

The pending gauge and an INFO record with the counts. They use a context
that isn't canceled, so a run stopped by SIGTERM still records them:

```go
func (o *observer) ended(ctx context.Context, counts tally, pending int64) {
	ctx = context.WithoutCancel(ctx)
	o.pending.Record(ctx, pending)
	o.emit(ctx, log.SeverityInfo, "INFO", "billing run ended",
		attribute.Int64("billing.invoices.issued", counts.issued),
		attribute.Int64("billing.invoices.charged", counts.charged),
		attribute.Int64("billing.invoices.declined", counts.declined),
		attribute.Int64("billing.invoices.failed", counts.failed),
		attribute.Int64("billing.invoices.pending", pending))
}

func (o *observer) emit(ctx context.Context, sev log.Severity, text, body string, attrs ...attribute.KeyValue) {
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(text)
	r.SetBody(attribute.StringValue(body))
	r.AddAttributes(attrs...)
	o.logger.Emit(ctx, r)
}
```
