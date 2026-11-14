# Migrating from kubebuilder

This guide moves a small [kubebuilder](https://book.kubebuilder.io) v4
operator to humus's `kube` module. Its reconciler runs on client-go through
humus, with no controller-runtime. Its process, settings, telemetry, probes,
leader election and shutdown become humus's. An operator built with
operator-sdk's Go plugin, which is built on kubebuilder, moves the same way.

kubebuilder generates the CRD and the RBAC from
[markers](https://book.kubebuilder.io/reference/markers.html) in comments,
with controller-gen. humus declares the custom resource as Go values with typed
constraints, keeps its CRD true to the types with a golden test, and leaves
the RBAC to the end user. The guide's steps go from the kubebuilder operator
to the humus controller, rather than each leaving an operator that runs.

## kubebuilder and humus

| kubebuilder and controller-runtime | humus |
|---|---|
| `ctrl.NewManager` and `mgr.Start` | `humus.Main`, and an `Init` that returns the Service from `kube.NewController`: [A controller](/humus/services/kubernetes/#a-controller) |
| `ctrl.SetupSignalHandler` | none: `humus.Main` stops the App on SIGTERM, and the controller finishes its reconciles in flight within a 20s grace period: [Leader election and shutdown](/humus/services/kubernetes/#leader-election-and-shutdown) |
| `ctrl.GetConfigOrDie`, with `--kubeconfig` and `KUBECONFIG` | a `kube.Connection` the end user's `Init` fills from the pod's service account: [In a pod](/humus/services/kubernetes/#in-a-pod) |
| the flags, `flag.Parse` and `zap.Options` | `kube.Settings` and the end user's own, sourced from the environment: [Settings](/humus/services/kubernetes/#settings) |
| the API types' markers, `+groupName`, `+kubebuilder:validation:Minimum`, `Maximum`, `+listType`, `+listMapKey`, `+optional`, `+required` and `+kubebuilder:subresource:status` | a `kube.Resource`, with typed `kube.Rules`: [Custom resources](/humus/services/kubernetes/#custom-resources) |
| the doc comments controller-gen copies into the CRD | `Description` values: [Rules](/humus/services/kubernetes/#rules) |
| `runtime.Scheme`, `AddToScheme`, `+kubebuilder:object:root=true`, the list type and `zz_generated.deepcopy.go` | none: an object is a plain `kube.Object[Spec, Status]`: [Custom resources](/humus/services/kubernetes/#custom-resources) |
| `make generate` and `make manifests`, with controller-gen | `Resource.CRD()` and a golden test: [The golden test](/humus/services/kubernetes/#the-golden-test) |
| `Reconcile(ctx, ctrl.Request)`, with `r.Get` and not found | a `kube.Reconciler`, handed its object: [Reconciling](/humus/services/kubernetes/#reconciling) |
| `r.Status().Update`, and fetching the object again | the `Result`'s `Status`, applied for it: [Reconciling](/humus/services/kubernetes/#reconciling) |
| `ctrl.Result{RequeueAfter: time.Minute}` | none: the Deployment's changes reconcile the Memcached again, and `Result.After` is there when nothing else would: [An error, After and the resync](/humus/services/kubernetes/#an-error-after-and-the-resync) |
| `Owns(&appsv1.Deployment{})` and `ctrl.SetControllerReference` | `kube.Owns`, and the owner reference the end user applies: [Owned resources](/humus/services/kubernetes/#owned-resources) |
| `r.Create` and `r.Update` of the Deployment | a server-side apply, with client-go's apply configurations: [Owned resources](/humus/services/kubernetes/#owned-resources) |
| `controllerutil.AddFinalizer`, `RemoveFinalizer` and the deletion timestamp | `kube.Finalize`: [Finalizers](/humus/services/kubernetes/#finalizers) |
| `mgr.GetEventRecorder` and `Eventf` | none: the end user creates the `events.k8s.io/v1` Event with the clientset |
| the `Degraded` condition while finalizing | none: a `Finalize`'s function returns no status, and the Event says the Memcached is being deleted |
| `logf.FromContext` and its records | a span for each reconcile, a child span for each API request, and a `reconcile failed` record for each error: [Telemetry](/humus/services/kubernetes/#telemetry) |
| `+kubebuilder:rbac` markers and `config/rbac/role.yaml` | the RBAC the end user writes: [RBAC](/humus/services/kubernetes/#rbac) |
| `LeaderElection`, `LeaderElectionID` and `--leader-elect` | always on, on a Lease named `Settings.Name`: [Leader election and shutdown](/humus/services/kubernetes/#leader-election-and-shutdown) |
| the controller's `Named` | `Settings.Name`, which names the Lease and the status's field manager too: [Settings](/humus/services/kubernetes/#settings) |
| `HealthProbeBindAddress`, and `healthz.Ping` on `/healthz` and `/readyz` | `httphealth.NewService`, ready once the controller's caches have synced: [Readiness](/humus/services/kubernetes/#readiness) |
| the metrics server, `filters.WithAuthenticationAndAuthorization` and controller-runtime's Prometheus metrics | the controller's own instruments, exported with OTLP: [The controller's instruments](/humus/services/kubernetes/#the-controllers-instruments) |

## The kubebuilder operator

The guide's code is under
[`example/kube/kubebuilder`](https://github.com/z5labs/humus/tree/main/example/kube/kubebuilder),
a module for each operator:

```text
example/kube/kubebuilder/
├── cases/    the black-box test cases both operators pass
├── start/    the kubebuilder operator the guide starts from
└── humus/    the humus controller it ends with
```

The operator is the Memcached operator of
[kubebuilder's tutorial](https://book.kubebuilder.io/getting-started.html), with
the finalizer and the Event of its
[deploy-image plugin](https://book.kubebuilder.io/plugins/available/deploy-image-plugin-v1-alpha.html).
A Memcached says how many memcached pods run. Its controller creates a
Deployment of them, keeps the Deployment's size, and reports it in the
Memcached's `Available` condition. Its types carry markers:

```go
type MemcachedSpec struct {
	// size defines the number of Memcached instances
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	// +optional
	Size *int32 `json:"size,omitempty"`
}

type MemcachedStatus struct {
	// conditions represent the current state of the Memcached resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

`make generate` writes `zz_generated.deepcopy.go`, and `make manifests`
writes the CRD to `config/crd/bases/` and the RBAC to
`config/rbac/role.yaml`, both with controller-gen.

`Reconcile` is handed a name, so it fetches the Memcached first:

```go
memcached := &cachev1alpha1.Memcached{}
err := r.Get(ctx, req.NamespacedName, memcached)
if err != nil {
	if apierrors.IsNotFound(err) {
		log.Info("Memcached resource not found, ignoring since object must be deleted")
		return ctrl.Result{}, nil
	}
	log.Error(err, "Failed to get memcached")
	return ctrl.Result{}, err
}
```

It sets the condition `Unknown` and fetches the Memcached again, adds the
finalizer with an update, and creates the Deployment, or updates its size,
and requeues after a minute. Once the Deployment has the size, it updates
the status:

```go
meta.SetStatusCondition(&memcached.Status.Conditions, metav1.Condition{Type: typeAvailableMemcached,
	Status: metav1.ConditionTrue, Reason: reasonReconciling,
	Message: fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", memcached.Name, desiredReplicas)})

if err := r.Status().Update(ctx, memcached); err != nil {
	log.Error(err, "Failed to update Memcached status")
	return ctrl.Result{}, err
}
```

Once the Memcached is being deleted, it sets a `Degraded` condition, records
an Event, sets the condition again and removes the finalizer:

```go
func (r *MemcachedReconciler) doFinalizerOperationsForMemcached(cr *cachev1alpha1.Memcached) {
	r.Recorder.Eventf(cr, nil, corev1.EventTypeWarning, "Deleting", "DeleteCR",
		"Custom Resource %s is being deleted from the namespace %s",
		cr.Name,
		cr.Namespace)
}
```

It watches the Memcacheds and the Deployments they own:

```go
func (r *MemcachedReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.Memcached{}).
		Named("memcached").
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
```

`main` parses the flags kubebuilder scaffolds, and starts a Manager with its
metrics, its probes and leader election:

```go
mgr, err := newManager(ctrl.GetConfigOrDie(), ctrl.Options{
	Scheme:                 scheme,
	Metrics:                metricsServerOptions,
	HealthProbeBindAddress: probeAddr,
	LeaderElection:         enableLeaderElection,
	LeaderElectionID:       "4b13e8ac.example.com",
})
if err != nil {
	setupLog.Error(err, "Failed to start manager")
	os.Exit(1)
}

setupLog.Info("Starting manager")
if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
	setupLog.Error(err, "Failed to run manager")
	os.Exit(1)
}
```

`newManager` builds the Manager, sets up the reconciler with its event
recorder, and adds the `healthz.Ping` checks, so the test can start it on
k3s.

## The test cases

The `cases` module holds black-box cases, written once. Each creates a
namespace of its own, and Memcacheds in it with the dynamic client, as a
user of the CRD would, then waits for what the operator does:

```go
type Target struct {
	Config  *rest.Config
	Prefix  string
	Timeout time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "Deployment", Run: Deployment},
	{Name: "Resize", Run: Resize},
	{Name: "ScaledByHand", Run: ScaledByHand},
	{Name: "DeploymentDeleted", Run: DeploymentDeleted},
	{Name: "Validation", Run: Validation},
	{Name: "Deletion", Run: Deletion},
}
```

One scales the Deployment by hand, and waits for the operator to put the
Memcached's size back:

```go
func ScaledByHand(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)

	c.create(ctx, t, "cache", 2)
	c.awaitAvailable(ctx, t, "cache", 2)

	deployments := c.kube.AppsV1().Deployments(c.namespace)
	scale, err := deployments.GetScale(ctx, "cache", metav1.GetOptions{})
	require.NoError(t, err)
	scale.Spec.Replicas = 1
	_, err = deployments.UpdateScale(ctx, "cache", &autoscalingv1.Scale{ObjectMeta: scale.ObjectMeta, Spec: scale.Spec}, metav1.UpdateOptions{})
	require.NoError(t, err)

	c.awaitReplicas(ctx, t, "cache", 2)
}
```

`Deletion` deletes a Memcached, and waits for the Event, for the Memcached
to be gone and for its Deployment to be garbage collected.

Each operator's test starts k3s in a container, creates the CRD from its
operator's `config/crd/bases/`, and runs the operator as a ServiceAccount
bound to just the RBAC in its `config/rbac/`. The kubebuilder operator's
test then runs its Manager, and the cases in parallel:

```go
mgr, err := newManager(cfg, ctrl.Options{
	Scheme:                  scheme,
	Metrics:                 metricsserver.Options{BindAddress: "0"},
	HealthProbeBindAddress:  "0",
	LeaderElection:          true,
	LeaderElectionID:        "4b13e8ac.example.com",
	LeaderElectionNamespace: controllerNamespace,
})
```

The tests start containers, so they sit behind the `testcontainers` build
tag:

```sh
go test -tags testcontainers ./...
```

## Step 1: main and its settings

`main` becomes one call to `humus.Main`:

```go
humus.Main(Init, humus.WithTelemetry(buildTelemetry))
```

`Init` builds the `Client`, the controller and its probes:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	client, err := kube.NewClient(s, settings.Connection)
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(client.Config())
	if err != nil {
		return nil, err
	}

	r := controller.Reconciler{Kube: kc, Instance: settings.Controller.LeaderElection.Identity}
	ctrl, err := kube.NewController(s, client, settings.Controller, v1alpha1.Memcached, r,
		kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments")),
		kube.Finalize(memcachedFinalizer, r.Finalize),
	)
	if err != nil {
		return nil, err
	}
	probes, err := httphealth.NewService(s, settings.Probes, health.Readiness(ctrl.Synced()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{ctrl, probes}, nil
}
```

`readSettings` sources the controller, the API server and the probes from
the environment:

```go
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	return config.Struct(func(r *config.Resolver) settings {
		k := kube.DefaultSettings()
		k.Name = r.Get(env("CONTROLLER_NAME").Validate(kube.CheckName))
		k.LeaderElection.Namespace = r.Get(env("POD_NAMESPACE").Validate(kube.CheckNamespace))
		k.LeaderElection.Identity = r.Get(env("POD_NAME").Validate(kube.CheckIdentity))

		tokenFile := r.Get(env("KUBERNETES_TOKEN_FILE").Default(serviceAccount + "token"))
		return settings{
			Connection: kube.Connection{
				Server: net.JoinHostPort(r.Get(env("KUBERNETES_SERVICE_HOST")), r.Get(env("KUBERNETES_SERVICE_PORT"))),
				TLS:    r.Get(env("KUBERNETES_CA_FILE").Default(serviceAccount + "ca.crt").Parse(trusting(s.Host.FS)).Validate(kube.CheckTLS)),
				Token: func(context.Context) (string, error) {
					b, err := readFile(s.Host.FS, tokenFile)
					return string(bytes.TrimSpace(b)), err
				},
			},
			Controller: k,
			Probes: httphealth.Settings{
				Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
				TLS: r.Get(env("PROBES_TLS_CERT_FILE").FlatMap(func(certFile string) config.Value[*tls.Config] {
					return env("PROBES_TLS_KEY_FILE").Parse(serverTLS(s.Host.FS, certFile))
				}).Validate(httphealth.CheckTLS)),
			},
		}
	}).Read(ctx)
}
```

The flags become variables:

| kubebuilder | humus |
|---|---|
| `--kubeconfig`, `KUBECONFIG` or the pod's service account | `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT`, which the kubelet sets, and the service account's CA and token, which `KUBERNETES_CA_FILE` and `KUBERNETES_TOKEN_FILE` move |
| `--leader-elect` | none: every controller elects a leader |
| `LeaderElectionID` | `CONTROLLER_NAME` |
| the namespace controller-runtime reads from the service account | `POD_NAMESPACE`, from the downward API |
| the Lease's identity, the hostname and a UUID | `POD_NAME`, from the downward API |
| `--health-probe-bind-address` | `PROBES_ADDR`, with `PROBES_TLS_CERT_FILE` and `PROBES_TLS_KEY_FILE` |
| `--metrics-bind-address` and `--metrics-secure` | `OTEL_METRICS_EXPORTER` and `OTEL_EXPORTER_OTLP_ENDPOINT` |
| `--zap-devel`, `--zap-log-level` and zap's other flags | none: `OTEL_LOGS_EXPORTER` sends every record to stdout, as JSON, or with OTLP |

From here on, these are humus's:

- **The process and its shutdown.** On SIGTERM the controller stops taking
  Memcacheds, finishes the reconciles in flight, releases the Lease and
  exits 0, within a 20s grace period, as
  [Leader election and shutdown](/humus/services/kubernetes/#leader-election-and-shutdown)
  describes. The Manager left its Lease to expire.
- **Leader election.** It is always on, on a Lease named `CONTROLLER_NAME`
  in `POD_NAMESPACE`. `CONTROLLER_NAME` is the operator's
  `LeaderElectionID`, so during the rollout the humus pods wait for the
  operator's Lease, and the two never reconcile at once.
- **The settings.** A missing or bad one makes the controller exit 78,
  naming its variable.
- **Telemetry.** `buildTelemetry` names the service `memcached-operator`, as
  [Writing the Builder](/humus/fundamentals/telemetry/#writing-the-builder)
  shows. Each reconcile is a span, each API request a child span, and the
  controller's instruments take the place of controller-runtime's Prometheus
  metrics.
- **The probes.** The kubelet probes `/health/liveness` and
  `/health/readiness` on `PROBES_ADDR`, with `scheme: HTTPS`. The pod is
  ready once the controller's caches have synced.

## Step 2: the custom resource

The types keep their fields and lose their markers. The constraints and the
descriptions become `kube.Rules`:

```go
type MemcachedSpec struct {
	Size int32 `json:"size,omitempty"`
}

type MemcachedStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

var (
	spec   MemcachedSpec
	status MemcachedStatus

	Memcached = kube.Resource[MemcachedSpec, MemcachedStatus]{
		Group:   "cache.example.com",
		Version: "v1alpha1",
		Kind:    "Memcached",
		Plural:  "memcacheds",
		Scope:   kube.Namespaced,
		Rules: []kube.TypeRules{
			kube.Rules(&spec,
				kube.Field(&spec.Size).Minimum(1).Maximum(3).Description("size defines the number of Memcached instances"),
			).Description("spec defines the desired state of Memcached"),
			kube.Rules(&status,
				kube.Field(&status.Conditions).ListMap("type").Description("conditions represent the current state of the Memcached resource."),
			).Description("status defines the observed state of Memcached"),
		},
	}
)
```

`Size` is an `int32` with `omitempty` rather than a pointer: either is an
optional integer in the schema, and a Memcached without one ends with a
Deployment of no pods. The `Memcached` and `MemcachedList` structs,
`groupversion_info.go` and `zz_generated.deepcopy.go` are gone, and with
them the scheme.

A golden test takes the place of `make manifests`. It writes the CRD where
controller-gen wrote it, so whatever applies the CRD keeps applying it:

```go
func TestMemcachedCRD(t *testing.T) {
	crd, err := Memcached.CRD()
	require.NoError(t, err)
	got, err := yaml.Marshal(crd)
	require.NoError(t, err)

	const path = "../../config/crd/bases/cache.example.com_memcacheds.yaml"
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}
```

```sh
go test ./api/... -update
```

The CRD it writes differs from the one controller-gen generated:

- **Descriptions.** The spec, the status, `size` and `conditions` keep
  theirs. The object, `apiVersion` and `kind` have none, so
  `kubectl explain` shows less.
- **Each condition.** controller-gen copies apimachinery's markers on
  `metav1.Condition`: a description of each field, an enum of `status`, a
  pattern and a length for `type` and `reason`, a length for `message` and
  a minimum for `observedGeneration`. humus reflects the Go type, so each
  field has its type, the same five are required, and none is constrained.
  Only the controller writes conditions, with `meta.SetStatusCondition`.
- **The rest.** The `controller-gen.kubebuilder.io/version` annotation is
  gone, and the file carries an empty `status`, which the API server
  ignores.

Both reject a size of 0 or 4, as the `Validation` case checks.

## Step 3: the reconciler

The `Reconciler` is handed the Memcached. It applies the Deployment, and
returns the condition the operator updated:

```go
func (r Reconciler) Reconcile(ctx context.Context, m kube.Object[v1alpha1.MemcachedSpec, v1alpha1.MemcachedStatus]) (kube.Result[v1alpha1.MemcachedStatus], error) {
	st := m.Status
	_, err := r.Kube.AppsV1().Deployments(m.Namespace).Apply(ctx, deploymentForMemcached(m), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
	if err != nil {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typeAvailableMemcached,
			Status: metav1.ConditionFalse, Reason: reasonReconciling,
			Message: fmt.Sprintf("Failed to apply the Deployment for the custom resource (%s): (%s)", m.Name, err)})
		return kube.Result[v1alpha1.MemcachedStatus]{Status: &st}, err
	}

	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typeAvailableMemcached,
		Status: metav1.ConditionTrue, Reason: reasonReconciling,
		Message: fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", m.Name, m.Spec.Size)})
	return kube.Result[v1alpha1.MemcachedStatus]{Status: &st}, nil
}
```

There is no `Get`, no not found, no first `Unknown` condition, no
`Status().Update`, no fetching again and no requeue. A change to the
Deployment reconciles the Memcached through `kube.Owns`, as `Owns` did.

The Deployment is the operator's, as client-go's apply configurations, with
the owner reference `ctrl.SetControllerReference` set:

```go
func deploymentForMemcached(m kube.Object[v1alpha1.MemcachedSpec, v1alpha1.MemcachedStatus]) *appsv1ac.DeploymentApplyConfiguration {
	ls := labelsForMemcached(m.Name)
	return appsv1ac.Deployment(m.Name, m.Namespace).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(m.APIVersion).WithKind(m.Kind).WithName(m.Name).WithUID(m.UID).
			WithController(true).WithBlockOwnerDeletion(true)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(m.Spec.Size).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(ls)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(ls).
				WithSpec(corev1ac.PodSpec().WithContainers(corev1ac.Container().
					WithImage("memcached:1.6.26-alpine3.19").
					WithName(memcachedContainerName).
					WithPorts(corev1ac.ContainerPort().WithContainerPort(11211).WithName(memcachedContainerName)).
					WithCommand("memcached", "--memory-limit=64", "-o", "modern", "-v")))))
}
```

Its selector is the operator's, and a Deployment's selector can't change, so
the controller takes over the Deployments the operator created as they are.
The apply is forced, so it puts back every field it declares, where the
operator compared only the replicas.

`kube.Finalize` adds the finalizer before the first reconcile, calls its
function once the Memcached is being deleted, and removes the finalizer once
the function returns nil. humus has no event recorder, so the function
creates the Event with the clientset, named as client-go names one:

```go
func (r Reconciler) Finalize(ctx context.Context, m kube.Object[v1alpha1.MemcachedSpec, v1alpha1.MemcachedStatus]) error {
	now := time.Now()
	_, err := r.Kube.EventsV1().Events(m.Namespace).Create(ctx, &eventsv1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: fmt.Sprintf("%s.%x", m.Name, now.UnixNano())},
		EventTime:           metav1.NewMicroTime(now),
		ReportingController: controllerName,
		ReportingInstance:   r.Instance,
		Action:              "DeleteCR",
		Reason:              "Deleting",
		Type:                corev1.EventTypeWarning,
		Regarding: corev1.ObjectReference{
			APIVersion: m.APIVersion, Kind: m.Kind, Name: m.Name, Namespace: m.Namespace,
			UID: m.UID, ResourceVersion: m.ResourceVersion,
		},
		Note: fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s", m.Name, m.Namespace),
	}, metav1.CreateOptions{})
	return err
}
```

An error retries the function, so a retry may record the Event twice. A
`Finalize`'s function returns no status, so the `Degraded` condition is
gone.

## Step 4: RBAC

humus generates no RBAC. The markers that generated `config/rbac/role.yaml`:

```go
// +kubebuilder:rbac:groups=cache.example.com,resources=memcacheds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cache.example.com,resources=memcacheds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cache.example.com,resources=memcacheds/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
```

become the end user's own `config/rbac/role.yaml`, under the ClusterRole's
name, so its binding still binds it:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules:
- apiGroups:
  - cache.example.com
  resources:
  - memcacheds
  verbs:
  - get
  - list
  - watch
  - patch
- apiGroups:
  - cache.example.com
  resources:
  - memcacheds/status
  verbs:
  - patch
- apiGroups:
  - cache.example.com
  resources:
  - memcacheds/finalizers
  verbs:
  - update
- apiGroups:
  - apps
  resources:
  - deployments
  verbs:
  - list
  - watch
  - patch
  - create
- apiGroups:
  - events.k8s.io
  resources:
  - events
  verbs:
  - create
```

| The marker's verbs | humus's |
|---|---|
| memcacheds: get, list, watch, create, update, patch, delete | get, list and watch, and patch, which applies the finalizer |
| memcacheds/status: get, update, patch | patch, which applies the status |
| memcacheds/finalizers: update | the same: the Deployment's owner reference blocks the Memcached's deletion, which a cluster with the `OwnerReferencesPermissionEnforcement` admission plugin checks |
| deployments: get, list, watch, create, update, patch, delete | list and watch for `Owns`, and patch and create for the apply |
| events: create, patch | create |
| pods: get, list, watch | none: nothing reads pods |

The leader election Role kubebuilder scaffolds keeps only the Lease's verbs:

```yaml
- apiGroups:
  - coordination.k8s.io
  resources:
  - leases
  verbs:
  - get
  - create
  - update
```

## Step 5: the pod

`config/manager/manager.yaml` sets variables in place of the flags. The
Lease's namespace and identity come from the downward API:

```yaml
env:
- name: CONTROLLER_NAME
  value: 4b13e8ac.example.com
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: PROBES_ADDR
  value: :8081
- name: PROBES_TLS_CERT_FILE
  value: /etc/manager/probes/tls.crt
- name: PROBES_TLS_KEY_FILE
  value: /etc/manager/probes/tls.key
```

The probes move to humus's paths, over HTTPS:

```yaml
livenessProbe:
  httpGet:
    path: /health/liveness
    port: probes
    scheme: HTTPS
  initialDelaySeconds: 15
  periodSeconds: 20
readinessProbe:
  httpGet:
    path: /health/readiness
    port: probes
    scheme: HTTPS
  initialDelaySeconds: 5
  periodSeconds: 10
```

The probes' certificate is the Secret `controller-manager-probes`, of the
end user's own: the kubelet doesn't verify it. `terminationGracePeriodSeconds`
goes from kubebuilder's 10 to 30, which holds humus's 20s grace period. The
metrics port, and the Service and the ServiceMonitor kubebuilder scaffolds
for it, are gone: the metrics leave with OTLP.

## Step 6: test it

The cases run against the whole humus controller, with `humustest.Start`
and `humus.OSHost()`, against k3s:

```go
p, status := humustest.Start(t, Init,
	humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder)),
	humustest.WithEnv(map[string]string{
		"KUBERNETES_SERVICE_HOST": server.Hostname(),
		"KUBERNETES_SERVICE_PORT": server.Port(),
		"KUBERNETES_CA_FILE":      writeFile(t, dir, "ca.crt", caData),
		"KUBERNETES_TOKEN_FILE":   writeFile(t, dir, "token", []byte(token+"\n")),
		"CONTROLLER_NAME":         controllerName,
		"POD_NAMESPACE":           controllerNamespace,
		"POD_NAME":                "controller-manager-0",
		"PROBES_ADDR":             probesAddr,
		"PROBES_TLS_CERT_FILE":    writeFile(t, dir, "tls.crt", certPEM),
		"PROBES_TLS_KEY_FILE":     writeFile(t, dir, "tls.key", keyPEM),
	}),
)
```

Once the cases pass, the test checks that the controller holds the Lease,
sends SIGTERM, and checks that it exits 0 and releases the Lease. The
`Reconciler` is tested as a function, on client-go's fakes, and `Init`'s
settings inside `synctest`, as
[Testing](/humus/services/kubernetes/#testing) describes. In
place of envtest's `kube-apiserver` and `etcd`, the whole controller runs
against k3s. The module's
[`go.mod`](https://github.com/z5labs/humus/blob/main/example/kube/kubebuilder/humus/go.mod)
requires no controller-runtime.

What clients see change:

- **The status** never shows the `Available` condition `Unknown` before the
  first reconcile, nor a `Degraded` condition while the Memcached is being
  deleted.
- **A hand-made change to the Deployment** is undone for every field the
  controller declares, not only the replicas.
- **The CRD** has fewer descriptions, and no constraints on its conditions.
- **The `Deleting` Event** names the pod as its `reportingInstance`, where
  client-go named the controller and the host.
- **The probes** answer on `/health/liveness` and `/health/readiness`, over
  HTTPS.
- **The metrics** leave with OTLP, rather than waiting for Prometheus on
  `:8443`.
