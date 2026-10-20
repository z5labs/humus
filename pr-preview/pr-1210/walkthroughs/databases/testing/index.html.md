# Testing

## A unit test of the Reconciler

The `Reconciler` takes a clientset and the `Admin` interface, so its unit
tests give it client-go's fake clientset and a fake `Admin`, which records
what it was given:

```go
type fakeAdmin struct {
	provisions   []provision
	provisionErr error
	databases    map[string]bool
	drops        []string
	dropErr      error
}

func (a *fakeAdmin) Provision(_ context.Context, name, password string) error {
	a.provisions = append(a.provisions, provision{name: name, password: password})
	return a.provisionErr
}

func (a *fakeAdmin) Drop(_ context.Context, name string) error {
	a.drops = append(a.drops, name)
	if a.dropErr != nil {
		return a.dropErr
	}
	delete(a.databases, name)
	return nil
}
```

```go
func newReconciler(kc *kubefake.Clientset, admin *fakeAdmin) controller.Reconciler {
	return controller.Reconciler{Kube: kc, Admin: admin, Host: "postgres.databases.svc", Port: 5432}
}
```

Each test reconciles a `Database`, and checks what the `Admin` was given,
the Secret, and the status:

```go
func TestReconciler_a_new_Database_gets_its_role_database_and_Secret_and_is_Ready(t *testing.T) {
	kc := kubefake.NewClientset()
	admin := &fakeAdmin{}
	db := newDatabase()

	res, err := newReconciler(kc, admin).Reconcile(t.Context(), db)

	require.NoError(t, err)
	require.Len(t, admin.provisions, 1)
	require.Equal(t, "orders_db", admin.provisions[0].name)
	password := admin.provisions[0].password
	require.NotEmpty(t, password)

	s := secret(t, kc)
	require.Equal(t, map[string][]byte{
		"host":     []byte("postgres.databases.svc"),
		"port":     []byte("5432"),
		"database": []byte("orders_db"),
		"username": []byte("orders_db"),
		"password": []byte(password),
	}, s.Data)
	require.Equal(t, []metav1.OwnerReference{{
		APIVersion:         "postgres.example.com/v1alpha1",
		Kind:               "Database",
		Name:               "orders",
		UID:                db.UID,
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}}, s.OwnerReferences)

	require.NotNil(t, res.Status)
	require.Equal(t, "orders-postgres", res.Status.SecretName)
	ready := meta.FindStatusCondition(res.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	require.Equal(t, "Provisioned", ready.Reason)
	require.Equal(t, int64(3), ready.ObservedGeneration)
	require.NotContains(t, ready.Message, password)
	require.Zero(t, res.After)
}
```

A reactor on the fake clientset fails a step, and the test checks the
`Ready` condition's reason:

```go
		{
			name: "the Secret's apply fails",
			setup: func(kc *kubefake.Clientset, _ *fakeAdmin) {
				kc.PrependReactor("patch", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, cause
				})
			},
			reason:  "SecretApplyFailed",
			message: "The Secret could not be applied.",
		},
```

`Finalize` is tested the same way, once for each `deletionPolicy`:

```go
func TestFinalize_Retain_drops_nothing(t *testing.T) {
	admin := &fakeAdmin{databases: map[string]bool{"orders_db": true}}
	db := newDatabase()
	db.Spec.DeletionPolicy = v1alpha1.Retain

	err := newReconciler(kubefake.NewClientset(), admin).Finalize(t.Context(), db)

	require.NoError(t, err)
	require.Empty(t, admin.drops)
	require.Contains(t, admin.databases, "orders_db")
}
```

```sh
go -C example/databases test ./...
```

## The golden CRD

`TestDatabaseCRD` fails when `deploy/crd.yaml` differs from the CRD
reflected from `Database`, as [The Database resource](/humus/pr-preview/pr-1210/walkthroughs/databases/resource/)
shows. After a change to the types, rewrite it:

```sh
go -C example/databases test ./api/v1alpha1/ -update
```

Each rule has a test of its own, against the reflected schema:

```go
func TestDatabase_spec_deletionPolicy_is_Retain_or_Delete_and_defaults_to_Retain(t *testing.T) {
	t.Parallel()

	policy := schema(t).Properties["spec"].Properties["deletionPolicy"]
	require.Equal(t, "string", policy.Type)
	require.Equal(t, []apiextensionsv1.JSON{{Raw: []byte(`"Retain"`)}, {Raw: []byte(`"Delete"`)}}, policy.Enum)
	require.NotNil(t, policy.Default)
	require.JSONEq(t, `"Retain"`, string(policy.Default.Raw))
}
```

The in-process cases create the CRD from the golden file, so they run
against what's deployed:

```go
	b, err := os.ReadFile(filepath.Join("..", "deploy", "crd.yaml"))
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.UnmarshalStrict(b, &crd))
```

## The shared cases

The cases are the package `cases`, which imports neither the App nor
humustest, so the compiler keeps them black-box. A `Target` says how to
reach the cluster the controller reconciles:

```go
type Target struct {
	Config      *rest.Config
	Prefix      string
	Annotation  string
	ScenarioKey string
	Postgres    string
	Timeout     time.Duration
}
```

| Field | What it is |
|---|---|
| `Config` | the client config that reaches the API server |
| `Prefix` | the prefix of the namespaces the cases create, which the controller trusts scenarios in |
| `Annotation`, `ScenarioKey` | the annotation a scenario goes in, and its baggage member |
| `Postgres` | an admin DSN for the PostgreSQL server the controller provisions in |
| `Timeout` | how long a case may take |

```go
type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "Provisioning", Run: Provisioning},
	{Name: "Rotation", Run: Rotation},
	{Name: "Deletion", Run: Deletion},
	{Name: "Rules", Run: Rules},
	{Name: "PostgreSQLDown", Run: PostgreSQLDown},
	{Name: "SecretApplyFailing", Run: SecretApplyFailing},
	{Name: "SlowPostgreSQL", Run: SlowPostgreSQL},
	{Name: "DropFailing", Run: DropFailing},
}
```

| Case | What it shows |
|---|---|
| `Provisioning` | a `Database` becomes `Ready`, and its Secret's credentials log in to a database the role owns |
| `Rotation` | deleting the Secret gets a new password, and the old one is refused |
| `Deletion` | `Delete` drops the role and the database, and `Retain` keeps both |
| `Rules` | the API server refuses an invalid name, and a changed one |

A case creates a `Database`, waits for it to be `Ready`, and logs in with
its Secret's credentials:

```go
func Provisioning(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)
	name := databaseName()

	c.create(ctx, t, "orders", name, deletePolicy)
	c.awaitReady(ctx, t, "orders")

	creds := c.secret(ctx, t, "orders")
	require.Equal(t, name, creds.database)
	require.Equal(t, name, creds.username)
	requireLogsIn(ctx, t, target, creds)
}
```

```go
	var user, owner string
	err = conn.QueryRow(ctx,
		`SELECT current_user, pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()`,
	).Scan(&user, &owner)
	require.NoError(t, err)
	require.Equal(t, creds.username, user)
	require.Equal(t, creds.username, owner, "the owner of the database %s", creds.database)
```

Each case creates a namespace of its own, and databases with names of their
own, so the cases run in parallel:

```go
	ns, err := kc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: target.Prefix},
	}, metav1.CreateOptions{})
```

```go
func databaseName() string {
	return "db_" + strings.ToLower(rand.Text())
}
```

The controller gets there in its own time, so a case polls for what it
expects until its deadline:

```go
func await(ctx context.Context, t testing.TB, what string, done func(ctx context.Context) (bool, error)) {
	t.Helper()
	var last error
	for {
		ok, err := done(ctx)
		if ok {
			return
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			require.FailNowf(t, "timed out", "waiting for %s: %v (last error: %v)", what, context.Cause(ctx), last)
		case <-time.After(poll):
		}
	}
}
```

## The cases, in process

Behind the `testcontainers` tag, the App runs on humustest's Host, against
k3s and PostgreSQL in containers, each with a password generated on each
run:

```go
	c, err := tck3s.Run(t.Context(), k3sImage,
		testcontainers.WithCmdArgs("--disable-agent", "--disable=metrics-server"),
		testcontainers.WithEnv(map[string]string{"K3S_TOKEN": rand.Text()}),
		testcontainers.WithWaitStrategyAndDeadline(startLimit, wait.ForLog("k3s is up and running").WithStartupTimeout(startLimit)),
	)
```

```go
	c, err := tcpostgres.Run(t.Context(), postgresImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("admin"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.BasicWaitStrategies(),
	)
```

The App reaches k3s as a ServiceAccount, with a token the test requests:

```go
	expiry := int64(time.Hour / time.Second)
	tr, err := kc.CoreV1().ServiceAccounts(controllerNamespace).CreateToken(ctx, controllerName, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry},
	}, metav1.CreateOptions{})
```

Its secrets are files, and it trusts scenarios in the cases' namespaces:

```go
	p, status := humustest.Start(t, app.Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(build), humus.WithFaults(app.Faults)),
		humustest.WithEnv(map[string]string{
			"KUBERNETES_SERVICE_HOST":        server.Hostname(),
			"KUBERNETES_SERVICE_PORT":        server.Port(),
			"KUBERNETES_CA_FILE":             writeFile(t, dir, "ca.crt", caData),
			"KUBERNETES_TOKEN_FILE":          writeFile(t, dir, "token", []byte(token+"\n")),
			"POD_NAMESPACE":                  controllerNamespace,
			"POD_NAME":                       controllerName + "-0",
			"POSTGRES_ADMIN_DSN_FILE":        writeFile(t, dir, "admin-dsn", []byte(dsn+"\n")),
			"POSTGRES_HOST":                  pgHost,
			"POSTGRES_PORT":                  pgPort,
			"PROBES_ADDR":                    probesAddr,
			"PROBES_TLS_PEM_FILE":            writeFile(t, dir, "probes.pem", pemOf(t, ca.Issue(t, "127.0.0.1"))),
			"FAULT_TRUSTED_NAMESPACE_PREFIX": casesPrefix,
		}),
	)
```

Once it's ready, the test hands the cases a `Target`:

```go
		target: cases.Target{
			Config:      rest.CopyConfig(cfg),
			Prefix:      casesPrefix,
			Annotation:  "postgres.example.com/failure-scenario",
			ScenarioKey: "acme.fault",
			Postgres:    dsn,
			Timeout:     waitLimit,
		},
```

Every case runs in parallel against one App, which then has to exit 0 on
SIGTERM:

```go
func TestInit_passes_the_shared_cases_against_k3s_and_PostgreSQL(t *testing.T) {
	c := startController(t, telemetry.NoopBuilder)

	t.Run("in parallel", func(t *testing.T) {
		for _, tc := range cases.All {
			t.Run(tc.Name, func(t *testing.T) {
				t.Parallel()
				tc.Run(t, c.target)
			})
		}
		for name, run := range faultCases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				run(t, c.target)
			})
		}
	})

	c.stop(t)
}
```

```sh
go -C example/databases test -tags testcontainers ./...
```

## The cases, on kind

Behind the `functional` tag, the same cases run against the kind overlay
that `up.sh` brings up, which [Deploying](/humus/pr-preview/pr-1210/walkthroughs/databases/deploying/)
covers:

```go
func TestDatabases_passes_the_shared_cases_on_kind(t *testing.T) {
	for _, d := range []string{"postgres", "databases"} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	target := sharedTarget(t)
	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.All {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})
}
```

The `Target` reaches the API server with the kubeconfig's `kind-kind`
context, and PostgreSQL through `kubectl port-forward`:

```go
func sharedTarget(t *testing.T) cases.Target {
	t.Helper()
	cfg := kubeconfig(t)
	return cases.Target{
		Config:      cfg,
		Prefix:      prefix,
		Annotation:  annotation,
		ScenarioKey: scenarioKey,
		Postgres:    adminDSN(t, cfg, forward(t, "postgres", 5432)),
		Timeout:     timeout,
	}
}
```

The admin password is the one `up.sh` put in the `postgres` Secret:

```go
	s, err := kc.CoreV1().Secrets(namespace).Get(t.Context(), "postgres", metav1.GetOptions{})
	require.NoError(t, err, "the secret %s/postgres", namespace)
	password := s.Data["password"]
	require.NotEmpty(t, password, "the secret %s/postgres has no password", namespace)
```

```sh
go -C example/databases test -tags functional ./functional/
```

## The failure injection cases

A case rehearses a failure by putting a scenario in the `Database`'s
annotation:

```go
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)

	db := database(name, dbName, policy)
	db.SetAnnotations(map[string]string{c.target.Annotation: b.String()})
```

```go
func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func delay(point string, d time.Duration) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Delay: d}}
}
```

Removing the annotation ends the rehearsal:

```go
	patch := `{"metadata":{"annotations":{"` + c.target.Annotation + `":null}}}`
```

| Case | Its scenario | What it shows |
|---|---|---|
| `PostgreSQLDown` | aborts `postgresql:*` | `Ready` is `False` with `PostgresUnavailable`, then `True` once the scenario is removed |
| `SecretApplyFailing` | aborts `kube:secrets:patch` | `Ready` is `False` with `SecretApplyFailed`, then `True` once the scenario is removed |
| `SlowPostgreSQL` | delays `postgresql:*` 2s | the `Database` still becomes `Ready` |
| `DropFailing` | aborts `postgresql:drop` | the finalizer holds the deleted `Database`, and the role and the database stay, until the scenario is removed |

`DropFailing`:

```go
func DropFailing(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)
	name := databaseName()

	c.rehearse(ctx, t, "orders", name, deletePolicy, abort("postgresql:drop"))
	c.awaitReady(ctx, t, "orders")
	err := c.dynamic.Resource(databases).Namespace(c.namespace).Delete(ctx, "orders", metav1.DeleteOptions{})
	require.NoError(t, err)

	c.holds(ctx, t, "orders")
	role, database := exists(ctx, t, target, name)
	require.True(t, role, "the role of a Database whose drop fails")
	require.True(t, database, "the database of a Database whose drop fails")

	c.unannotate(ctx, t, "orders")
	c.awaitGone(ctx, t, "orders")
	role, database = exists(ctx, t, target, name)
	require.False(t, role, "the role of a Database whose drop recovered")
	require.False(t, database, "the database of a Database whose drop recovered")
}
```

Two more run in process only, beside the shared cases:

```go
var faultCases = map[string]func(t *testing.T, target cases.Target){
	"an untrusted scenario is stripped":              stripsUntrusted,
	"a trusted scenario is carried on to the Secret": carriesToSecret,
}
```

An untrusted scenario is one in a namespace outside the trusted prefix:

```go
	ns := f.namespace(t, untrustedPrefix)
	value := f.scenario(t, fit.Rule{Point: "postgresql:*", Fault: fit.Fault{Abort: true}})

	f.create(t, ns, "orders", value)

	f.awaitReady(t, ns, "orders", metav1.ConditionTrue, "Provisioned")
	_, carried := f.secretScenario(t, ns, "orders-postgres")
	require.False(t, carried, "the Secret of an untrusted Database carries a scenario")
```
