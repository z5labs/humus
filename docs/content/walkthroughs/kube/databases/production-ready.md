---
title: Production-ready
linkTitle: Production-ready
description: >
  The checklist the databases controller meets, and how it meets each item,
  with the page that builds it. Check your own Kubernetes controller against
  it.
type: docs
weight: 100
---

The controller meets each item. Check your own controller against them:

| Item | How | Where |
|---|---|---|
| Every setting sourced and checked; a bad one exits 78, naming its variable | `config.Struct`, `kube`'s checks and the controller's own | [The settings]({{< relref "/walkthroughs/kube/databases/init#the-settings" >}}) |
| No secret in the environment, the logs or the spans | the admin DSN, the service account's token and CA, and the probes' key are files read through `s.Host.FS`. A role's password reaches PostgreSQL as a SCRAM verifier, and a statement's span carries its summary, never its SQL. A test of the App checks that no password, DSN or token is in a span, a record or a metric | [The secrets, as files]({{< relref "/walkthroughs/kube/databases/init#the-secrets-as-files" >}}), [Keeping the password out of the SQL]({{< relref "/walkthroughs/kube/databases/postgres#keeping-the-password-out-of-the-sql" >}}), [Keeping credentials out of the spans]({{< relref "/walkthroughs/kube/databases/postgres#keeping-credentials-out-of-the-spans" >}}) |
| Certificates renewed without a restart | the probes' certificate is served through `GetCertificate`, by a small helper of the walkthrough's own that reads its file again when it changes. The token's file is read each time the Client asks, so a rotated one is picked up | [The probes]({{< relref "/walkthroughs/kube/databases/init#the-probes" >}}), [The Connection]({{< relref "/walkthroughs/kube/databases/init#the-connection" >}}) |
| Available through a lost replica | two replicas share the Lease, and the standby takes over when the leader stops. A rollout starts a new replica before it stops an old one, and a PodDisruptionBudget of `minAvailable: 1` | [The Settings and the Lease]({{< relref "/walkthroughs/kube/databases/init#the-settings-and-the-lease" >}}), [The Deployment]({{< relref "/walkthroughs/kube/databases/deploying#the-deployment" >}}), [The disruption budget]({{< relref "/walkthroughs/kube/databases/deploying#the-disruption-budget" >}}) |
| Ready only once its caches have synced | `Synced` as readiness, on an HTTPS probes port | [The probes]({{< relref "/walkthroughs/kube/databases/init#the-probes" >}}) |
| A rollout loses nothing | SIGTERM finishes the reconciles in flight and releases the Lease, in a grace period of 20s, and `terminationGracePeriodSeconds` is 30. The next leader lists every `Database` again | [The grace period]({{< relref "/walkthroughs/kube/databases/deploying#the-grace-period" >}}), [`main`]({{< relref "/walkthroughs/kube/databases/init#main" >}}) |
| Reconciling again is harmless | the Secret is the password's source of truth, so every reconcile sets the same one. The role and the database are ensured, and the drops are `IF EXISTS` | [The password]({{< relref "/walkthroughs/kube/databases/reconciling#the-password" >}}), [Provisioning]({{< relref "/walkthroughs/kube/databases/postgres#provisioning" >}}), [Dropping]({{< relref "/walkthroughs/kube/databases/cleaning-up#dropping" >}}) |
| Deleting cleans up, as the `Database` says | the finalizer holds a deleted `Database` until `Finalize` has dropped what its `deletionPolicy` says to. The Secret is deleted with it, as the `Database` controls it | [The finalizer]({{< relref "/walkthroughs/kube/databases/cleaning-up#the-finalizer" >}}), [The Secret]({{< relref "/walkthroughs/kube/databases/cleaning-up#the-secret" >}}) |
| Least privilege | a ClusterRole that watches and patches `Database`s and applies Secrets, and a Role for its own Lease and no other. A non-root container, on a read-only root filesystem, with every capability dropped | [RBAC]({{< relref "/walkthroughs/kube/databases/deploying#rbac" >}}), [The Deployment]({{< relref "/walkthroughs/kube/databases/deploying#the-deployment" >}}) |
| Observable | traces, logs and metrics over OTLP, and rules for failing, panicking and stuck reconciles, a backlog and restarts, on humus's own instruments, and a dashboard | [Observing]({{< relref "/walkthroughs/kube/databases/observing" >}}) |
| Failures say why | a failed step sets `Ready` to `False`, with a reason naming the step, `PostgresUnavailable` or `SecretApplyFailed` | [The conditions]({{< relref "/walkthroughs/kube/databases/reconciling#the-conditions" >}}) |
| Failures are rehearsed | the failure injection cases | [The failure injection cases]({{< relref "/walkthroughs/kube/databases/testing#the-failure-injection-cases" >}}) |
| Safe by default | `deletionPolicy` defaults to `Retain`, and `name` is immutable. No namespace's scenarios are trusted | [The resource and its rules]({{< relref "/walkthroughs/kube/databases/resource#the-resource-and-its-rules" >}}), [Trusting a namespace's scenarios]({{< relref "/walkthroughs/kube/databases/init#trusting-a-namespaces-scenarios" >}}) |
