---
title: Production-ready
linkTitle: Production-ready
description: >
  The billing job's production-ready checklist: each item, and how the job
  meets it, to check a job of your own against.
type: docs
weight: 90
---

## Every setting is sourced and checked

`config.Struct` sources every setting, and each one's check names its
variable. A bad one exits 78:

```go
Period:      r.Get(env("BILLING_PERIOD").Parse(parseMonth).Default(lastMonth(time.Now())).Validate(checkEnded)),
Concurrency: r.Get(env("BILLING_CONCURRENCY").Parse(strconv.Atoi).Default(4).Validate(checkConcurrency)),
```

The pod failure policy fails the Job on 78, with no retry:

```yaml
- action: FailJob
  onExitCodes:
    containerName: billing
    operator: In
    values: [78]
```

## No secret is in the environment, the logs or the spans

The database URL and the API key are files from Secrets, read through the
Host:

```go
file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }
```

```go
Database:    r.Get(file("DATABASE_URL_FILE").Parse(databaseURL)),
Payments:    client,
APIKey:      r.Get(file("PAYMENTS_API_KEY_FILE").Parse(apiKey)),
```

The key is a header, which `otelhttp` doesn't record:

```go
req.Header.Set("Authorization", "Bearer "+c.key)
```

## It never charges twice

The invoice's ID is the charge's idempotency key:

```go
req.Header.Set("Idempotency-Key", ch.ID.String())
```

A final state is never overwritten:

```go
`UPDATE invoices SET state = $2, `+column+` = $3, settled_at = now() WHERE id = $1 AND state = 'pending'`,
```

## It is safe to run again, or twice at once

Issuing a month issues each invoice once:

```go
INSERT INTO invoices (account_id, period, amount_cents, currency)
SELECT account_id, $1::date, amount_cents, currency FROM subscriptions WHERE active
ON CONFLICT (account_id, period) DO NOTHING`,
```

Scheduled runs don't overlap, and a retry waits for its failed pod to stop:

```yaml
concurrencyPolicy: Forbid
```

```yaml
podReplacementPolicy: Failed
```

Any other overlap sends the same keys, so it charges nothing twice.

## It resumes after any interruption

Every run charges whatever is pending, from any month:

```go
SELECT id, account_id, period, amount_cents, currency FROM invoices
WHERE state = 'pending'
ORDER BY period, issued_at, account_id`,
```

## An interruption isn't a failure

SIGTERM ends the run with exit 1 and the signal as its cause, which the Job
retries:

```go
if ctx.Err() != nil {
	return context.Cause(ctx)
}
```

An eviction isn't counted against `backoffLimit`:

```yaml
- action: Ignore
  onPodConditions:
    - type: DisruptionTarget
      status: "True"
```

## It is bounded

Each schedule has an hour to start, and each Job an hour to finish:

```yaml
startingDeadlineSeconds: 3600
```

```yaml
backoffLimit: 6
activeDeadlineSeconds: 3600
```

Each call has `httpclient`'s `Timeout`:

```go
client.Timeout = r.Get(env("PAYMENTS_TIMEOUT").Parse(time.ParseDuration).Default(client.Timeout).Validate(httpclient.CheckTimeout))
```

An open breaker stops the run, rather than failing every invoice left:

```go
if errors.Is(err, circuit.ErrRejected) {
	return Result{}, fmt.Errorf("charging invoice %s: %w: %w", ch.ID, ErrStop, err)
}
```

## It has least privilege

It has no Kubernetes API:

```yaml
automountServiceAccountToken: false
```

It runs as a user that isn't root, on a read-only root filesystem, with no
capabilities:

```yaml
runAsNonRoot: true
runAsUser: 65532
```

```yaml
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: [ALL]
```

## It is observable

It sends traces, logs and metrics over OTLP:

```yaml
- name: OTEL_TRACES_EXPORTER
  value: otlp
- name: OTEL_METRICS_EXPORTER
  value: otlp
- name: OTEL_LOGS_EXPORTER
  value: otlp
```

Grafana alerts on its instruments, humus's own included, with the rules in
`deploy/alerts.yaml`: `BillingNotSucceeded`, `BillingInvoicesPending`,
`BillingInvoicesFailed` and `BillingPaymentsBreakerOpened`. The **Billing**
dashboard is `deploy/dashboard.json`. Grafana provisions both:

```yaml
- name: grafana-alerting
  files:
    - ../alerts.yaml
- name: grafana-dashboard-providers
  files:
    - grafana/dashboards.yaml
- name: grafana-dashboards
  files:
    - ../dashboard.json
```

[Observing]({{< relref "walkthroughs/job/billing/observing" >}}) shows each.

## Its failures say why

The exit cause names what is left pending, and why:

```go
if halted {
	return fmt.Errorf("stopped early, leaving %d of %d invoices pending: %w", left, len(invoices), errors.Join(why...))
}
return fmt.Errorf("left %d of %d invoices pending: %w", left, len(invoices), errors.Join(why...))
```

A failed invoice keeps the answer that failed it:

```go
func (s *Store) SettleFailed(ctx context.Context, id uuid.UUID, answer string) (settled bool, err error) {
	return s.settle(ctx, id, "failed", `answer`, answer)
}
```

## Its failures are rehearsed

The [failure injection cases]({{< relref "walkthroughs/job/billing/testing#the-failure-injection-cases" >}})
run in containers and on kind, and a
[rehearsal]({{< relref "walkthroughs/job/billing/deploying#a-rehearsal" >}})
runs a scenario wherever the job is deployed:

```go
{Name: "ThePaymentsAPIIsDown", Run: ThePaymentsAPIIsDown},
{Name: "ThePaymentsAPIIsSlow", Run: ThePaymentsAPIIsSlow},
{Name: "ChargedButNotRecorded", Run: ChargedButNotRecorded},
{Name: "TheMonthCantBeIssued", Run: TheMonthCantBeIssued},
```

## It is safe by default

The CronJob's runs set no `BAGGAGE`, so they carry no scenario, and the
payments client never forwards one:

```go
settings.ForwardScenario = false
```

A declined card is final, because a final state is never overwritten. A
month that hasn't ended can't be issued:

```go
func checkEnded(month time.Time) error {
	if month.AddDate(0, 1, 0).After(time.Now()) {
		return fmt.Errorf("%s hasn't ended", month.Format("2006-01"))
	}
	return nil
}
```

## It has no probes and no PodDisruptionBudget

Nothing routes to a Job's pod, and the pod failure policy already absorbs a
node's drain, so `Init` returns the run alone:

```go
return []humus.Service{job.NewService(s, run.Run)}, nil
```
