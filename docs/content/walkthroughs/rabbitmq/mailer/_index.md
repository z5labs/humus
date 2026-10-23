---
title: Mailer
linkTitle: Mailer
description: >
  A walkthrough of a RabbitMQ worker on humus that sends the emails other
  services ask for through a provider's API, retries what fails for now after
  a delay, and dead-letters what never will. These pages build it step by
  step.
type: docs
weight: 20
---

The mailer consumes requests for emails from RabbitMQ. For each one, it:

1. calls the provider's API, which sends the email;
2. publishes what became of it, `mail.sent` or `mail.refused`, for the
   services that asked;
3. parks a request that failed for now, which comes back after a delay, and
   dead-letters one that never will succeed, with why.

No request stops it, and a request delivered twice sends one email, because
its `MessageId` is the provider's idempotency key.

## The contract

A request is published to the `mail` exchange with the routing key
`mail.send`:

- its `MessageId` is required, and is the provider's idempotency key;
- its `CorrelationId` is optional, and the events carry it back;
- its body is JSON, `to`, `subject` and `text`, up to 256 KiB.

The sender's address is the mailer's setting, so no publisher can send as
anyone else.

An event is published to the `mail.events` topic exchange, with the
request's `MessageId` and `CorrelationId`:

- `mail.sent`, with the provider's ID for the email, as `provider_id`;
- `mail.refused`, with the provider's `reason`, for a request the provider
  will never accept.

A service that wants the events binds a queue of its own to `mail.events`.

## The names

The `mail` package holds the contract and the topology's names:

```go
const (
	Exchange       = "mail"
	RetryExchange  = "mail.retry"
	DeadExchange   = "mail.dead"
	EventsExchange = "mail.events"
)

const (
	SendQueue  = "mail.send"
	RetryQueue = "mail.send.retry"
	DeadQueue  = "mail.send.dead"
)

const (
	SendKey    = "mail.send"
	SentKey    = "mail.sent"
	RefusedKey = "mail.refused"
)
```

| Name | Kind | What it does |
|---|---|---|
| `mail` | direct exchange | where requests are published |
| `mail.send` | quorum queue, bound to `mail` by `mail.send` | the work queue the mailer consumes |
| `mail.retry` | direct exchange | where the mailer parks a request that failed for now |
| `mail.send.retry` | quorum queue, bound to `mail.retry` | holds each parked request for the delay, then dead-letters it back to `mail` |
| `mail.dead` | fanout exchange | dead letters, from the mailer and from `mail.send`'s delivery limit |
| `mail.send.dead` | quorum queue, bound to `mail.dead` | the parking lot, where dead letters wait with why they died |
| `mail.events` | topic exchange | `mail.sent` and `mail.refused` |

## The files it ends with

Its code is a module of its own, at
[`example/rabbitmq/mailer`](https://github.com/z5labs/humus/tree/main/example/rabbitmq/mailer):

```text
example/rabbitmq/mailer/
├── main.go             humus.Main(app.Init, ...), with a grace period of 25s
├── app/                Init, the settings, BuildTelemetry and Faults
├── mail/               the request and the events, and the topology's names
├── provider/           the provider's client, over httpclient, and its Classify
├── worker/             the Processor: decode, send, decide, publish
├── internal/certfile/  the probes' certificate, read again when it's renewed
├── cmd/mailrequest/    a CLI that publishes a request, to try the mailer by hand
├── cases/              the test cases the container tests and kind share
├── functional/         the cases against kind
├── deploy/             definitions.json, the kustomization, the alerts and
│                       dashboard, and the kind overlay
└── Dockerfile          the mailer's and mailrequest's image
```

These pages set up the topology, then build `provider`, `mail` and `worker`,
then `app` and `main`, then test it, deploy it and observe it, and end with
the production-ready checklist.
