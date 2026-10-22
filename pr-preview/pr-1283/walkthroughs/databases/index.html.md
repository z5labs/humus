# Databases controller

The databases controller reconciles `Database` resources, with
[`kube`](/humus/pr-preview/pr-1283/kubernetes/). For each one, it:

1. ensures a role, and a database the role owns, in a PostgreSQL server;
2. applies a Secret, which the `Database` controls, holding the role's
   credentials;
3. sets the `Database`'s `Ready` condition, saying which step failed if one
   did.

Deleting a `Database` drops its database and role, or keeps them, as its
`deletionPolicy` says. The controller runs two replicas, and the one that
holds the Lease reconciles.

## The `Database` resource

A `Database`, as the kind overlay applies it:

```yaml
apiVersion: postgres.example.com/v1alpha1
kind: Database
metadata:
  name: orders
spec:
  name: orders
  deletionPolicy: Delete
```

Once it's provisioned, its `status.secretName` is `orders-postgres`, and its
`Ready` condition is `True`, with the reason `Provisioned`.

## Its names

| Name | What it is |
|---|---|
| `postgres.example.com`, `v1alpha1`, `Database`, `databases` | the resource's group, version, kind and plural |
| `spec.name` | the database's name in the server, and its role's |
| `<metadata.name>-postgres` | the Secret holding the role's credentials, in the `Database`'s namespace |
| `postgres.example.com/finalizer` | the finalizer that keeps a deleted `Database` until it's cleaned up |
| `postgres.example.com/failure-scenario` | the annotation that carries a failure-injection scenario |
| `databases-controller` | the controller's name, its Lease's, and the field manager of its applies |

## The files it ends with

Its code is a module of its own, at
[`example/databases`](https://github.com/z5labs/humus/tree/main/example/databases):

```text
example/databases/
├── main.go             humus.Main(app.Init, ...)
├── api/v1alpha1/       the Database resource and its rules
├── postgres/           the admin interface, and the Server that implements
│                       it on PostgreSQL
├── controller/         the Reconciler and its finalizer
├── app/                Init, the settings, BuildTelemetry and Faults
├── internal/certfile/  the probes' certificate, read again when it's renewed
├── cases/              the test cases the in-process tests and kind share
├── functional/         the cases against kind, and the Grafana check
├── deploy/             the golden CRD, the kustomization, the alerts and
│                       dashboard, and the kind overlay
└── Dockerfile          the databases image
```

These pages build `api/v1alpha1`, `postgres` and `controller`, then `app`
and `main`, then test it, deploy it and observe it, and end with the
production-ready checklist.
