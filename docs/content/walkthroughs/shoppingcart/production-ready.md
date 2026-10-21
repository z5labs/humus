---
title: Production-ready
linkTitle: Production-ready
description: >
  The checklist the cart and the catalog meet, and how they meet each item,
  with the page that builds it. Check your own API against it.
type: docs
weight: 110
---

The cart and the catalog meet each item. Check your own API against them:

| Item | How | Where |
|---|---|---|
| Every setting sourced and checked; a bad one exits 78, naming its variable | `config.Struct` and the packages' checks, in the cart, the catalog and `migrate` | [The cart's settings]({{< relref "walkthroughs/shoppingcart/init#the-carts-settings" >}}), [The other checks]({{< relref "walkthroughs/shoppingcart/init#the-other-checks" >}}), [`migrate`'s settings]({{< relref "walkthroughs/shoppingcart/schema#migrates-settings" >}}) |
| No secret in the environment, the logs or the spans | the database URLs and the keys are files, read through `s.Host.FS`. No token is recorded, which a test of the App checks | [The secrets, as files]({{< relref "walkthroughs/shoppingcart/init#the-secrets-as-files" >}}), [Their log records]({{< relref "walkthroughs/shoppingcart/observing#their-log-records" >}}) |
| Certificates renewed without a restart | `internal/certfile`, on the `kubernetes.io/tls` Secrets that cert-manager renews. Its unit tests rewrite the files under a running server | [The certificates, read again when renewed]({{< relref "walkthroughs/shoppingcart/init#the-certificates-read-again-when-renewed" >}}) |
| Two replicas of each service | a PodDisruptionBudget of `minAvailable: 1`, and a rolling update of `maxUnavailable: 0` | [The Deployments]({{< relref "walkthroughs/shoppingcart/deploying#the-deployments" >}}), [The disruption budgets]({{< relref "walkthroughs/shoppingcart/deploying#the-disruption-budgets" >}}) |
| Ready only once it has reached its dependencies, and still ready through their outages | the latching monitors, on PostgreSQL and on the catalog's readiness | [Readiness]({{< relref "walkthroughs/shoppingcart/init#readiness" >}}), [The catalog's probes]({{< relref "walkthroughs/shoppingcart/catalog#the-probes" >}}), [The cart's readiness]({{< relref "walkthroughs/shoppingcart/calling-the-catalog#readiness" >}}) |
| A rollout loses no request | `httpserver.Main`'s 5s drain, in both services, and a `terminationGracePeriodSeconds` above the drain, the grace period and the telemetry's shutdown | [The drain]({{< relref "walkthroughs/shoppingcart/deploying#the-drain" >}}) |
| The schema is current before the code that needs it serves | the `migrate` init container, under tern's lock, with backwards-compatible migrations | [The migrations]({{< relref "walkthroughs/shoppingcart/schema#the-migrations" >}}), [The job]({{< relref "walkthroughs/shoppingcart/schema#the-job" >}}), [The init container]({{< relref "walkthroughs/shoppingcart/schema#the-init-container" >}}) |
| Least privilege | the services' roles read and write their tables and change no schema. A non-root, read-only container. No service account token mounted | [The grant]({{< relref "walkthroughs/shoppingcart/schema#the-grant" >}}), [The roles]({{< relref "walkthroughs/shoppingcart/schema#the-roles" >}}), [The Deployments]({{< relref "walkthroughs/shoppingcart/deploying#the-deployments" >}}) |
| A caller sees only their own carts | the owner, from the token's subject, and a 404 for anyone else | [A cart's owner]({{< relref "walkthroughs/shoppingcart/access#a-carts-owner" >}}) |
| Prices can't be set by a client | the cart asks the catalog for each price, over mTLS. Only a token with `catalog:write` changes one | [The bodies]({{< relref "walkthroughs/shoppingcart/api#the-bodies" >}}), [mTLS]({{< relref "walkthroughs/shoppingcart/catalog#mtls" >}}), [The operations]({{< relref "walkthroughs/shoppingcart/catalog#the-operations" >}}) |
| Observable | traces across both services, logs and metrics over OTLP, the alerts in `deploy/alerts.yaml`, `deploy/dashboard.json`, and alerts on each Deployment from the Collector's Kubernetes cluster receiver | [Observing]({{< relref "walkthroughs/shoppingcart/observing" >}}) |
| Failures say why | a 503 for a dependency that is down, a 422 for an unknown product, and problem details throughout | [The answers]({{< relref "walkthroughs/shoppingcart/api#the-answers" >}}), [The catalog's answers]({{< relref "walkthroughs/shoppingcart/calling-the-catalog#the-answers" >}}) |
| Failures are rehearsed | the failure injection cases, in CI, and against an end user's staging through its own `Target` | [The shared cases]({{< relref "walkthroughs/shoppingcart/testing#the-shared-cases" >}}), [The failure injection cases]({{< relref "walkthroughs/shoppingcart/testing#the-failure-injection-cases" >}}) |
| Safe by default | `FAULT_TRUST_SCOPE` empty, so no scenario is trusted. The OpenAPI document is the only open path besides the probes | [`TrustScenario`]({{< relref "walkthroughs/shoppingcart/init#trustscenario" >}}), [Requiring it]({{< relref "walkthroughs/shoppingcart/access#requiring-it" >}}) |
