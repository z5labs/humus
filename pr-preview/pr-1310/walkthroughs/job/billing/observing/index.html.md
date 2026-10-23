# Observing

## Grafana

On kind, Grafana's admin password is in the Secret `up.sh` made. Forward its
port, and sign in as `admin` at <http://localhost:3000>:

```sh
kubectl --namespace billing get secret grafana --output jsonpath='{.data.password}' | base64 --decode
kubectl --namespace billing port-forward service/grafana 3000
```

## A run's trace

Each run is a trace. Its root is the `job` span, or a child of the trace in
`TRACEPARENT` when whatever started the run set one. Under it are an `issue`
span, a `charge` span for each invoice, and a `POST` span for each attempt
at the payments API. In Explore, Tempo finds the runs with:

```text
{resource.service.name="billing" && name="job"}
```

A `charge` span carries its invoice's ID:

```go
ctx, span := o.tracer.Start(ctx, "charge", trace.WithAttributes(attribute.String("billing.invoice.id", inv.ID.String())))
```

## A run's records

Each record carries its trace's ID, and Grafana links the two: a span's
**Logs for this span** opens its records in Loki, and a record's `TraceID`
opens its trace in Tempo. Loki finds the job's records with:

```text
{service_name="billing"}
```

A run ends with an INFO record of its counts:

```go
o.emit(ctx, log.SeverityInfo, "INFO", "billing run ended",
	attribute.Int64("billing.invoices.issued", counts.issued),
	attribute.Int64("billing.invoices.charged", counts.charged),
	attribute.Int64("billing.invoices.declined", counts.declined),
	attribute.Int64("billing.invoices.failed", counts.failed),
	attribute.Int64("billing.invoices.pending", pending))
```

Each invoice that fails gets a WARN record, with its ID and the payments
API's answer:

```go
o.emit(ctx, log.SeverityWarn, "WARN", "invoice failed",
	attribute.String("billing.invoice.id", inv.ID.String()),
	attribute.String("billing.invoice.answer", res.Answer))
```

```text
{service_name="billing"} |= "invoice failed"
```

## A run's metrics

Mimir names each instrument with underscores for its dots, and labels each
series with `job="billing"`:

```text
billing_invoices_issued
billing_invoices_settled{billing_invoice_state=~"charged|declined|failed"}
billing_invoices_pending
humus_circuit_state
humus_circuit_transitions
humus_circuit_calls
humus_retry_decisions
```

A job exports its metrics as it exits, and each run is a new process whose
counters start from zero. So a series holds one run's totals, often in a
single sample, and every query reads it with `max_over_time` or
`last_over_time`, never `increase`, which needs two samples of one process:

```json
"expr": "max(max_over_time(billing_invoices_pending{job=\"billing\"}[$__interval]))",
```

## The dashboard

`deploy/dashboard.json` is the **Billing** dashboard. On kind, Grafana
provisions it into the **Billing** folder:

```yaml
apiVersion: 1
providers:
  - name: billing
    orgId: 1
    folder: Billing
    type: file
    disableDeletion: true
    allowUiUpdates: false
    options:
      path: /etc/grafana/dashboards
```

Its first row is **Runs**, each run's `billing run ended` record, newest
first:

```json
"expr": "{service_name=\"billing\"} |= \"billing run ended\""
```

The second is **Invoices by state**, issued and settled per run; **Pending**,
what each run left pending; and **Left pending by the last run**, orange when
it isn't 0:

```json
"expr": "sum by (billing_invoice_state) (max_over_time(billing_invoices_settled{job=\"billing\"}[$__interval]))",
```

```json
"expr": "max(last_over_time(billing_invoices_pending{job=\"billing\"}[26h]))",
```

The third is the payments client's: **Payments breaker**, its transitions and
its calls by outcome, and **Payments retries**, its retry decisions:

```json
"expr": "sum by (humus_circuit_state) (max_over_time(humus_circuit_transitions{job=\"billing\"}[$__interval]))",
```

```json
"expr": "sum by (humus_retry_decision) (max_over_time(humus_retry_decisions{job=\"billing\"}[$__interval]))",
```

## The alerts

`deploy/alerts.yaml` is a Grafana alerting provisioning file. Its four rules
are evaluated every minute, in the **Billing** folder:

```yaml
apiVersion: 1
groups:
  - orgId: 1
    name: billing
    folder: Billing
    interval: 1m
```

Each rule queries Mimir, then fires when the answer is above 0. Route them by
their `severity` label in your Grafana's notification policies:

```yaml
- refId: B
  datasourceUid: __expr__
  model:
    refId: B
    type: threshold
    expression: A
    conditions:
      - evaluator:
          type: gt
          params: [0]
```

### BillingNotSucceeded

Critical. No run has left nothing pending in 26h. A CronJob that never
starts records nothing, which the rule reads as 1:

```yaml
expr: min(min_over_time(billing_invoices_pending{job="billing"}[26h])) or vector(1)
```

26h is a day's schedule, plus the hour a run may wait to start and the hour
it may run. Look at the CronJob's Jobs, and at the last run's records:

```yaml
labels:
  severity: critical
annotations:
  summary: No billing run has left nothing pending in 26h.
  description: Either no run has ended in 26h, or every run that did left invoices pending. Look at the CronJob's Jobs and the latest run's logs.
```

### BillingInvoicesPending

Warning. The last run left invoices pending:

```yaml
expr: max(last_over_time(billing_invoices_pending{job="billing"}[26h])) or vector(0)
```

The Job retries the run, and the next day's run charges whatever is left.
The run's exit cause says why it left them:

```yaml
labels:
  severity: warning
annotations:
  summary: The last billing run left {{ $values.A.Value }} invoices pending.
  description: The Job retries the run, and the next scheduled run charges whatever is still pending. The run's exit cause says why they were left.
```

### BillingInvoicesFailed

Warning. An invoice failed in the last day:

```yaml
expr: sum(max_over_time(billing_invoices_settled{job="billing", billing_invoice_state="failed"}[1d])) or vector(0)
```

A failed invoice is final, so a person has to look at each one. Its
`invoice failed` record names it, with the answer that failed it:

```yaml
labels:
  severity: warning
annotations:
  summary: Billing failed {{ $values.A.Value }} invoices in the last day.
  description: A failed invoice is final, and keeps the payments API's answer that failed it. A person has to look at each one.
```

### BillingPaymentsBreakerOpened

Info. The payments client's breaker opened in the last day:

```yaml
expr: sum(max_over_time(humus_circuit_transitions{job="billing", humus_circuit_state="open"}[1d])) or vector(0)
```

The run stopped charging when it opened, and left the rest pending for its
retry or the next run. **Payments breaker** on the dashboard shows when:

```yaml
labels:
  severity: info
annotations:
  summary: The payments client's breaker opened in the last day.
  description: A run stops charging when the breaker opens, and leaves the rest pending for its retry or the next run.
```

Next: [Production-ready](/humus/pr-preview/pr-1310/walkthroughs/job/billing/production-ready/)
