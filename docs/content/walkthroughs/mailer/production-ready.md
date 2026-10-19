---
title: Production-ready
linkTitle: Production-ready
description: >
  The checklist the mailer meets, and how it meets each item, with the page
  that builds it. Check your own RabbitMQ worker against it.
type: docs
weight: 100
---

The mailer meets each item. Check your own worker against them:

| Item | How | Where |
|---|---|---|
| Every setting sourced and checked; a bad one exits 78, naming its variable | `config.Struct` and the packages' checks | [The settings]({{< relref "walkthroughs/mailer/init#the-settings" >}}) |
| No secret in the environment, the logs or the spans | the RabbitMQ password, the provider's key and the probes' certificate are files read through `s.Host.FS` | [The secrets, as files]({{< relref "walkthroughs/mailer/init#the-secrets-as-files" >}}) |
| Certificates renewed without a restart | the probes' certificate is served through `GetCertificate`, by a small helper of the walkthrough's own that reads its files again when they change | [The probes]({{< relref "walkthroughs/mailer/init#the-probes" >}}) |
| No personal data in the telemetry | addresses and bodies are never logged or put on a span | [What it records]({{< relref "walkthroughs/mailer/processing#what-it-records" >}}) |
| Two replicas, competing for one queue | a PodDisruptionBudget of `minAvailable: 1` | [The disruption budget]({{< relref "walkthroughs/mailer/deploying#the-disruption-budget" >}}) |
| Ready only once it consumes | `Consuming` as readiness, on a probes server | [The probes]({{< relref "walkthroughs/mailer/init#the-probes" >}}) |
| A rollout loses nothing, and sends nothing twice | SIGTERM cancels the consumer, finishes the deliveries in flight within 25s, and nacks the rest, which counts nothing against them. `terminationGracePeriodSeconds: 35` | [The grace period]({{< relref "walkthroughs/mailer/deploying#the-grace-period" >}}) |
| A broker's rolling restart costs no restart | the Service reconnects. The deliveries that were in flight are redelivered, and the idempotency key sends them once | [The request and its key]({{< relref "walkthroughs/mailer/provider#the-request-and-its-key" >}}) |
| A dependency's outage is ridden out | the client's retries, then the delay queue, then the parking lot after `MAIL_MAX_ATTEMPTS`. The breaker sheds calls while the provider keeps failing | [Retries and dead letters]({{< relref "walkthroughs/mailer/retries" >}}) |
| No request stops the worker | malformed requests go to the parking lot at once. The rest are parked in `mail.send.retry`, an event that can't be published included, and dead-lettered after `MAIL_MAX_ATTEMPTS`. Only a request that can go to neither `mail.retry` nor `mail.dead` stops the worker, and the delivery limit dead-letters it after 5 deliveries | [The delivery limit]({{< relref "walkthroughs/mailer/retries#the-delivery-limit" >}}) |
| Least privilege | no configure permission on the broker; write and read on its own exchanges and queue only, granted with `rabbitmqctl set_permissions`. No service account token. A non-root, read-only container | [The logins]({{< relref "walkthroughs/mailer/topology#the-logins" >}}), [The Deployment]({{< relref "walkthroughs/mailer/deploying#the-deployment" >}}) |
| Observable | traces, logs and metrics over OTLP, and the alerts in `deploy/alerts.yaml` | [Observing]({{< relref "walkthroughs/mailer/observing" >}}) |
| Failures say why | each dead letter's reason headers, and the `mail.refused` event | [Dead letters]({{< relref "walkthroughs/mailer/retries#dead-letters" >}}), [The events]({{< relref "walkthroughs/mailer/processing#the-events" >}}) |
| Failures are rehearsed | the failure injection cases | [The failure injection cases]({{< relref "walkthroughs/mailer/testing#the-failure-injection-cases" >}}) |
| Safe by default | `TrustScenario` trusts no publisher; the sender's address is a setting | [`TrustScenario`]({{< relref "walkthroughs/mailer/init#trustscenario" >}}) |
