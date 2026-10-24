# Reconciling

The `controller` package's `Reconciler` brings a `Database` to its spec,
whatever state it finds. A reconcile that fails is retried, with backoff, so
each step is safe to repeat.

## The `Reconciler`

It holds a clientset for the Secret, the `Admin`, and the server's address,
which the Secret hands on:

```go
const fieldManager = "databases-controller"

type database = kube.Object[v1alpha1.DatabaseSpec, v1alpha1.DatabaseStatus]

type Reconciler struct {
	Kube  kubernetes.Interface
	Admin postgres.Admin
	Host  string
	Port  int
}

var _ kube.Reconciler[v1alpha1.DatabaseSpec, v1alpha1.DatabaseStatus] = Reconciler{}
```

## The steps

`Reconcile` reads the password, provisions the role and the database,
applies the Secret, then sets `Ready`. An apply the API server refuses
with a conflict means the Secret was deleted after it was read, so the
password is rotated there and then:

```go
func (r Reconciler) Reconcile(ctx context.Context, db database) (kube.Result[v1alpha1.DatabaseStatus], error) {
	st := db.Status
	st.Conditions = slices.Clone(st.Conditions)
	name := db.Name + "-postgres"

	password, uid, err := r.password(ctx, db.Namespace, name)
	if err != nil {
		return r.failed(db, st, "SecretApplyFailed", "The Secret could not be read.", fmt.Errorf("reading Secret %s/%s: %w", db.Namespace, name, err))
	}

	if err := r.Admin.Provision(ctx, db.Spec.Name, password); err != nil {
		return r.failed(db, st, "PostgresUnavailable", "The role and the database could not be provisioned in PostgreSQL.", err)
	}

	err = r.applySecret(ctx, db, name, uid, password)
	if uid != "" && apierrors.IsConflict(err) {
		password = rand.Text()
		if err := r.Admin.Provision(ctx, db.Spec.Name, password); err != nil {
			return r.failed(db, st, "PostgresUnavailable", "The role and the database could not be provisioned in PostgreSQL.", err)
		}
		err = r.applySecret(ctx, db, name, "", password)
	}
	if err != nil {
		return r.failed(db, st, "SecretApplyFailed", "The Secret could not be applied.", fmt.Errorf("applying Secret %s/%s: %w", db.Namespace, name, err))
	}

	st.SecretName = name
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Provisioned",
		Message:            "The role, the database and the Secret are provisioned.",
		ObservedGeneration: db.Generation,
	})
	return kube.Result[v1alpha1.DatabaseStatus]{Status: &st}, nil
}
```

## The password

The Secret is the password's source of truth. A new password is made only
when there's no Secret, or no password in it, so every reconcile sets the
same one. The Secret's UID comes back with it, empty when there's no
Secret:

```go
func (r Reconciler) password(ctx context.Context, namespace, name string) (string, types.UID, error) {
	secret, err := r.Kube.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return rand.Text(), "", nil
	}
	if err != nil {
		return "", "", err
	}
	if password := string(secret.Data["password"]); password != "" {
		return password, secret.UID, nil
	}
	return rand.Text(), secret.UID, nil
}
```

`rand` is `crypto/rand`.

## The Secret

The Secret holds what a client needs to connect, and the `Database` is its
controller, so deleting the `Database` deletes it, and changing it
reconciles the `Database`:

```go
func (r Reconciler) applySecret(ctx context.Context, db database, name string, uid types.UID, password string) error {
	secret := corev1ac.Secret(name, db.Namespace).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(v1alpha1.Database.Group + "/" + v1alpha1.Database.Version).
			WithKind(v1alpha1.Database.Kind).
			WithName(db.Name).
			WithUID(db.UID).
			WithController(true).
			WithBlockOwnerDeletion(true)).
		WithData(map[string][]byte{
			"host":     []byte(r.Host),
			"port":     []byte(strconv.Itoa(r.Port)),
			"database": []byte(db.Spec.Name),
			"username": []byte(db.Spec.Name),
			"password": []byte(password),
		})
	if uid != "" {
		secret.WithUID(uid)
	}
	_, err := r.Kube.CoreV1().Secrets(db.Namespace).Apply(ctx, secret, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
	return err
}
```

It's a server-side apply, forced, so the controller owns the fields it sets.
An apply creates a Secret that doesn't exist, but not one whose UID it
names, so an apply of the Secret that was read fails with a conflict once
that Secret is deleted, rather than creating it again with the old
password.

While the `Database` carries a scenario annotation, so does its Secret: the
Client sets the annotation on a child's server-side apply. Nothing reads it
there.

## The conditions

A failed step sets `Ready` to `False`, with a reason naming the step and a
message that doesn't change from one failure to the next. It returns the
error too, so the `Database` backs off:

```go
func (r Reconciler) failed(db database, st v1alpha1.DatabaseStatus, reason, message string, err error) (kube.Result[v1alpha1.DatabaseStatus], error) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: db.Generation,
	})
	return kube.Result[v1alpha1.DatabaseStatus]{Status: &st}, err
}
```

| `Ready` | Reason | When |
|---|---|---|
| `True` | `Provisioned` | the role, the database and the Secret are provisioned |
| `False` | `PostgresUnavailable` | `Provision` failed |
| `False` | `SecretApplyFailed` | the Secret couldn't be read or applied |

The error, with its detail, goes to the reconcile's span and its
`reconcile failed` record, not to the condition.

## Rotation

Deleting the Secret rotates the password. `Init` has the Controller own
Secrets, so the deletion reconciles the `Database`, which finds no Secret,
makes a new password, sets it on the role, and applies a new Secret. A
reconcile that read the Secret before the deletion rotates it itself, when
its apply fails with a conflict, and the deletion's reconcile then keeps
the new password:

```sh
kubectl delete secret orders-postgres
```

Next: [Cleaning up](/humus/pr-preview/pr-1329/walkthroughs/kube/databases/cleaning-up/)
