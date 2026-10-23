# Billing job

The billing job runs once a day, as a Kubernetes CronJob. Each run:

1. issues an invoice for every active subscription, for last month;
2. charges every invoice still pending, from any month, through a payments
   API;
3. records each answer, and exits 0 when no invoice is left pending, or 1
   when any is.

The next run charges whatever a failed run left pending. A run that is
retried, or that overlaps another, charges nothing twice, because each
invoice's ID is its charge's idempotency key.

## The files it ends with

Its code is a module of its own, at
[`example/job/billing`](https://github.com/z5labs/humus/tree/main/example/job/billing):

```text
example/job/billing/
├── main.go          humus.Main(app.Init, app.Options()...)
├── app/             Init, the settings, Faults, the telemetry Builder and Options
├── billing/         the run: issue, charge, settle, and what it returns
├── postgres/        the invoices in PostgreSQL, and schema.sql
├── payments/        the payments API's client: the request, the answers and Classify
├── cases/           the test cases the container tests and kind share
├── functional/      the cases against kind
├── deploy/          the CronJob, its alerts and dashboard, and the kind overlay
└── Dockerfile       the job's image
```

These pages build `postgres`, `payments`, `billing`, then `app` and `main`.

Next: [The invoices](/humus/pr-preview/pr-1310/walkthroughs/job/billing/invoices/)
