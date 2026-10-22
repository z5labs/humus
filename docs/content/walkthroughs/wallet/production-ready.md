---
title: Production-ready
linkTitle: Production-ready
description: >
  The checklist the wallet and the ledger meet, and how they meet each item,
  with the page that builds it. Check your own gRPC services against it.
type: docs
weight: 110
---

The wallet and the ledger meet each item. Check your own gRPC services
against them:

| Item | How | Where |
|---|---|---|
| Every setting sourced and checked; a bad one exits 78, naming its variable | `config.Struct` and the packages' checks, in both services | [The wallet's settings]({{< relref "walkthroughs/wallet/init#the-wallets-settings" >}}), [The ledger's settings]({{< relref "walkthroughs/wallet/init#the-ledgers-settings" >}}) |
| No secret in the environment, the logs or the spans | PostgreSQL's password and the private keys are files, read through `s.Host.FS`. The token is metadata, which otelgrpc doesn't record, and no handler logs it, which a test of the Apps checks | [The secrets, as files]({{< relref "walkthroughs/wallet/init#the-secrets-as-files" >}}), [Their log records]({{< relref "walkthroughs/wallet/observing#their-log-records" >}}) |
| Certificates and signing keys renewed without a restart | `internal/certfile`, for both servers' certificates and the wallet's client certificate. The key set is fetched again for a `kid` the wallet lacks | [The certificates, read again when renewed]({{< relref "walkthroughs/wallet/init#the-certificates-read-again-when-renewed" >}}), [The fetched key set]({{< relref "walkthroughs/wallet/serving-the-wallet#the-fetched-key-set" >}}) |
| Two replicas of each service | a PodDisruptionBudget of `minAvailable: 1`, and a rolling update of `maxUnavailable: 0` | [The Deployments]({{< relref "walkthroughs/wallet/deploying#the-deployments" >}}), [The disruption budgets]({{< relref "walkthroughs/wallet/deploying#the-disruption-budgets" >}}) |
| Ready once it has reached its dependency, and still ready through its outages | `Connected`, and the ledger's latch on PostgreSQL | [Readiness: `Connected`]({{< relref "walkthroughs/wallet/calling-the-ledger#readiness-connected" >}}), [The ledger's probes]({{< relref "walkthroughs/wallet/serving-the-ledger#the-probes" >}}), [Readiness]({{< relref "walkthroughs/wallet/init#readiness" >}}) |
| A rollout loses no call | `grpcserver.Main`'s 5s drain. A unary call is bounded by `LEDGER_TIMEOUT`, 10s, inside the 15s grace period. Watches end with `Unavailable` and are opened again. `terminationGracePeriodSeconds` is 30, above the drain, the grace period and the telemetry's shutdown | [The drain]({{< relref "walkthroughs/wallet/deploying#the-drain" >}}), [Relaying the watch]({{< relref "walkthroughs/wallet/serving-the-wallet#relaying-the-watch" >}}) |
| Calls spread across pods | `MaxConnectionAge` on both servers | [`MaxConnectionAge`]({{< relref "walkthroughs/wallet/init#maxconnectionage" >}}) |
| Money is never made or lost | one transaction per transfer, locks in the order of the IDs, and a `CHECK` that no account but `external` goes below zero | [`schema.sql`]({{< relref "walkthroughs/wallet/accounts#schemasql" >}}), [A transfer's transaction]({{< relref "walkthroughs/wallet/accounts#a-transfers-transaction" >}}) |
| A retry never moves money twice | every transfer's ID: the subject and the request's ID | [Why every transfer is safe to retry]({{< relref "walkthroughs/wallet/calling-the-ledger#why-every-transfer-is-safe-to-retry" >}}) |
| The contract never breaks a caller | `buf breaking` against main, in CI | [buf's rules]({{< relref "walkthroughs/wallet/contract#bufs-rules" >}}), [`buf generate`]({{< relref "walkthroughs/wallet/contract#buf-generate" >}}) |
| Least privilege | the ledger's role has `SELECT`, `INSERT` and `UPDATE` on its two tables. Only the internal CA's certificates call the ledger. A token can use only its subject's account. A non-root, read-only container, with no service account token | [Running it on kind]({{< relref "walkthroughs/wallet/deploying#running-it-on-kind" >}}), [mTLS]({{< relref "walkthroughs/wallet/serving-the-ledger#mtls" >}}), [`grpcauth`]({{< relref "walkthroughs/wallet/serving-the-ledger#grpcauth" >}}), [The subject's account]({{< relref "walkthroughs/wallet/serving-the-wallet#the-subjects-account" >}}), [The Deployments]({{< relref "walkthroughs/wallet/deploying#the-deployments" >}}) |
| Observable | traces across both services, logs and metrics over OTLP, `deploy/alerts.yaml`, `deploy/dashboard.json`, and alerts on each Deployment from the Collector's Kubernetes cluster receiver | [Observing]({{< relref "walkthroughs/wallet/observing" >}}) |
| Failures say why | statuses with `PreconditionFailure`, `BadRequest`, `ErrorInfo` and `RetryInfo` details | [The statuses, with details]({{< relref "walkthroughs/wallet/serving-the-ledger#the-statuses-with-details" >}}), [The ledger's answers, as the wallet's]({{< relref "walkthroughs/wallet/calling-the-ledger#the-ledgers-answers-as-the-wallets" >}}), [A key set that can't be fetched]({{< relref "walkthroughs/wallet/serving-the-wallet#a-key-set-that-cant-be-fetched" >}}) |
| Failures are rehearsed | the failure injection cases, in CI and against staging through its own `Target` | [The shared cases]({{< relref "walkthroughs/wallet/testing#the-shared-cases" >}}), [The failure injection cases]({{< relref "walkthroughs/wallet/testing#the-failure-injection-cases" >}}) |
| Safe by default | `FAULT_TRUST_SCOPE` empty, so no scenario is trusted. Server reflection off. The probes ports serve gRPC health and nothing else | [`TrustScenario`]({{< relref "walkthroughs/wallet/init#trustscenario" >}}), [The gRPC probes]({{< relref "walkthroughs/wallet/deploying#the-grpc-probes" >}}) |
