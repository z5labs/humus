---
title: Cleaning up
linkTitle: Cleaning up
description: >
  How the databases controller cleans up a deleted Database: a finalizer
  keeps it until the role and the database are dropped, or kept, as its
  deletionPolicy says, and a failed drop keeps it until a retry succeeds.
type: docs
weight: 50
---

The database and the role are outside the cluster, so no owner reference can
garbage collect them. A finalizer keeps a deleted `Database` until the
controller has cleaned up after it.

## The finalizer

`Init` registers the Reconciler's `Finalize` under the finalizer's name:

```go
const (
	serviceAccount     = "/var/run/secrets/kubernetes.io/serviceaccount/"
	finalizer          = "postgres.example.com/finalizer"
	scenarioAnnotation = "postgres.example.com/failure-scenario"
)
```

```go
kube.Finalize(finalizer, rec.Finalize),
```

The controller adds the finalizer to each `Database`. Once one is deleted, it
calls `Finalize`, and removes the finalizer when `Finalize` succeeds, which
lets the API server delete the `Database`.

## `deletionPolicy`

`Finalize` drops the role and the database only when the policy is
`Delete`. With `Retain`, the default, it drops nothing:

```go
func (r Reconciler) Finalize(ctx context.Context, db database) error {
	if db.Spec.DeletionPolicy != v1alpha1.Delete {
		return nil
	}
	return r.Admin.Drop(ctx, db.Spec.Name)
}
```

## Dropping

`Drop` injects at `postgresql:drop`, drops the database, then the role.
`WITH (FORCE)` ends the database's connections, since a drop fails while
any are open. `IF EXISTS` makes a second drop succeed:

```go
func (s *Server) Drop(ctx context.Context, name string) error {
	if err := s.inject(ctx, "postgresql:drop"); err != nil {
		return fmt.Errorf("dropping %s: %w", name, err)
	}
	ident := pgx.Identifier{name}.Sanitize()
	if err := s.exec(ctx, "DROP DATABASE", "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("dropping %s: dropping its database: %w", name, err)
	}
	if err := s.exec(ctx, "DROP ROLE", "DROP ROLE IF EXISTS "+ident); err != nil {
		return fmt.Errorf("dropping %s: dropping its role: %w", name, err)
	}
	return nil
}
```

A failed drop returns its error, so the `Database` keeps its finalizer, and
`Finalize` is retried, with backoff, until it succeeds.

## The Secret

The Secret needs no cleaning up: the `Database` is its controller, so
Kubernetes' garbage collector deletes it with the `Database`.
