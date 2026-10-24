# Order status processor

The order status processor consumes orders' lifecycle events from Kafka. For
each one, it:

1. folds the order's events into its status, through a state machine;
2. produces each change of status to `orders.status`, for the services that
   want them;
3. stores the event and the status in PostgreSQL.

An event that fails for now goes to `orders.retry`, and is tried again after
a wait. One that never will succeed goes to `orders.dlq`, with why. No record
stops it, and a record processed twice changes nothing.

## The topics

| Topic | Key | Value | Written by |
|---|---|---|---|
| `orders.events` | the order's ID | an event, `type` and `at` | the end user's other services |
| `orders.status` | the order's ID | a change of status, `order_id`, `status`, `previous` and `at` | the processor |
| `orders.retry` | the order's ID | the consumed record's value, unchanged | the processor, for each record it will try again |
| `orders.dlq` | the order's ID | the consumed record's value, unchanged | the processor, for each record it gives up on |

An event's `type` is one of the statuses:

```go
const (
	Placed    = "placed"
	Paid      = "paid"
	Shipped   = "shipped"
	Delivered = "delivered"
	Cancelled = "cancelled"
)
```

An event, and a change of status, as JSON:

```go
type event struct {
	Type string    `json:"type"`
	At   time.Time `json:"at"`
}
```

```go
type change struct {
	OrderID  string    `json:"order_id"`
	Status   string    `json:"status"`
	Previous string    `json:"previous"`
	At       time.Time `json:"at"`
}
```

## The files it ends with

Its code is a module of its own, at
[`example/kafka/orderstatus`](https://github.com/z5labs/humus/tree/main/example/kafka/orderstatus):

```text
example/kafka/orderstatus/
├── main.go             humus.Main(app.Init, ...)
├── app/                Init, the settings, BuildTelemetry and Faults
├── store/              the projection in PostgreSQL, and its schema.sql
├── projector/          the Processor: decode, fold, produce, store, retry
├── internal/certfile/  the certificates, read again when they're renewed
├── cmd/orderevents/    a CLI that produces an order's events, to try the
│                       processor by hand
├── cases/              the test cases the in-process tests and kind share
├── functional/         the cases against kind
├── deploy/             the kustomization, the alerts and dashboard, and the
│                       kind overlay
└── Dockerfile          the orderstatus and orderevents image
```

These pages create the topics, then build `store`, `projector`, then `app`
and `main`, then test it, deploy it and observe it, and end with the
production-ready checklist.

Next: [The topics](/humus/pr-preview/pr-1329/walkthroughs/kafka/orderstatus/topics/)
