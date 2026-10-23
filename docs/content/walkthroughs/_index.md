---
title: Walkthroughs
linkTitle: Walkthroughs
description: >
  Example applications that are production-ready, each with pages that build
  it step by step: the bar every one meets, which its pages close with as a
  checklist, and the three levels of tests it runs.
type: docs
weight: 60
---

A walkthrough is an example application that is production-ready, with pages
that build it step by step. Its code is a module of its own, at
`example/<type>/<name>` in the repository, under the folder of its service
type, and every sample on its pages is quoted from that code.

## The walkthroughs

- [Shoppingcart]({{< relref "/walkthroughs/http/shoppingcart" >}}): an API for
  shopping carts, behind JWT bearer tokens, that prices each item from a
  catalog service of its own over mTLS, both on PostgreSQL.
- [Wallet]({{< relref "/walkthroughs/grpc/wallet" >}}): a gRPC service for each
  caller's own account, behind JWT bearer tokens, on a ledger service of its
  own that it calls over mTLS, which keeps the accounts and the transfers
  between them in PostgreSQL.
- [Billing job]({{< relref "/walkthroughs/job/billing" >}}): a job, run daily by
  a CronJob, that bills a month of subscriptions in PostgreSQL through a
  payments API, once, however often it runs.
- [Order status processor]({{< relref "/walkthroughs/kafka/orderstatus" >}}): a
  Kafka processor that projects orders' events into their statuses in
  PostgreSQL, produces each change of status, retries what fails for now
  through a retry topic, and dead-letters what never will.
- [Mailer]({{< relref "/walkthroughs/rabbitmq/mailer" >}}): a RabbitMQ worker that
  sends the emails other services ask for through a provider's API, retries
  what fails for now after a delay, and dead-letters what never will.
- [Databases controller]({{< relref "/walkthroughs/kube/databases" >}}): a
  Kubernetes controller that provisions a database and the role that owns it
  in PostgreSQL for each `Database`, writes the role's credentials to a
  Secret, and drops both on delete, as the `Database` says.

## The bar

Each walkthrough's pages close with this checklist, and the application meets
every item.

- **Settings:** every one is sourced and checked, and a bad one exits 78,
  naming its variable.
- **Secrets:** files mounted from Secrets and read through the Host's `FS`,
  never environment variables, and none reaches a log or a span.
- **Certificates:** files too, read again when they're renewed, so a renewal
  needs no restart. A job reads them afresh on every run.
- **Availability:** replicas and a PodDisruptionBudget, or, for a job, its
  concurrency and its retries.
- **Readiness:** its probes. A job serves none.
- **Rollouts and shutdown:** what SIGTERM does, the drain, and
  `terminationGracePeriodSeconds` against humus's defaults.
- **Least privilege:** a non-root container with a read-only root filesystem,
  and RBAC if it needs any.
- **Observability:** traces, logs and metrics over OTLP, alerts on its
  instruments, humus's own included, and a dashboard.
- **Failures say why, and failures are rehearsed,** with failure injection.
- **Safe defaults.**

## Three levels of tests

1. **Units:** the application's own types and functions, as plain Go tests,
   inside `synctest` where they take time.
2. **The whole App, in process,** with `humustest.Start`: on humustest's
   `Host` where its dependencies can run there, and on `humus.OSHost()`
   against containers, behind the `testcontainers` tag, where they can't.
3. **The deployed image,** in a kind cluster, behind the `functional` tag.

The second and third levels run the same black-box cases, written once.
