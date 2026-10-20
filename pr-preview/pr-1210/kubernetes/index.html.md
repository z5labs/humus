# Writing a Kubernetes controller

The [`kube`](https://pkg.go.dev/github.com/z5labs/humus/kube) package writes
Kubernetes controllers as `humus.Service`s, with
[client-go](https://github.com/kubernetes/client-go), Kubernetes' own client,
and no controller-runtime. It reaches the API server through the Setup's
Host, declares custom resources as Go values, reflects their CRDs from Go
types, and reconciles their objects, or those of Kubernetes' own kinds, on
client-go's informers, work queue and leader election. The end user's
`Init` fills a `kube.Connection` and sources `kube.Settings`, builds a
`Client` and a `Reconciler`, and returns the Service that
`kube.NewController` builds to `humus.Main`, whose defaults fit a
controller.

`kube` is in the nested module `github.com/z5labs/humus/kube`, so client-go
and apimachinery stay out of the module graph of a service that doesn't talk
to Kubernetes. It generates no code, reads no comment markers, generates no
RBAC and ships no test package. The design is in
[Proposal: Kubernetes controllers](https://github.com/z5labs/humus/discussions/851),
with its revisions
[1](https://github.com/z5labs/humus/discussions/851#discussioncomment-19012629),
[3](https://github.com/z5labs/humus/discussions/851#discussioncomment-19020329),
[4](https://github.com/z5labs/humus/discussions/851#discussioncomment-19021637)
and [5](https://github.com/z5labs/humus/discussions/851#discussioncomment-19021697).

The examples on this page are the kubebuilder book's Memcached operator: a
`Memcached` custom resource, whose controller applies a Deployment of
memcached pods and reports how many are ready.

The [databases controller walkthrough](/humus/pr-preview/pr-1210/walkthroughs/databases/)
builds a production-ready controller on `kube`, step by step: a `Database`
resource whose controller provisions a database and its role in PostgreSQL,
owns a Secret of their credentials, and drops them with a finalizer.

## Why not kubebuilder

Nearly every Go controller is written with kubebuilder: controller-runtime at
run time, and controller-gen for the code and YAML it generates from comment
markers. humus neither wraps nor depends on either, for two reasons, which
[the proposal's Background](https://github.com/z5labs/humus/discussions/851)
details with links into their source.

**controller-runtime breaks humus's principles, some of them just by being
imported.** Its `init` functions fill a package-level Prometheus registry
and set client-go's global metrics providers. It logs through a global
logger, which prints a warning and a stack trace to the real stderr if no
logger is set within 30 seconds. Its config package adds `--kubeconfig` to
`flag.CommandLine`, and reads `KUBECONFIG` and the home directory from the
real process. `SetupSignalHandler` calls `signal.Notify` and `os.Exit`. Its
probe and metrics servers listen with `net.Listen`, the metrics server on
`:8080` by default. A Manager is the cache, the clients, the scheme, leader
election and four servers in one type, with 28 options, and envtest runs
real `kube-apiserver` and `etcd` binaries it finds through
`KUBEBUILDER_ASSETS`. Its telemetry is Prometheus only. A wrapper can't
avoid any of that: the globals are set by `init` functions.

**Its developer experience is cumbersome.** Markers are comments, so
controller-gen silently skips one it doesn't know:
`// +kubebuilder:validation:Minimun=1` produces a CRD with no minimum.
`make generate` and `make manifests` must be run after every change to the
types. A Reconcile is handed a name, so it starts by fetching its object and
handling not-found, and writing its status can fail with "the object has
been modified", after which it fetches the object again. There are three
ways to requeue, one deprecated. Reads go to a cache that a first read of a
type starts, watching every namespace, and every type must be registered in
a scheme.

`kube` is client-go's own parts with none of that. depguard denies
`sigs.k8s.io/controller-runtime` and `sigs.k8s.io/controller-tools` in every
humus module, so neither comes back as a convenience. Admission and
conversion webhooks need nothing from `kube`: they are HTTPS POSTs of JSON,
which an [OpenAPI](/humus/pr-preview/pr-1210/openapi/) API on
[`httpserver`](/humus/pr-preview/pr-1210/http/) serves.

## A controller

The end user's `Init` sources the `Settings` and the `Connection`, builds the
`Client`, a clientset on it and the `Reconciler`, and returns the
`Controller`:

```go
// Init is the end user's Init: it sources the Settings and the Connection,
// builds the Client, a clientset on it and the Reconciler, and returns the
// Controller, which reconciles Memcacheds and the Deployments they own.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}
	conn, err := inPod(ctx, s, serviceAccount)
	if err != nil {
		return nil, err
	}

	client, err := kube.NewClient(s, conn) // opens no connection: Serve does
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(client.Config())
	if err != nil {
		return nil, err
	}
	ctrl, err := kube.NewController(s, client, settings, Memcached, Reconciler{Kube: kc},
		kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments")),
	)
	if err != nil {
		return nil, err
	}
	return []humus.Service{ctrl}, nil
}
```

`kubernetes` is client-go's clientset package,
`k8s.io/client-go/kubernetes`, which is why humus's package is named `kube`.
`readSettings` is under [Settings](#settings), `inPod` under
[Connecting](#connecting), `Memcached` under
[Custom resources](#custom-resources), and the `Reconciler` under
[Reconciling](#reconciling). The controller's `main` is one call to
`humus.Main`, here with a telemetry `Builder` that names the service, as
[Telemetry](/humus/pr-preview/pr-1210/telemetry/#writing-the-builder) describes:

```go
humus.Main(Init, humus.WithTelemetry(buildTelemetry))
```

**`humus.Main`'s defaults fit a controller,** so `kube` has no `Main` of its
own: nothing routes traffic to a controller, so it needs no drain, and its
reconciles in flight finish within the 20s grace period, as
[Leader election and shutdown](#leader-election-and-shutdown) describes.

**`NewClient` and `NewController` open no connection.** Each checks what it
is given and reports every mistake in one error, so `Init` fails before
anything is dialled. `Serve` lists, watches and campaigns for the Lease, as
[What Serve does](#what-serve-does) describes.

## Connecting

A `kube.Client` reaches one API server through the App's Host, from a
`kube.Connection` the end user's `Init` fills: the API server's `Server`,
`host:port`, the `TLS` it is dialled with, and a `Token` function that
returns the bearer token every request carries. humus reads no
`KUBERNETES_SERVICE_HOST`, no token file and no kubeconfig itself, so a test
fills the Connection with whatever its cluster needs, and nothing reads the
real environment or disk behind the end user's back. The package reads no
environment variable, reads no file, and defines no variable's name.

### In a pod

The kubelet sets `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT` in
every container, and mounts the service account's CA and token in
`/var/run/secrets/kubernetes.io/serviceaccount/`. The end user's `inPod`
sources `Server` from the two variables, and reads the CA and the token
through `s.Host.FS`:

```go
// serviceAccount is where the kubelet mounts the pod's service account: its
// token, and the cluster's CA.
const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount/"
```

```go
// inPod fills the Connection of a pod from what the kubelet gives every
// container: the API server's address in two variables, and the service
// account's CA and token in dir, read through s.Host.FS.
func inPod(ctx context.Context, s *humus.Setup, dir string) (kube.Connection, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	ca, err := readFile(s.Host.FS, dir+"ca.crt")
	if err != nil {
		return kube.Connection{}, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return kube.Connection{}, errors.New("the service account's ca.crt holds no PEM certificate")
	}

	return config.Struct(func(r *config.Resolver) kube.Connection {
		return kube.Connection{
			// The kubelet sets both in every container. JoinHostPort brackets an IPv6 host.
			Server: net.JoinHostPort(r.Get(env("KUBERNETES_SERVICE_HOST")), r.Get(env("KUBERNETES_SERVICE_PORT"))),
			TLS:    &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			// Read again at most once a minute, so a token the kubelet rotates is picked up.
			Token: func(context.Context) (string, error) {
				b, err := readFile(s.Host.FS, dir+"token")
				return string(bytes.TrimSpace(b)), err
			},
		}
	}).Read(ctx)
}
```

`readFile` is the end user's helper, which reads a whole file through a
`humus.FS`:

```go
// readFile reads the whole file name through fsys.
func readFile(fsys humus.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}
```

`inPod` takes the directory, rather than naming it, so its test puts the CA
and the token in a humustest Host's directory and passes
`h.Path("serviceaccount")`.

**The `Client` calls `Token` at most once a minute,** however many requests
it sends, so a token the kubelet rotates is picked up within a minute. When a call fails, the Client keeps the last token, which a
rotated service account token outlives, and the request goes on. Until a
call has returned a token there is none to keep, so each request calls
`Token`, and fails with its error. A call that returns an empty token has
failed, so no request carries a bare `Bearer`. A nil `Token` sends no
`Authorization` header, for a client that authenticates with a certificate
in `Connection.TLS`.

**`TLS.ServerName` defaults to `Server`'s host.** In a pod that is the
Service's cluster IP, which the API server's certificate names. Set it when
it doesn't, as when `Server` is a load balancer in front of the API server.

### From a kubeconfig, for local development

On a laptop, the end user fills the Connection from a kubeconfig's embedded
data, as kind and k3s write it, with client-go's `clientcmd.Load`. The end
user reads the file, through the Host's FS:

```go
// fromKubeconfig fills a Connection from the kubeconfig at path, for local
// development: the API server, CA and client certificate of its current
// context, embedded in the file, as kind and k3s write them. It reads the
// file through fsys.
func fromKubeconfig(fsys humus.FS, path string) (kube.Connection, error) {
	b, err := readFile(fsys, path)
	if err != nil {
		return kube.Connection{}, err
	}
	kc, err := clientcmd.Load(b)
	if err != nil {
		return kube.Connection{}, err
	}
	current, ok := kc.Contexts[kc.CurrentContext]
	if !ok {
		return kube.Connection{}, fmt.Errorf("the kubeconfig has no context %q", kc.CurrentContext)
	}
	cluster, user := kc.Clusters[current.Cluster], kc.AuthInfos[current.AuthInfo]
	if cluster == nil || user == nil {
		return kube.Connection{}, errors.New("the kubeconfig's current context names no cluster or no user")
	}
	u, err := url.Parse(cluster.Server)
	if err != nil {
		return kube.Connection{}, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cluster.CertificateAuthorityData) {
		return kube.Connection{}, errors.New("the kubeconfig's cluster embeds no CA")
	}
	cert, err := tls.X509KeyPair(user.ClientCertificateData, user.ClientKeyData)
	if err != nil {
		return kube.Connection{}, err
	}
	return kube.Connection{
		Server: u.Host, // host:port
		TLS:    &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}, nil
}
```

`clientcmd.Load` parses the file, and reads nothing else: not `KUBECONFIG`,
not the home directory, and none of the files a kubeconfig can name in place
of embedded data. The path is the end user's own setting, sourced in `Init`
like any other.

**humus has no helper for this.** A kubeconfig's exec plugins and auth
providers grow with every cloud, and a Connection is three fields. A cloud's
token comes from the cloud's own SDK, in a `Token` function of the end
user's. `kube.CheckServer` and `kube.CheckTLS` check the two required
fields, and `NewClient` reports every bad field together.

### `Config()`, for client-go's clientsets

`Client.Config()` returns a `rest.Config` for client-go's own clients: the
clientset `kubernetes.NewForConfig` returns, as `Init` builds, and the
dynamic and metadata clients. Every one of them reaches the API server
through the Client's transport, which:

- dials with `s.Host.Dial`;
- runs TLS with a clone of `Connection.TLS`;
- uses no proxy, whatever the environment says;
- over HTTP/2, sends a ping after 30s without a frame, and gives up on the
  connection when 15s pass without the answer, so a watch on a dead
  connection doesn't wait for TCP to give up;
- adds the bearer token to every request, when `Token` isn't nil;
- traces and measures every request with `otelhttp`, on `s.Telemetry`'s
  providers and propagator, except a watch, which lasts as long as an
  informer runs and has no span;
- doesn't limit the client's rate. client-go's default is 5 requests a
  second, which throttles a busy controller, and the API server's priority
  and fairness limits each client by its identity itself.

Each call returns a new `rest.Config`, so changing one changes no other. The
Client closes its idle connections at `OnStop`. Dialling through the Host is
what lets a test put the API server on a network of its own, with no
test-only seam, and building its own transport keeps client-go's
`DISABLE_HTTP2` and `HTTP2_*` variables, and `http.ProxyFromEnvironment`,
out of the process.

## Custom resources

A `kube.Resource` declares a custom resource as Go values: its group,
version, kind, plural and scope, with its spec and status as plain Go
structs. The end user writes no markers and generates no code:

```go
type MemcachedSpec struct {
	Size  int32  `json:"size"`
	Image string `json:"image"`
}

type MemcachedStatus struct {
	ReadyReplicas int32              `json:"readyReplicas,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
}
```

```go
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
				kube.Field(&spec.Size).Minimum(1).Maximum(3).Description("How many memcached pods run."),
				kube.Field(&spec.Image).Pattern(regexp.MustCompile(`^memcached:`)).Default("memcached:1.6.26-alpine3.19"),
			),
			kube.Rules(&status,
				kube.Field(&status.Conditions).ListMap("type"),
			),
		},
	}
)
```

One of its objects is a `kube.Object[MemcachedSpec, MemcachedStatus]`: a
plain struct of `metav1.TypeMeta`, `metav1.ObjectMeta`, `Spec` and `Status`,
with no `DeepCopy`, no scheme and no `runtime.Object`. The controller reads
objects with client-go's dynamic client and decodes each into a new one.
`Resource.GVR()` names the resource to the dynamic client. A Resource has
one version, served and stored, with the status subresource.

### The schema

`Resource.CRD()` reflects the resource's CustomResourceDefinition from
`Spec` and `Status`, as `encoding/json` encodes them, into a structural
schema:

| Go | Schema |
|---|---|
| `bool` | `type: boolean` |
| `int8` to `int32`, `uint8`, `uint16` | `type: integer`, `format: int32` |
| `int`, `int64`, `uint`, `uint32`, `uint64` | `type: integer`, `format: int64` |
| `float32`, `float64` | `type: number` |
| `string` | `type: string` |
| `[]byte` | `type: string`, `format: byte` |
| a slice or an array | `type: array`, with `items` |
| `map[string]V` | `type: object`, with `additionalProperties` |
| a struct | `type: object`, with a property for each field |
| `*T` | `T`'s schema, `nullable` unless the field is `omitempty` or `omitzero` |
| `metav1.Time`, `metav1.MicroTime` | `type: string`, `format: date-time` |
| `metav1.Duration` | `type: string` |
| `resource.Quantity`, `intstr.IntOrString` | `x-kubernetes-int-or-string`, `anyOf` integer and string |
| `runtime.RawExtension` | `type: object`, `x-kubernetes-preserve-unknown-fields` |
| `metav1.ObjectMeta`, in a template | `type: object` |

A struct has a property for each field `encoding/json` writes, named by its
`json` tag, or by the field's name when the tag gives none. An embedded
struct with no name in its tag is inlined, as `encoding/json` does, and a
field with the `string` option is a string. The schema is OpenAPI 3.0's,
which a CRD takes, every subschema inlined, with no `$ref`: not the JSON
Schema 2020-12 that [`openapi`](/humus/pr-preview/pr-1210/openapi/) writes, in which a
pointer's type is an array, which the API server rejects.

**A field is required unless its `json` tag has `omitempty` or `omitzero`,
or it has a `Default`.** The encoding decides it, since a field the encoder
always writes is always there. A field promoted from an embedded pointer
isn't required either, since a nil pointer leaves it out. So `size` is
required, and `image` isn't, since it has a default. The CRD's root has
`apiVersion`, `kind` and `metadata`, a required `spec`, and `status`.

### Rules

`Resource.Rules` constrain the fields of `Spec`, of `Status` and of the
types they hold, with typed values attached to each field by pointer.
`kube.Rules` constrains the fields of one type, each named by a `kube.Field`
of a pointer into a value of the type, as `&spec.Size` is above. Each
constraint is a method of `kube.FieldOf[T]`, whose value has the field's
type, so a misspelled constraint, or a value of the wrong type, doesn't
compile:

```text
kube.Field(&spec.Size).Minimun(1)       // kube.FieldOf[int32] has no field or method Minimun
kube.Field(&spec.Size).Minimum("one")   // cannot use "one" (untyped string constant) as int32 value
kube.Field(&spec.Image).Default(3)      // cannot use 3 (untyped int constant) as string value
```

Each constraint becomes its counterpart on the field's schema:

| `FieldOf` method | Schema |
|---|---|
| `Description` | `description` |
| `Default` | `default`, and the field is optional |
| `Enum` | `enum` |
| `Minimum`, `Maximum` | `minimum`, `maximum`, of a number |
| `MinLength`, `MaxLength` | `minLength`, `maxLength`, of a string |
| `Pattern` | `pattern`, of a string |
| `MinItems`, `MaxItems` | `minItems`, `maxItems`, of a list |
| `ListSet` | `x-kubernetes-list-type: set` |
| `ListMap` | `x-kubernetes-list-type: map`, and `x-kubernetes-list-map-keys` |
| `Validation` | a CEL rule and its message, in `x-kubernetes-validations` |

`TypeRules.Description` and `TypeRules.Validation` describe and constrain
the type's own schema, and `kube.Schema` gives a type's schema whole: it is
how a type that encodes itself, with a `MarshalJSON` or `MarshalText`
method, gets a schema at all.

- **Rules belong to a type, not to a place in an object.** Rules of a type
  constrain its fields, those promoted from embedded structs included,
  wherever the type appears in `Spec` or `Status`, so a type such as a port
  or a template is constrained once, and alike everywhere. A field of a
  nested struct, such as `&spec.Template.Port`, is constrained by that
  struct's own Rules, and a rule on it in the outer type's Rules is a
  mistake.
- **A description is a value, not the field's doc comment.** The schema is
  reflected from the types at run time, when doc comments are gone, so
  `kubectl explain` shows only what `Description` gives.
- **`Rules` needs a value of the type to point into,** usually a
  package-level variable, as `spec` and `status` are above. It reads nothing
  from the value: it only finds which field each pointer names.

**`CRD()` returns one error that names every mistake it finds,** each where
in the object it is: a missing `Group`, `Version`, `Kind` or `Plural`, a
`Scope` that is neither `Namespaced` nor `Cluster`, a type that encodes
itself and has no `Schema`, a map whose keys aren't strings, an interface, a
channel, a function, a type that contains itself, which a schema without
`$ref` can't express, a constraint that doesn't fit its field's type, such
as `Pattern` on a number, a `Field` that names no field of the value given to
`Rules`, and two `Rules`, or two `Schema`s, for one type. It doesn't compile
CEL: the API server compiles every rule when the CRD is applied, which the
end user's tests against a cluster do, as [Testing](#testing) shows.

### The golden test

The end user checks the CRD's YAML into their repository, and a golden test
keeps it true to the types. `sigs.k8s.io/yaml` writes it, and the test
rewrites it when it runs with `-update`:

```go
// TestMemcachedCRD is the end user's golden test of their CRD: its YAML is
// checked in, where whatever deploys the controller applies it, and the
// test keeps it true to the types. With -update, it rewrites the file.
func TestMemcachedCRD(t *testing.T) {
	crd, err := Memcached.CRD()
	require.NoError(t, err)
	got, err := yaml.Marshal(crd) // sigs.k8s.io/yaml
	require.NoError(t, err)

	const path = "testdata/memcached.yaml"
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}
```

`update` is a flag of the test package's own:

```go
var update = flag.Bool("update", false, "rewrite the golden files in testdata")
```

A change to the types that isn't in the YAML fails `go test`, in place of a
`make manifests` that someone forgot to run, and the diff of the YAML is in
the same pull request as the change to the types. Memcached's spec, as the
golden file has it:

```yaml
spec:
  properties:
    image:
      default: memcached:1.6.26-alpine3.19
      pattern: '^memcached:'
      type: string
    size:
      description: How many memcached pods run.
      format: int32
      maximum: 3
      minimum: 1
      type: integer
  required:
  - size
  type: object
```

The CRD carries its own `apiVersion` and `kind`, so the YAML applies with
`kubectl apply -f` or a Helm chart as it stands.

**The controller never installs its CRD.** Installing it would give the
controller's ServiceAccount write access to every CRD in the cluster, and a
CRD outlives the controller's pods. Whatever deploys the controller, such as
its Helm chart, its kustomization or its `kubectl apply`, applies the YAML
first. A controller whose CRD isn't installed exits 1, as
[What Serve does](#what-serve-does) describes, rather than waiting forever.

## Reconciling

A `kube.Reconciler` is handed its object, and returns what the object's
status is now. This one applies a Deployment that the Memcached controls,
and reports the Deployment's ready replicas, and whether it is available:

```go
// Reconciler is the end user's Reconciler of the kubebuilder book's
// Memcached operator: it applies a Deployment that the Memcached controls,
// and reports the Deployment's ready replicas, and whether it is available.
type Reconciler struct{ Kube kubernetes.Interface }

func (r Reconciler) Reconcile(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
	labels := map[string]string{"app": "memcached", "memcached": m.Name}
	d, err := r.Kube.AppsV1().Deployments(m.Namespace).Apply(ctx,
		appsv1ac.Deployment(m.Name, m.Namespace).
			WithOwnerReferences(metav1ac.OwnerReference().
				WithAPIVersion(m.APIVersion).WithKind(m.Kind).WithName(m.Name).WithUID(m.UID).
				WithController(true).WithBlockOwnerDeletion(true)).
			WithSpec(appsv1ac.DeploymentSpec().
				WithReplicas(m.Spec.Size).
				WithSelector(metav1ac.LabelSelector().WithMatchLabels(labels)).
				WithTemplate(corev1ac.PodTemplateSpec().
					WithLabels(labels).
					WithSpec(corev1ac.PodSpec().WithContainers(
						corev1ac.Container().WithName("memcached").WithImage(m.Spec.Image))))),
		metav1.ApplyOptions{FieldManager: "memcached-controller", Force: true},
	)
	st := m.Status
	if err != nil {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: "Available", Status: metav1.ConditionUnknown, Reason: "ApplyFailed",
			Message: err.Error(), ObservedGeneration: m.Generation,
		})
		return kube.Result[MemcachedStatus]{Status: &st}, err
	}
	st.ReadyReplicas = d.Status.ReadyReplicas
	meta.SetStatusCondition(&st.Conditions, available(d, m.Generation))
	return kube.Result[MemcachedStatus]{Status: &st}, nil
}
```

`appsv1ac`, `corev1ac` and `metav1ac` are client-go's apply configurations,
which Kubernetes generates and publishes, so no end user generates anything.
`meta` is apimachinery's `k8s.io/apimachinery/pkg/api/meta`. Compared with
the book's controller, there is no `Get` and no not-found, no
`Status().Update` and no fetching again, and no conflict to retry.

**The object is the Reconciler's own.** It comes from the controller's
cache, decoded into a new `Object`, so there is no `Get` and no not-found to
handle: an object gone from the cache is never reconciled, and what it owned
is garbage collected through its owner references. An object being deleted
is left alone, unless a [`Finalize`](#finalizers) holds it.

**The status is applied for it.** When `Result.Status` isn't nil and its
JSON encoding differs from the object's status, the controller applies it
to the status subresource with server-side apply, under the field manager
`Settings.Name`, forcing, since the controller owns the status whole. So a
status write never fails with "the object has been modified", and needs no
`Get` after it. A nil `Status` leaves the status alone, so an error path
that returns a zero `Result` can't wipe the conditions.

### Level-triggered

**Reconciles are level-triggered.** A Reconciler isn't told what changed,
only handed the object as it is now, and brings the cluster to what it
declares. So it is written to be run any number of times: the `Reconciler`
above applies the same Deployment however often it runs, and server-side
apply makes the second apply a no-op.

An object is queued, and reconciled, when:

- the controller starts, for every object, as its informer lists them;
- it is added, or it changes;
- a resource it [owns](#owned-resources) is added, changes or is deleted;
- a positive `Result.After` has passed, as below;
- an error's backoff has passed, as below;
- the resync comes, every `Settings.Resync`, 10h by default, whether or not
  it changed.

The work queue holds each object once, however many of those happen before
a worker takes it, and one object is never reconciled twice at once:
`Settings.Workers` objects are reconciled at once, one by default. Applying
a status changes the object, so it is reconciled once more, returns the same
status, which isn't applied, and settles: two reconciles in all.

### An error, `After` and the resync

Each reconcile settles its object one way, and there is one way to reconcile
again for each reason:

- **an error** retries it after a backoff of its own, from
  `RetryBaseDelay`, doubling to `RetryMaxDelay`, under an overall limit of
  10 retries a second, bursting to 100. The error never ends the Service:
  a restart would only reconcile the same object again, after a crash
  loop's back-off;
- otherwise, **a positive `Result.After`** reconciles it again after that
  long, unless it changes first;
- otherwise it waits for **a change**, or **the resync**.

A [panic](#panics) is the one exception: it is recovered, and never retried.
controller-runtime's `Result.Requeue`, `RequeueAfter` and a returned error,
of which an error makes the Result ignored, become an error or `After`.

### A failure as a condition

**The status is applied whether or not `Reconcile` returned an error,** so a
failure can be a condition and an error together: the condition is what
`kubectl get` and `kubectl describe` show, and the error retries the object.
The `Reconciler` above reports a failed apply as `Available` `Unknown`, with
the reason `ApplyFailed` and the error as its message, and returns the
error.

A failure further on, in a resource the object owns, is reported by copying
that resource's own condition. A Deployment whose pods can't be scheduled,
or whose image won't pull, says so in its `Available` condition, so the
`Reconciler`'s `available` copies it onto the Memcached's:

```go
// available is the Memcached's Available condition: the Deployment's own,
// copied, so a Deployment whose pods don't become available says why in
// the Memcached's status. Until the Deployment controller has written it,
// it is Unknown.
func available(d *appsv1.Deployment, generation int64) metav1.Condition {
	c := metav1.Condition{
		Type: "Available", Status: metav1.ConditionUnknown, Reason: "DeploymentPending",
		ObservedGeneration: generation,
	}
	for _, dc := range d.Status.Conditions {
		if dc.Type != appsv1.DeploymentAvailable {
			continue
		}
		c.Status, c.Message = metav1.ConditionStatus(dc.Status), dc.Message
		if dc.Reason != "" {
			c.Reason = dc.Reason // the API server requires a reason
		}
	}
	return c
}
```

That isn't an error: the Reconciler did its part, and returning an error
would only retry an apply that succeeds. The Deployment's status changing is
what reconciles the Memcached again, through
[`Owns`](#owned-resources), so its condition follows the Deployment's with
no polling. `ObservedGeneration` says which spec the condition is about, so
a reader can tell a condition about the spec they just applied from one
about the last.

## Owned resources

`kube.Owns` reconciles an object again whenever a resource it controls
changes or is deleted, such as the Deployment its Reconciler applies. `Init`
passes it to `NewController`:

```go
ctrl, err := kube.NewController(s, client, settings, Memcached, Reconciler{Kube: kc},
	kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments")),
)
```

So a hand-made change to the Deployment, such as a `kubectl scale`, is
undone, and a change in its status reaches the Memcached's status. When a
resource of an `Owns` is added, changed or deleted, and its owner reference
with `controller: true` names the Resource's group and kind, the object that
reference names is reconciled. The reference's version isn't compared. An
owner reference without `controller: true`, one that names another group or
kind, and a resource with no owner reference reconcile nothing. The
controller watches each owned resource through the metadata client, since
it looks only at their owner references, limited to `Settings.Namespace`.

**The end user sets the controller owner reference when they apply the
resource,** with client-go's apply configurations, as the `Reconciler`
does:

```go
d, err := r.Kube.AppsV1().Deployments(m.Namespace).Apply(ctx,
	appsv1ac.Deployment(m.Name, m.Namespace).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(m.APIVersion).WithKind(m.Kind).WithName(m.Name).WithUID(m.UID).
			WithController(true).WithBlockOwnerDeletion(true)).
```

The same reference is what garbage collects the Deployment when the
Memcached is deleted. `BlockOwnerDeletion` holds a foreground deletion of
the Memcached until the Deployment is gone.

**A resource the controller doesn't own isn't mapped to its objects.**
controller-runtime's `Watches`, with a function from any resource to the
objects it concerns, has no counterpart. A Reconciler that depends on such a
resource, such as a Secret its objects name or a service outside the
cluster, polls it, by returning a positive `Result.After`, such as a minute,
with its status: the object is reconciled again after that long, unless it
changes first.

## Finalizers

`kube.Finalize` keeps each object until the end user's code has cleaned up
after it, such as something outside the cluster that owner references
can't garbage collect. This Controller deletes a Memcached's backups before
the API server deletes the Memcached:

```go
// finalizing returns Init's Controller with a finalizer, which deletes a
// Memcached's backups before the API server deletes the Memcached.
func finalizing(s *humus.Setup, client *kube.Client, settings kube.Settings, kc kubernetes.Interface, b backups) (*kube.Controller, error) {
	return kube.NewController(s, client, settings, Memcached, Reconciler{Kube: kc},
		kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments")),
		kube.Finalize("cache.example.com/finalizer", func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) error {
			return b.Delete(ctx, m.Namespace, m.Name)
		}),
	)
}
```

`backups` is an interface of the end user's own, which a test fakes:

```go
// backups is where the end user keeps each Memcached's backups, outside the
// cluster, where owner references can't garbage collect them.
type backups interface {
	Delete(ctx context.Context, namespace, name string) error
}
```

The object's type is inferred from the function, and `NewController` checks
it is the Controller's `Object[Spec, Status]`.

- **The finalizer is added before the object's first reconcile.** The
  object's first pass adds it to `metadata.finalizers`, and the change that
  makes reconciles the object, with the finalizer on it.
- **Once the object is being deleted, and the finalizer still holds it,**
  the controller calls the function, handed the object as the Reconciler is,
  and never the Reconciler.
- **An error retries the object with backoff,** as a reconcile's does, so the
  function may be called again for an object it has already cleaned up
  after, and must be idempotent: deleting backups that are gone is no error.
  A panic is recovered and never retried, as a Reconciler's is: the
  finalizer stays, and the deletion waits for the object's next reconcile.
- **Once it returns nil, the controller removes the finalizer,** and the API
  server completes the deletion, unless another finalizer still holds the
  object. An object being deleted that the finalizer doesn't hold, as one
  deleted before its first reconcile, is left alone.

**The finalizer is applied, not updated.** Adding it and removing it are
both server-side applies of the object's metadata, under the field manager
`Settings.Name`, which name its uid. So they never conflict with other
writers of the object's metadata, need no `Get` first, and never create an
object that has been deleted since the cache saw it. Removing it is an apply
of the metadata without it, which the API server removes since no other
field manager set it. When another field manager set it too, such as a
`kubectl edit` that added it by hand, it stays on the object: the controller
then fails the reconcile with an error that says so, and retries, calling
the function again, until that field manager's finalizer is removed.

So **a Reconciler that applies its own object,** rather than its status,
does so under a field manager other than `Settings.Name`: the controller's
applies of the metadata would remove the fields it applied under
`Settings.Name`. The `Reconciler` above applies only Deployments, so its
field manager may be the controller's name.

A Controller takes one `Finalize`. A second is a mistake `NewController`
reports.

## Kubernetes' own kinds

A controller can reconcile one of Kubernetes' own kinds, such as a Namespace
or a Secret, rather than a custom resource. A `kube.Kind` is a kind whose Go
type is defined elsewhere, and whose status belongs to whoever defines it:

```go
var Namespaces = kube.Kind[corev1.Namespace]{
	GVR:   corev1.SchemeGroupVersion.WithResource("namespaces"),
	Kind:  "Namespace",
	Scope: kube.Cluster,
}
```

Its `GVR`'s `Group` is `""` for the core group. A `kube.KindReconciler` is
handed client-go's own struct, here a `corev1.Namespace`, and returns only
when to reconcile the object again. This one gives every Namespace a
NetworkPolicy that denies its pods any ingress, unless a policy of the
Namespace's own allows it:

```go
// DefaultDeny is the end user's KindReconciler of Namespaces: it applies a
// NetworkPolicy in each Namespace that selects every pod and allows them no
// ingress, so a Namespace's pods take traffic only from the policies its
// owner adds. The Namespace controls the policy, so a hand-made change to
// it is undone.
type DefaultDeny struct{ Kube kubernetes.Interface }

func (r DefaultDeny) Reconcile(ctx context.Context, ns corev1.Namespace) (time.Duration, error) {
	_, err := r.Kube.NetworkingV1().NetworkPolicies(ns.Name).Apply(ctx, policy(ns), metav1.ApplyOptions{FieldManager: "default-deny", Force: true})
	return 0, err
}

// policy is the NetworkPolicy DefaultDeny applies in ns.
func policy(ns corev1.Namespace) *networkingv1ac.NetworkPolicyApplyConfiguration {
	return networkingv1ac.NetworkPolicy("default-deny", ns.Name).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion("v1").WithKind("Namespace").WithName(ns.Name).WithUID(ns.UID).
			WithController(true)).
		WithSpec(networkingv1ac.NetworkPolicySpec().
			WithPodSelector(metav1ac.LabelSelector()).
			WithPolicyTypes(networkingv1.PolicyTypeIngress))
}
```

`kube.NewKindController` reconciles its objects, with the same `Controller`,
and `Owns` reconciles a Namespace again when its policy changes:

```go
// defaultDeny returns the Controller that gives every Namespace
// DefaultDeny's policy, and applies it again when it changes.
func defaultDeny(s *humus.Setup, client *kube.Client, settings kube.Settings, kc kubernetes.Interface) (*kube.Controller, error) {
	return kube.NewKindController(s, client, settings, Namespaces, DefaultDeny{Kube: kc},
		kube.Owns(networkingv1.SchemeGroupVersion.WithResource("networkpolicies")),
	)
}
```

A Kind has no CRD and no Rules: the kind's own definition is the API
server's. Its objects are reconciled as a Resource's are, except:

- **the KindReconciler is handed client-go's own struct,** decoded from the
  cache into a new `T`, the Reconciler's own copy, as an `Object` is;
- **no status is applied,** since Kubernetes' own controllers write it. A
  positive `after` reconciles the object again after that long, unless it
  changes first, and zero waits for a change, or for the resync;
- **an `Owns` matches the owner references that name the Kind's `GVR.Group`
  and `Kind`.** A namespaced resource can name a cluster-scoped owner, as
  each NetworkPolicy names its Namespace.

`Finalize`, the Lease, shutdown, panics, telemetry and `Synced` work as they
do for a Resource, with the span named `reconcile <Kind>`. A `Finalize`'s
function takes a `T`, which `NewKindController` checks. `NewKindController`
reports every mistake `NewController` does in the Settings and the parts,
and a GVR without a `Version` or a `Resource`, a missing `Kind`, a `Scope`
that is neither `Namespaced` nor `Cluster`, a `T` whose pointer isn't a
`metav1.Object`, a `Settings.Namespace` for a cluster-scoped Kind, and a nil
KindReconciler.

## Panics

**A panic in `Reconcile`, or in a `Finalize`'s function, is recovered and
recorded, but never retried.** It is an exception event on the reconcile's
span and an ERROR record, `reconcile panicked`, with the stack of the panic,
as [Telemetry](#telemetry) describes. The panic never ends the Service, and
the other objects go on being reconciled.

A panic is a mistake in the controller, such as a nil pointer, an index out
of range or a division by zero: it shows the code's assumptions about the
cluster are wrong. A retry would only panic again, and fill the logs and
traces with the same bug once per backoff. So the object is settled as one
that reconciled with no `Result.After` is: no status is applied, since there
is no `Result`, and it is reconciled again only when it, or a resource it
owns, changes, when the resync comes, or when a new leader takes over. A
change made while the panicked reconcile ran reconciles it again once that
reconcile is done, as after any reconcile. The rollout that ships the fix
starts a new leader, which reconciles every object, the panicked ones with
the fixed code. A panic of a `Finalize`'s function leaves the object with
its finalizer, so its deletion waits for its next reconcile.

This differs from controller-runtime, which turns a panic into an error and
retries it with backoff, and from `queue`'s consumers, which crash: a
restart doesn't help a level-triggered loop, since the same object panics
again after every restart, and it stops every other object being reconciled.

## Settings

`kube.Settings` follows the
[settings convention](/humus/pr-preview/pr-1210/configuration/#what-a-package-with-settings-exports):
plain Go values, a check for each field, and `kube.DefaultSettings()` for the
defaults. Every field's zero value means zero, never "use the default", so
an `Init` starts from `DefaultSettings()` and changes the fields it sources.
The package reads no environment variable: the end user sources the
settings, with whatever variable names they choose, and runs each field's
check as they do:

```go
// readSettings sources the Controller's Settings from the environment.
func readSettings(ctx context.Context, s *humus.Setup) (kube.Settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	d := kube.DefaultSettings()
	return config.Struct(func(r *config.Resolver) kube.Settings {
		k := d
		// No .Default: the name, and the Lease's namespace and identity, are required.
		k.Name = r.Get(env("CONTROLLER_NAME").Validate(kube.CheckName))
		k.Workers = r.Get(env("CONTROLLER_WORKERS").Parse(strconv.Atoi).Default(d.Workers).Validate(kube.CheckWorkers))
		// The pod's spec sets both from the downward API.
		k.LeaderElection.Namespace = r.Get(env("POD_NAMESPACE").Validate(kube.CheckNamespace))
		k.LeaderElection.Identity = r.Get(env("POD_NAME").Validate(kube.CheckIdentity))
		return k
	}).Read(ctx)
}
```

`env` names a variable through the Process's environment, and `config` is
[bedrock's](https://pkg.go.dev/github.com/z5labs/bedrock/config), as
[Configuring a humus service](/humus/pr-preview/pr-1210/configuration/) describes. A
variable that isn't set fails as not found, and a value that fails its check
fails `Init`, naming the variable, so the process exits 78. With none of the
three set, `Init` fails with:

```text
init: config CONTROLLER_NAME: not found
config POD_NAMESPACE: not found
config POD_NAME: not found
```

| Field | Default | Check | Is |
|---|---|---|---|
| `Name` | none, required | `CheckName`: a DNS-1123 subdomain | the controller's field manager, and its Lease's name |
| `Namespace` | `""`, every namespace | `CheckWatchNamespace`: `""` or a DNS-1123 label | the one namespace the controller watches, its owned resources' included |
| `Workers` | 1 | `CheckWorkers`: at least 1 | how many objects are reconciled at once |
| `Resync` | 10h | `CheckResync`: positive | how often every object is reconciled again, whether or not it changed |
| `RetryBaseDelay` | 5ms | `CheckRetryDelay`: positive | the first backoff of an object whose reconcile failed |
| `RetryMaxDelay` | 1000s | `CheckRetryDelay`: positive | the longest backoff, doubled up to from `RetryBaseDelay` |
| `LeaderElection.Namespace` | none, required | `CheckNamespace`: a DNS-1123 label | the Lease's namespace |
| `LeaderElection.Identity` | none, required | `CheckIdentity`: not empty | this replica's name in the Lease |
| `LeaderElection.LeaseDuration` | 15s | none of its own | how long the other replicas wait before taking over from a leader that stopped renewing |
| `LeaderElection.RenewDeadline` | 10s | none of its own | how long the leader keeps trying to renew before it gives up leading |
| `LeaderElection.RetryPeriod` | 2s | `CheckRetryPeriod`: positive | how often a replica tries to take the Lease, and the leader to renew it |

**The defaults are Kubernetes' own.** `RetryBaseDelay`'s 5ms and
`RetryMaxDelay`'s 1000s are the per-object backoff of client-go's
`DefaultTypedControllerRateLimiter`. The Lease's 15s, 10s and 2s are
kube-controller-manager's.

**Three bounds take two fields to tell,** so no one field's check has them,
and `NewController` reports them: `RetryMaxDelay` is at least
`RetryBaseDelay`, `LeaseDuration` is longer than `RenewDeadline`, and
`RenewDeadline` is longer than 1.2 `RetryPeriod`s. The defaults keep all
three. A `Settings.Namespace` for a cluster-scoped Resource or Kind is a
mistake too.

**`Name`, `LeaderElection.Namespace` and `LeaderElection.Identity` have no
default,** so the end user always sources them, with no `.Default` in their
chains. humus imposes no name and no namespace, as it imposes no port.

### The Lease's namespace and identity, from the downward API

In a pod, the Lease's `Namespace` and `Identity` come from `POD_NAMESPACE`
and `POD_NAME`, which the pod's spec sets from the downward API:

```yaml
env:
  - name: CONTROLLER_NAME
    value: memcached-controller
  - name: POD_NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
  - name: POD_NAME
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
```

Each replica's pod has a name of its own, so each has an identity of its own
in the Lease, and the Lease sits beside the controller, in its own
namespace.

## What `Serve` does

`Controller.Serve` first lists the resource, and each resource an `Owns`
names, once, with a limit of 1. An error, such as a CRD that isn't
installed, ends `Serve` with an error that names the resource, so the App
exits 1, rather than leave a controller that waits forever:

```text
kube: listing cache.example.com/v1alpha1, Resource=memcacheds: the server could not find the requested resource
```

It then starts an informer on the resource, and one on each owned resource,
limited to `Settings.Namespace`, waits for their caches to sync, and
campaigns for the Lease. The leader's workers then take keys from the work
queue and reconcile them.

## Leader election and shutdown

**Every controller elects a leader,** on a Lease named `Settings.Name`, in
`LeaderElection.Namespace`, as `LeaderElection.Identity`. There is no option
to turn it off: apart from the Lease's RBAC, it costs one replica nothing,
and two replicas without it would apply the same objects twice.

- **Standbys keep their caches warm.** Every replica lists and watches, and
  only the Lease's holder reconciles, so a new leader starts reconciling at
  once, with no list of its own to wait for. A standby's caches cost it the
  memory and the watches a leader's do.
- **A lost Lease exits 1.** When the Lease's renewal fails for
  `RenewDeadline`, or another replica takes it, the controller stops taking
  keys, cancels the reconciles in flight, and `Serve` returns
  `kube.ErrLostLeadership`, so the App exits 1. Another replica may already
  be reconciling, so carrying on would reconcile objects twice. The pod
  campaigns again when it restarts, as client-go's own leader election
  example does.
- **SIGTERM releases the Lease.** On `Serve`'s context, as when the App
  shuts down, the controller stops taking keys, and each worker finishes the
  reconcile it has in flight. Then it releases the Lease, so the next
  replica takes it without waiting out `LeaseDuration`, stops its informers,
  and `Serve` returns nil, so the App exits 0.
- **The backlog is left for the next leader.** The work queue is shut down,
  never drained: the next leader lists every object when its informer
  starts, so it reconciles whatever was waiting, and a level-triggered loop
  loses nothing by it.

A reconcile's context is cancelled only when `Controller.Shutdown`'s context
is done or the Lease is lost: then `Shutdown` cancels the reconciles in
flight, waits for them, and returns an error that wraps the context's error.
`humus.Main`'s defaults, a 20s grace period and no drain, fit a controller,
inside the pod's default `terminationGracePeriodSeconds` of 30s. A
Reconciler whose reconciles can take longer passes its context on to every
call it makes, so a cancelled reconcile returns promptly.

## Readiness

`Controller.Synced` returns a `health.Monitor` that is unhealthy until the
controller's caches have synced, and healthy from then until `Serve`
returns, on every replica, leader or not, so a rollout isn't held up by the
replicas waiting for the Lease. It goes in the readiness of whatever serves
the pod's probes. A controller's pod has no server of its own, so that is a
probes server made with `httphealth.NewService`, returned beside the
Controller:

```go
probes, err := httphealth.NewService(s, probeSettings, health.Readiness(ctrl.Synced()))
if err != nil {
	return nil, err
}
return []humus.Service{ctrl, probes}, nil
```

A new pod is then held not ready until its caches have synced, so a rollout
whose new pods can't list their resource, for want of RBAC or a CRD, stalls,
and is reported failed at its progress deadline, rather than replacing every
pod that worked. [Health](/humus/pr-preview/pr-1210/health/) describes the probes
server and its manifest.

**`Synced` is for readiness only.** Restarting the pod doesn't make the
Lease come any sooner, and a Serve that can't list exits on its own.
`Synced` may be called in `Init`, before `Serve`, and its monitor never
returns an error.

## RBAC

**humus generates no RBAC.** The end user writes the rules, in their Helm
chart or manifests, beside the CRD. A Controller needs these itself:

- **for a Resource:** get, list and watch on its resource, and patch on its
  status subresource, where the status is applied;
- **for a Kind:** get, list and watch on its kind, and no status, which the
  controller never writes;
- **for the Lease:** get, create and update on Leases, in
  `LeaderElection.Namespace`.

What the parts add:

- **each `Owns`:** list and watch on the resource it names;
- **a `Finalize`:** patch on the resource or kind itself, where the
  finalizer is applied.

The end user adds their Reconciler's own: an apply is a patch, and an apply
that creates its object needs create as well. For the Memcached controller,
with its `Finalize`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: memcached-controller
rules:
  # The Controller's own, for the Memcached Resource and its Finalize.
  - apiGroups: ["cache.example.com"]
    resources: ["memcacheds"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: ["cache.example.com"]
    resources: ["memcacheds/status"]
    verbs: ["patch"]
  # Owns, and the Reconciler's applies.
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["list", "watch", "patch", "create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: memcached-controller-leader-election
  namespace: memcached-system
rules:
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update"]
```

Each is bound to the controller's ServiceAccount with a
`ClusterRoleBinding` and a `RoleBinding`. A controller limited to one
namespace, with `Settings.Namespace`, needs only a Role in that namespace in
place of the ClusterRole. The `DefaultDeny` controller needs get, list and
watch on `namespaces`, list, watch, patch and create on
`networking.k8s.io`'s `networkpolicies`, and the Lease's Role.

## Telemetry

A Controller reports through `s.Telemetry`, under the scope
`github.com/z5labs/humus/kube`, and never through OpenTelemetry's globals.

### The reconcile's span

**Every reconcile is a root span,** `reconcile <Kind>`, such as
`reconcile Memcached`, with `k8s.namespace.name` for a namespaced object,
and `humus.kube.object.name`. `Reconcile`'s context carries it, so the
Reconciler's API requests are its children. An error sets its status to
Error. With a `Finalize`, adding the finalizer, and calling the function and
removing the finalizer, are each a reconcile too, and the function's context
carries the span. A reconcile is a root span because nothing caused it that
a trace could link to: an informer's event has no trace context.

### The records

- **A failed reconcile,** a failed `Finalize` among them, is one WARN record,
  `reconcile failed`, in the span's context, with the object and the error.
- **A panic** is an exception event on the span, with `exception.type`,
  `exception.message` and `exception.stacktrace`, the stack of the panic,
  and sets the span's status to Error. It is one ERROR record,
  `reconcile panicked`, in the span's context, with the object and the same
  three attributes, and no `reconcile failed`.

### client-go's records

client-go's own records, from the reflector, the work queue and leader
election, reach the `LoggerProvider` through a logger the controller puts in
their context. Those klog writes by default arrive: an Info at verbosity 0,
as an INFO record, and every Error, as an ERROR record. Nothing reaches the
real stderr from what the Controller runs. client-go's
[ambient leaks](#what-client-go-still-reaches) are below.

### The API requests

**Every API request is a client span from `otelhttp`,** as the Client
traces them, and is measured by `otelhttp`'s client instruments, such as
`http.client.request.duration`, on the Setup's `MeterProvider`. A watch has
no span: it lasts as long as an informer runs. So a reconcile's trace shows
each apply it made, and the API server's latency and its errors, a 409 or a
429, are `otelhttp`'s instruments, by status code.

### The controller's instruments

The controller's own six instruments measure its work queue and its
reconciles, from the Setup's `MeterProvider`, in the scope
`github.com/z5labs/humus/kube`. OpenTelemetry has no conventions for
controllers, so they are humus's, under `humus.kube.controller.`, as
[humus's own instruments](/humus/pr-preview/pr-1210/telemetry/#humuss-own-instruments)
lists:

| Instrument | Kind | Unit | Is |
|---|---|---|---|
| `humus.kube.controller.queue.depth` | UpDownCounter (`Int64UpDownCounter`) | `{object}` | objects waiting in the work queue for a worker: +1 when a key is added, and -1 when a worker takes it |
| `humus.kube.controller.queue.adds` | Counter (`Int64Counter`) | `{object}` | objects added to the work queue, not counting one already waiting |
| `humus.kube.controller.queue.duration` | Histogram (`Float64Histogram`) | `s` | how long an object waited in the work queue for a worker. The wait of a `Result.After` or of a backoff comes before the key is added, so it isn't counted |
| `humus.kube.controller.reconcile.duration` | Histogram (`Float64Histogram`) | `s` | how long each reconcile took, by its outcome: one for each `reconcile <Kind>` span, over the same time |
| `humus.kube.controller.reconcile.unfinished_duration` | Gauge (`Float64Gauge`) | `s` | how long the reconciles in flight have run, added together |
| `humus.kube.controller.reconcile.longest_unfinished_duration` | Gauge (`Float64Gauge`) | `s` | how long the longest reconcile in flight has run |

Every data point carries `humus.kube.controller.name`, which is
`Settings.Name`. `reconcile.duration`'s also carry
`humus.kube.controller.reconcile.outcome`:

- `error`, for a reconcile whose key is retried with backoff;
- `panic`, for a recovered panic;
- `success`, for everything else, a `Result.After` included.

No data point carries an object's namespace or name: an object's reconciles
are its spans. Both histograms take OpenTelemetry's advice for a duration in
seconds, the boundaries 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5,
0.75, 1, 2.5, 5, 7.5 and 10.

- **The work queue records them.** Every instrument but
  `reconcile.duration` is recorded by client-go's work queue, which is named
  `Settings.Name`, through a `workqueue.MetricsProvider` of the controller's
  own, set in the queue's config, never client-go's global provider. The
  queue updates the two gauges every 500ms while it runs.
- **Two of the queue's own metrics aren't recorded:** its work duration,
  which times what `reconcile.duration` times without the outcome, and its
  retries, which count every `AddAfter`, so every `Result.After` would count
  as a retry. The `error` outcome counts the retries exactly.
- **A standby records them too.** It runs no workers, so nothing leaves its
  queue, and its `queue.depth` holds a key for every object. Until it takes
  the Lease, its `reconcile.duration` records nothing, and its two gauges
  read 0. So read `queue.depth` per replica, by its resource's attributes: a
  sum over the replicas counts the standbys' keys too.
- **The gauges catch a stuck reconcile.** A reconcile that never returns
  records no `reconcile.duration`, while `longest_unfinished_duration` grows
  for as long as it runs.

In Prometheus, they are `humus_kube_controller_queue_depth`,
`humus_kube_controller_queue_adds_total`,
`humus_kube_controller_queue_duration_seconds`,
`humus_kube_controller_reconcile_duration_seconds`,
`humus_kube_controller_reconcile_unfinished_duration_seconds` and
`humus_kube_controller_reconcile_longest_unfinished_duration_seconds`, and
the attributes are the labels `humus_kube_controller_name` and
`humus_kube_controller_reconcile_outcome`:

```promql
# The controller's reconciles that failed, a second, over 5 minutes.
sum by (humus_kube_controller_name) (rate(humus_kube_controller_reconcile_duration_seconds_count{humus_kube_controller_reconcile_outcome="error"}[5m]))

# A reconcile panicked in the last hour.
increase(humus_kube_controller_reconcile_duration_seconds_count{humus_kube_controller_reconcile_outcome="panic"}[1h]) > 0

# A reconcile has been running for more than a minute.
humus_kube_controller_reconcile_longest_unfinished_duration_seconds > 60
```

### What client-go still reaches

client-go reaches around the process in six places. The Connection and the
Client's transport close four of them: `rest.InClusterConfig`, which lint
denies in every humus module, a token file re-read from the disk, the proxy
from the environment, and its transports' three variables. Two stay, and
are on the lifecycle's list of
[ambient leaks](/humus/pr-preview/pr-1210/lifecycle/#ambient-leaks):

- **client-go's feature gates** read `KUBE_FEATURE_*` variables from the real
  environment, with `os.LookupEnv`, the first time a gate is used. The only
  way to replace them is `features.ReplaceFeatureGates`, which is global, so
  humus never calls it, and lint denies it.
- **klog's global logger** writes to the real stderr, for a code path of
  client-go's that finds no logger in its context. The Controller puts one
  in the context of everything of client-go's it runs, a reconcile's
  included, so what still reaches it is client-go code handed a context
  without one, such as a call the end user's `Init` makes, or code that
  logs through klog's functions with no context at all.

The package sets no global of client-go's: no klog setting, no feature gate,
and no metrics provider. Lint denies klog's `SetLogger`,
`SetLoggerWithOptions`, `SetOutput` and `InitFlags` in every humus module.

## Failure injection

A controller takes part in
[failure injection](/humus/pr-preview/pr-1210/failure-injection/) as every humus
server and client does. Its reconciles answer no request, so a scenario
reaches them through their object: **an object's annotation carries a
scenario into that object's reconciles,** and **every request through the
Client is an injection point.** Both need an App with failure injection,
turned on with `humus.WithFaults`, as
[Turning it on](/humus/pr-preview/pr-1210/failure-injection/#turning-it-on)
describes. The design is in
[Proposal: failure injection for Kubernetes controllers](https://github.com/z5labs/humus/discussions/922),
with its revisions
[1](https://github.com/z5labs/humus/discussions/922#discussioncomment-19029029)
and [2](https://github.com/z5labs/humus/discussions/922#discussioncomment-19029220).

### Trusting an object's annotation: `TrustScenario`

`kube.TrustScenario` is a `Part`, for `NewController` and
`NewKindController` alike. It names the annotation that carries a scenario,
and a `trust` func, handed the object, that decides whether to take it.
This controller runs experiments only in the staging namespace:

```go
// underTest returns the Memcached controller under failure injection: a
// Memcached in the staging namespace carries the scenario its annotation
// holds into its reconciles.
func underTest(s *humus.Setup, client *kube.Client, settings kube.Settings, reconciler kube.Reconciler[MemcachedSpec, MemcachedStatus]) (*kube.Controller, error) {
	return kube.NewController(s, client, settings, Memcached, reconciler,
		kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments")),
		// Only the staging namespace runs experiments. An admission policy lets
		// only the chaos harness's ServiceAccount set the annotation there.
		kube.TrustScenario(scenarioAnnotation, func(obj metav1.Object) bool {
			return obj.GetNamespace() == "staging"
		}),
	)
}
```

The end user names the annotation, as they name the baggage member of
`jsonscenario`:

```go
const scenarioAnnotation = "acme.example.com/failure-scenario"
```

- **Without `TrustScenario`, nothing is read,** and no reconcile carries a
  scenario, as a server's nil `TrustScenario` trusts no caller. A
  controller that never adds it can't be put under test by anyone who can
  annotate its objects.
- **`trust` is asked only about an object whose annotation carries a
  scenario,** and only when the App has failure injection. It is handed its
  own copy of the object, and is set in code, not sourced.
- **A panic in `trust` is recovered and recorded** as a panic in the
  Reconciler is, under [Panics](#panics): the reconcile's outcome is
  `panic`, its key is forgotten, and nothing else runs, so no finalizer and
  no status is applied.
- **`NewController` and `NewKindController` report three mistakes:** an
  annotation that isn't a valid annotation key, a nil `trust`, and a second
  `TrustScenario`.

**Who may set the annotation is decided outside the process,** by RBAC and
admission, and `trust` reads what those decisions leave visible: the
object's namespace, a label an admission policy guards, or the object's
owner. Here a ValidatingAdmissionPolicy lets only the chaos harness's
ServiceAccount set or change the annotation on a Memcached in staging, so
trusting every Memcached in staging trusts the harness alone:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: memcached-failure-scenario
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["cache.example.com"]
        apiVersions: ["*"]
        operations: ["CREATE", "UPDATE"]
        resources: ["memcacheds"]
  variables:
    - name: key
      expression: "'acme.example.com/failure-scenario'"
    - name: scenario
      expression: >-
        has(object.metadata.annotations) && variables.key in object.metadata.annotations
        ? object.metadata.annotations[variables.key] : ''
    - name: oldScenario
      expression: >-
        oldObject != null && has(oldObject.metadata.annotations) && variables.key in oldObject.metadata.annotations
        ? oldObject.metadata.annotations[variables.key] : ''
  validations:
    - expression: >-
        variables.scenario == variables.oldScenario ||
        request.userInfo.username == 'system:serviceaccount:chaos:harness'
      message: only the chaos harness sets acme.example.com/failure-scenario
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: memcached-failure-scenario
spec:
  policyName: memcached-failure-scenario
  validationActions: ["Deny"]
  matchResources:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: staging
```

**When a reconcile's span starts, the controller reads the annotation** from
the object as the cache holds it. That is before it adds or removes its
finalizer, calls a `Finalize`'s function, or calls the Reconciler, so each
of them, the controller's own applies, and every request and injection
point they reach see the scenario. The annotation is read one of four ways:

- **no annotation, or none of the `Decoder`'s members in it:** the reconcile
  carries no scenario, and nothing is recorded;
- **a value that isn't W3C baggage:** the reconcile carries no scenario, and
  its span gets a `humus.fit.strip` event with `error.type` and
  `error.message`. The reconcile goes on;
- **an object `trust` doesn't trust:** the reconcile carries no scenario, and
  its span gets a `humus.fit.strip` event with no attributes, as a server's
  request does;
- **a trusted object:** the `Decoder`'s members join the reconcile's
  context, and no other member of the annotation does, so an annotation
  can't put arbitrary baggage on every request a reconcile makes.

**The scenario belongs to the object.** A reconcile that a change to an
[owned resource](#owned-resources) triggered carries the owner's scenario,
since it is the owner that is reconciled, and the owned resource's
annotations aren't read.

### Writing a scenario into an annotation

The annotation's value is W3C baggage that carries a scenario as the App's
`Decoder` reads it, such as `jsonscenario`'s member. So a scenario written
for an HTTP request's `baggage` header goes on an object unchanged, and one
tool writes both: `fit.ContextWithScenario` and a codec that is also a
`fit.Encoder`, as
[Writing a scenario](/humus/pr-preview/pr-1210/failure-injection/#writing-a-scenario)
describes. The tool that runs the experiment then takes the baggage it
leaves:

```go
// annotationValue returns the value of an annotation that carries s,
// written with codec, as the tool that runs an experiment writes it: the
// W3C baggage that fit.ContextWithScenario leaves.
func annotationValue(codec fit.Encoder, s fit.Scenario) (string, error) {
	ctx, err := fit.ContextWithScenario(context.Background(), codec, s)
	if err != nil {
		return "", err
	}
	return baggage.FromContext(ctx).String(), nil
}
```

This scenario aborts every apply of a Deployment, with `jsonscenario`'s
codec of the member `acme.fault`:

```go
value, err := annotationValue(codec, fit.Scenario{Rules: []fit.Rule{
	{Point: "kube:deployments.apps:patch", Fault: fit.Fault{Abort: true}},
}})
```

Its value is one `acme.fault` member, percent-encoded as W3C Baggage
requires:

```text
acme.fault={%22rules%22:[{%22point%22:%22kube:deployments.apps:patch%22%2C%22abort%22:true}]}
```

### Injection points

**Every request through the Client's transport is an injection point** of
`s.Faults`, the end user's clientsets' included, as every humus client is.
The point names what the request does to which resource, as the API
server's authorizer and audit log name it:

```text
kube:<resource>[.<group>][/<subresource>]:<verb>
```

- `<resource>` is the plural, and `<group>` is left out for the core group,
  as kubectl writes a resource: `deployments.apps`, `secrets`,
  `memcacheds.cache.example.com`;
- `<verb>` is the API server's: `get`, `list`, `watch`, `create`, `update`,
  `patch`, `delete` or `deletecollection`, or `proxy` for a request to a
  resource's proxy. A server-side apply is a `patch`;
- a request for a path that isn't a resource, such as discovery's `/apis`,
  is `kube:<path>:<verb>`, such as `kube:/apis:get`.

A point carries no namespace and no object's name: the scenario is already
scoped to one object's reconciles. `fit` matches a point exactly, or by a
prefix ending in `*`, so a rule can fail everything about one resource:

| A rule | Fails |
|---|---|
| `kube:*` | every request to the API server |
| `kube:deployments.apps:*` | every request about Deployments, but not their subresources |
| `kube:deployments.apps:patch` | every apply of a Deployment |
| `kube:memcacheds.cache.example.com/status:patch` | the controller's apply of a Memcached's status. A Kind's controller applies no status, so only a Resource's controller makes this request |
| `kube:memcacheds.cache.example.com:patch` | the controller's applies of a Memcached's finalizer |

A request whose context carries no scenario is evaluated, stripped and
recorded not at all. The informers, leader election, the Lease's release
and the check that a resource is served run on `Serve`'s context, which
never carries a scenario, so they always run without one.

**The injection layer sits between `otelhttp` and the token.** So a delay is
inside the request's span and in `http.client.request.duration`, an abort
fails before `Connection.Token` is called, and the `humus.fit.evaluation`
event lands on the request's span. A watch has no span, so its event lands
on the span in its context, such as the reconcile's.

### A fault is a real failure

A fault emulates a failure of the API server, and everything that meets it
reacts as it would to that failure:

| | An abort | A delay |
|---|---|---|
| The request | fails with nothing sent, and with no call to `Connection.Token`, as an unreachable API server's does. client-go returns an error that wraps the `*fault.InjectedError`, which `errors.As` finds | waits before the request is sent, inside the request's context, and then goes on. Cut short, it fails with an `*fault.InjectedError` that wraps the context's cause, except when a `rest.Config`'s `Timeout` cuts it short (below) |
| client-go | returns it at once, not even retrying a GET: it retries only a connection that was reset, ended early or lost, and a response with `Retry-After` | sends the request once the delay is over |
| The reconcile | whatever the Reconciler does with the error. Returned, it retries the object with backoff, and `humus.kube.controller.reconcile.outcome` is `error` | slower, as `humus.kube.controller.reconcile.duration` shows |

**The controller's own writes are requests like any other.** An abort at
`…/status:patch` fails the status apply, which retries the object as any
failed reconcile does, and an abort at the resource's own `patch` fails
adding or removing a finalizer.

**A delay that a `rest.Config`'s `Timeout` cuts short** is the one
exception. `Client.Config()` sets no `Timeout`, so the controller's own
requests always fail as the table says. An end user who sets one on their
copy gets two timers from client-go: a deadline on each request's context,
and the `http.Client`'s own `Timeout`. They go off together, and when
net/http's has gone off by the time the transport returns, net/http replaces
the transport's error with a timeout error of its own, which matches
`context.DeadlineExceeded` and holds no `*fault.InjectedError`. Either way,
the request timed out, and its span has the `error.type`
`github.com/z5labs/humus/fault.InjectedError`.

**The API server takes no part in failure injection,** so the Client strips
the scenario from every request it sends, after the point has read it: its
members leave the request's `baggage` header, every other member and the
trace context stay, and the request's span gets a `humus.fit.strip` event.
There is no `ForwardScenario`. A scenario goes on to the reconciled object's
children instead, as
[An operator of two controllers](#an-operator-of-two-controllers) shows.

### Starting and ending an experiment

**Adding the annotation starts an experiment, and removing it ends one, at
once:** either changes the object, which reconciles it straight away. The
harness's `setScenario` does both, with a JSON merge patch:

```go
// setScenario starts or ends an experiment on the Memcached name: it sets
// the annotation to value, a scenario, with a JSON merge patch, or removes
// the annotation when value is nil. Either reconciles the Memcached at
// once.
func setScenario(ctx context.Context, memcacheds dynamic.ResourceInterface, name string, value any) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{scenarioAnnotation: value}},
	})
	if err != nil {
		return err
	}
	_, err = memcacheds.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
```

`dynamic` is client-go's `k8s.io/client-go/dynamic`, and `types` is
apimachinery's `k8s.io/apimachinery/pkg/types`.

- **A scenario stays as long as its annotation.** Every retry meets it
  again, so the scenario above, which aborts the Reconciler's apply of the
  Deployment, keeps the Memcached backing off to `RetryMaxDelay`, as an
  outage would. The status apply isn't aborted, so the Memcached's status
  shows the Reconciler's `ApplyFailed` condition all the while.
- **The reconcile after the annotation is removed carries no scenario,** so
  the next apply of the Deployment succeeds. A `Decoder` that knows expiry
  can end an experiment sooner.

### An operator of two controllers

An operator is often several controllers, each reconciling the objects the
one above it applies. **In a reconcile that carries a scenario, the Client
sets the `TrustScenario`'s annotation on each child the reconcile
applies,** so one experiment on the top object reaches every controller
below it. This operator's second controller, `Budget`, reconciles the
Deployments that Memcacheds control, and applies a PodDisruptionBudget for
each:

```go
// Deployments is Kubernetes' own Deployment kind.
var Deployments = kube.Kind[appsv1.Deployment]{
	GVR:   appsv1.SchemeGroupVersion.WithResource("deployments"),
	Kind:  "Deployment",
	Scope: kube.Namespaced,
}
```

```go
// Budget is the end user's KindReconciler of the Deployments that
// Memcacheds control, the second controller of their operator: it applies
// a PodDisruptionBudget that the Deployment controls, so a drain evicts a
// Memcached's pods one at a time.
type Budget struct{ Kube kubernetes.Interface }

func (r Budget) Reconcile(ctx context.Context, d appsv1.Deployment) (time.Duration, error) {
	if !memcachedChild(&d) || d.Spec.Selector == nil {
		return 0, nil
	}
	_, err := r.Kube.PolicyV1().PodDisruptionBudgets(d.Namespace).Apply(ctx,
		policyv1ac.PodDisruptionBudget(d.Name, d.Namespace).
			WithOwnerReferences(metav1ac.OwnerReference().
				WithAPIVersion("apps/v1").WithKind("Deployment").WithName(d.Name).WithUID(d.UID).
				WithController(true)).
			WithSpec(policyv1ac.PodDisruptionBudgetSpec().
				WithMaxUnavailable(intstr.FromInt32(1)).
				WithSelector(metav1ac.LabelSelector().WithMatchLabels(d.Spec.Selector.MatchLabels))),
		metav1.ApplyOptions{FieldManager: "memcached-budget", Force: true},
	)
	return 0, err
}

// memcachedChild reports whether a Memcached controls obj.
func memcachedChild(obj metav1.Object) bool {
	owner := metav1.GetControllerOf(obj)
	return owner != nil && owner.Kind == Memcached.Kind && strings.HasPrefix(owner.APIVersion, Memcached.Group+"/")
}
```

The App runs both controllers, each with `Settings` of its own, whose
`Name` is its Lease's and its field manager's. `Budget`'s controller trusts
the scenario on a Deployment a Memcached controls:

```go
// operator returns the Controllers of an operator of two: the Memcached
// controller under test, and Budget's, which carries the scenario that a
// Memcached's reconcile sets on its Deployment into the Deployment's
// reconciles. Each has Settings of its own, with a Name of its own.
func operator(s *humus.Setup, client *kube.Client, settings, budgetSettings kube.Settings, kc kubernetes.Interface) ([]humus.Service, error) {
	memcacheds, err := underTest(s, client, settings, Reconciler{Kube: kc})
	if err != nil {
		return nil, err
	}
	budgets, err := kube.NewKindController(s, client, budgetSettings, Deployments, Budget{Kube: kc},
		kube.Owns(policyv1.SchemeGroupVersion.WithResource("poddisruptionbudgets")),
		// An admission policy lets only the operator's ServiceAccount set the
		// annotation on Deployments.
		kube.TrustScenario(scenarioAnnotation, memcachedChild),
	)
	if err != nil {
		return nil, err
	}
	return []humus.Service{memcacheds, budgets}, nil
}
```

`policyv1ac` is client-go's `k8s.io/client-go/applyconfigurations/policy/v1`,
and `intstr` is apimachinery's `k8s.io/apimachinery/pkg/util/intstr`. A
scenario on a staging Memcached that aborts
`kube:poddisruptionbudgets.policy:patch` spares the Memcached's own
reconciles, whose apply of the Deployment carries the scenario on, and
fails `Budget`'s apply of the budget, which retries the Deployment with
backoff.

The Client changes a request that is all of these:

- a server-side apply, a `PATCH` whose content type is
  `application/apply-patch+yaml`, to an object, not to one of its
  subresources;
- made with the reconcile's context, or a context made from it;
- of an object whose `metadata.ownerReferences` has a controller reference
  whose `uid` is the reconciled object's, as Kubernetes' garbage collector
  names a child. The body declares the reference, so the child's resource
  needs no `Owns`.

It sets `metadata.annotations[<annotation>]` in the request's body to the
scenario's members as W3C baggage, without any other member of the
reconcile's baggage, replacing any value the body gave it, and sends the
body on as JSON, which is YAML too. That happens after the point is
evaluated, so an aborted apply sends nothing, and after the request's
baggage is stripped, so the API server still gets no scenario. The
`Reconciler` applies its Deployment as it always does, with nothing added.

- **The child's controller decides.** `Budget`'s controller carries the
  Deployment's scenario into the Deployment's reconciles when its `trust`
  trusts the Deployment, and its own apply of the budget carries the
  scenario on to the grandchild. What makes a child's annotation worth
  trusting is an admission policy that lets only the operator's
  ServiceAccount set it on the children's resources, here Deployments.
- **Ending the experiment on the top object ends it all the way down.**
  Removing the Memcached's annotation reconciles it at once, without a
  scenario. Its apply of the Deployment no longer declares the annotation,
  and a server-side apply removes what its field manager stops declaring, so
  the API server removes the annotation, unless another field manager also
  set it. That changes the Deployment, whose controller reconciles it at
  once, without a scenario, and so on down.
- **A child that no controller reads keeps an inert annotation** for as long
  as the experiment runs, such as the budget here, or the Deployment of a
  Memcached controller with no second controller. Kubernetes' own
  controllers ignore it, and a Deployment's annotations aren't its pods'
  template, so it starts no rollout.

What is left alone:

- a request in a reconcile with no scenario, or outside a reconcile;
- a create, an update, and a patch that isn't an apply. Only an apply takes
  the annotation off again: a child created or updated with it would keep it
  after the experiment ends, and keep failing;
- a body that isn't JSON or YAML. client-go sends JSON unless the end user
  sets the `rest.Config`'s `ContentType` to protobuf, or turns on its
  `ClientsAllowCBOR` and `ClientsPreferCBOR` feature gates for CBOR. Such an
  apply is sent as it is;
- the controller's applies of the reconciled object's own status and
  finalizer, which are of the object, and it isn't its own child;
- an object that isn't the reconciled object's child: one whose controller
  owner reference names another object, or that has only owner references
  that aren't a controller's.

### What it records

Nothing of its own: the events are `fault`'s, as
[Telemetry](/humus/pr-preview/pr-1210/failure-injection/#telemetry) on the failure
injection page lists them.

- `humus.fit.evaluation` is on each request's span, on the span in a watch's
  context, and wherever the end user evaluates a point of their own.
- `humus.fit.strip` is on a request's span, and on a reconcile's span for an
  annotation that is dropped. A strip has no attributes, with one exception:
  a reconcile's strip of an annotation that isn't W3C baggage has
  `error.type` and `error.message`. The controller parses the annotation
  itself, so only it can tell an annotation it can't read from an object it
  doesn't trust.
- Setting a child's annotation adds no event: the apply's span has its
  evaluation and its strip, and the child's reconciles show the scenario
  they carry.

An aborted request's span has the `error.type`
`github.com/z5labs/humus/fault.InjectedError`, as every client's has,
`otelhttp`'s `http.client.request.duration` counts it, and
`humus.kube.controller.reconcile.duration` counts the reconcile it fails
with the outcome `error`.

## Testing

**humus ships no fake API server, and no test package.** humus is neither
Kubernetes' client nor its server, so it doesn't write or keep a fake of the
API server: client-go's fakes have no admission, no validation, no
defaulting and no garbage collection, and can't apply an unstructured
object, so a test on them passes where the cluster would reject. So a
controller is tested in three parts:

- **its Reconciler, as a function,** handed a plain `Object`;
- **its CRD,** with [the golden test](#the-golden-test);
- **the whole App, against k3s in a container,** in real time.

The third departs from the charter's principle 6, which runs every App whole
inside `synctest` on a test Host, as RabbitMQ's tests do, and the charter's
[revision 10](https://github.com/z5labs/humus/discussions/629#discussioncomment-19009924)
records the exception.
[Testing a humus App](/humus/pr-preview/pr-1210/testing/#runtime-packages) says the
same for every runtime package.

### The Reconciler as a function

A Reconciler is handed a plain `Object`, so a test builds one. Its children
are applied through `kubefake.NewClientset()`, client-go's
`k8s.io/client-go/kubernetes/fake`, whose field-managed tracker applies
typed objects, with their managed fields, as the API server does:

```go
// cacheObject is the Memcached default/cache, as a test of the Reconciler
// builds it.
func cacheObject() kube.Object[MemcachedSpec, MemcachedStatus] {
	return kube.Object[MemcachedSpec, MemcachedStatus]{
		TypeMeta:   metav1.TypeMeta{APIVersion: "cache.example.com/v1alpha1", Kind: "Memcached"},
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "default", UID: "cache-uid", Generation: 1},
		Spec:       MemcachedSpec{Size: 2, Image: "memcached:1.6.26-alpine3.19"},
	}
}

func TestReconciler_applies_a_Deployment_the_Memcached_controls(t *testing.T) {
	kc := kubefake.NewClientset()
	m := cacheObject()

	res, err := Reconciler{Kube: kc}.Reconcile(t.Context(), m)
	require.NoError(t, err)

	d, err := kc.AppsV1().Deployments("default").Get(t.Context(), "cache", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, int32(2), *d.Spec.Replicas)
	require.Equal(t, "memcached:1.6.26-alpine3.19", d.Spec.Template.Spec.Containers[0].Image)
	owner := metav1.GetControllerOf(d)
	require.NotNil(t, owner)
	require.Equal(t, m.UID, owner.UID, "the Memcached controls the Deployment")

	require.NotNil(t, res.Status)
	c := meta.FindStatusCondition(res.Status.Conditions, "Available")
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionUnknown, c.Status, "until the Deployment controller has written its own")
}
```

No Deployment controller runs on the fake, so a test plays its part: it
writes the Deployment's status with `UpdateStatus`, and reconciles again to
see the condition copied. A reactor makes an apply fail, to see the failure
reported as a condition and an error together:

```go
func TestReconciler_reports_a_failed_apply_as_a_condition_and_an_error(t *testing.T) {
	kc := kubefake.NewClientset()
	kc.PrependReactor("patch", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("admission webhook denied the request")
	})

	res, err := Reconciler{Kube: kc}.Reconcile(t.Context(), cacheObject())
	require.ErrorContains(t, err, "admission webhook denied the request", "the error retries the Memcached")
	require.NotNil(t, res.Status, "the status is applied with the error")
	c := meta.FindStatusCondition(res.Status.Conditions, "Available")
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionUnknown, c.Status)
	require.Equal(t, "ApplyFailed", c.Reason)
	require.Contains(t, c.Message, "admission webhook denied the request")
}
```

`clienttesting` is `k8s.io/client-go/testing`. A `KindReconciler` is tested
the same way, handed a `corev1.Namespace`. A `Finalize`'s function is a
plain function too, called with an `Object` whose `DeletionTimestamp` is
set.

`inPod` and `fromKubeconfig` run inside `synctest`, against an API server of
the test's own on humustest's Host, with a CA from `humustest.NewCA`: the
service account's files written with `humustest.WithFiles`, the token file
rewritten to show a rotation picked up within a minute, and a kubeconfig
written with `clientcmd.Write`. `Init` runs inside `synctest` as far as it
goes without a cluster: with its variables unset it exits 78, naming each
one.

A controller under [failure injection](#failure-injection) runs inside
`synctest` on the fakes too, with a Setup whose `Faults` comes from
`fault.New`. Its test hands `underTest` a Reconciler of its own, which
evaluates `kube:deployments.apps:patch` with `s.Faults.Eval`, and starts
and ends the experiment with `setScenario` on the fake dynamic client.
client-go's fakes never see a request's context, so the points of the
Client's own requests, and the annotation carried on to children, need the
real API server below.

### The whole App, against k3s

**The whole App runs against k3s in a container.** Its test runs the App
with `humustest.Start` and `humus.WithHost(humus.OSHost())`, so the Client
dials the container through the real network, in real time, and the App
still reads its environment from humustest's process. testcontainers-go's
k3s module starts `rancher/k3s` as `server --disable-agent`: the API server
and the controller manager without a kubelet, so no pod runs, but the
garbage collector does. Rootless Podman can't run k3s's agent, since it
doesn't delegate the cpuset controller.

The test fills the Connection from the kubeconfig k3s writes, as
[`fromKubeconfig`](#from-a-kubeconfig-for-local-development) does, creates
the CRD from `Memcached.CRD()`, as whatever deploys the controller would,
and waits for it to be established:

```go
ext, err := apiextensions.NewForConfig(client.Config())
require.NoError(t, err)
crds := ext.ApiextensionsV1().CustomResourceDefinitions()
_, err = crds.Create(ctx, crd, metav1.CreateOptions{FieldValidation: "Strict"})
require.NoError(t, err)
t.Cleanup(func() {
	_ = crds.Delete(context.Background(), crd.Name, metav1.DeleteOptions{})
})
```

That is the test that the API server accepts the reflected CRD, compiles its
CEL rules and enforces its constraints, which `CRD()` leaves to it. The App
under test is an `Init` of the same shape as the end user's:

```go
return humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
	client, err := kube.NewClient(s, cluster.conn)
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(client.Config())
	if err != nil {
		return nil, err
	}
	ctrl, err := kube.NewController(s, client, controllerSettings(), r, deployer{kube: kc}, parts...)
	if err != nil {
		return nil, err
	}
	return []humus.Service{ctrl}, nil
}, humustest.WithApp(humus.WithHost(humus.OSHost())))
```

It creates a Memcached, waits for its status's `Available` condition, checks
that the Deployment is applied under `Settings.Name` with the Memcached as
its controller, and that the replica holds the Lease. Then it sends SIGTERM,
and checks the App exits 0 and leaves the Lease without a holder:

```go
p.Signal(syscall.SIGTERM)
st := appExit(t, status)
require.Zero(t, st.Code(), "cause: %v", st.Cause())

lease, err = kc.CoordinationV1().Leases("default").Get(ctx, controllerName, metav1.GetOptions{})
require.NoError(t, err)
require.Empty(t, lease.Spec.HolderIdentity, "the Lease is left without a holder")
```

`kube`'s own tests against k3s also undo a hand-made scale of a Deployment
the Memcached owns, run its finalizer when it is deleted, exit 1 when the
CRD isn't installed, and give every Namespace a ConfigMap with a Kind
controller. Under [failure injection](#failure-injection), they annotate a
Memcached with a scenario that aborts its Deployment's apply, or its
finalizer's, and see it retried until the annotation is removed. They see
the annotation carried on to the Deployment and taken off it again, and an
operator of two controllers carry a scenario down: only a real API server
sees a request's context, and removes what an apply stops declaring. A
Deployment's pods never run without a kubelet, which is why
`kube`'s own test Reconciler reports its condition from the apply alone.

### Behind a build tag, in a CI job of its own

**Tests that start a container sit behind a build tag,** so `go test ./...`
runs the unit tests alone, with no Docker. `kube`'s own run behind
`testcontainers`, starting with:

```
//go:build testcontainers
```

and run in a CI job of their own, "Kubernetes (testcontainers)":

```
go -C kube test -race -tags testcontainers ./...
```

The job isn't a required check, and the coverage gate counts the unit tests
alone: a container test is slower, and depends on Docker and on pulling an
image, so it informs a review rather than blocking one. The build job vets
the tagged tests without starting a container, so they keep compiling. An
end user's own container tests fit the same shape, as
[RabbitMQ's](/humus/pr-preview/pr-1210/rabbitmq/#behind-a-build-tag-in-a-ci-job-of-its-own)
do.

## Migrating from kubebuilder

An end user moving from kubebuilder and controller-runtime maps its pieces
like this:

| kubebuilder and controller-runtime | `kube` |
|---|---|
| `kubebuilder init`, `create api`: 54 files in 20 directories | the module, and the end user's own files |
| `// +kubebuilder:validation:Minimum=1` and other markers | `kube.Field(&spec.Size).Minimum(1)`, in `Resource.Rules` |
| doc comments as descriptions | `.Description(…)` |
| `// +kubebuilder:object:root=true`, `zz_generated.deepcopy.go`, `make generate` | nothing: `kube.Object[Spec, Status]` |
| `make manifests` | `Resource.CRD()`, and a golden test |
| `AddToScheme`, and a scheme in `main` | nothing |
| `Reconcile(ctx, req)`, then `r.Get` and `IsNotFound` | `Reconcile(ctx, obj)` |
| `r.Status().Update(ctx, obj)`, then `r.Get` again | `kube.Result{Status: &st}` |
| `Result{Requeue: true}`, `RequeueAfter`, a returned error | a returned error, or `Result.After` |
| `controllerutil.AddFinalizer`, `r.Update`, `RemoveFinalizer` | `kube.Finalize(name, f)` |
| `Owns(&appsv1.Deployment{})` | `kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments"))` |
| `For(&corev1.Namespace{})` | `kube.Kind[corev1.Namespace]` and `kube.NewKindController` |
| `Watches(…, handler.EnqueueRequestsFromMapFunc(…))` | `Result.After`, polling |
| `ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{…})` | `kube.NewClient` and `kube.NewController`, in `Init` |
| `ctrl.SetupSignalHandler()`, `mgr.Start` | `humus.Main` |
| `LeaderElection: true`, `LeaderElectionID` | always, named `Settings.Name` |
| `HealthProbeBindAddress`, `AddReadyzCheck` | `ctrl.Synced()`, served with health probes |
| `metrics.Registry.MustRegister` | the `MeterProvider` in `s.Telemetry` |
| `log.SetLogger(zap.New(…))` | the `LoggerProvider` in `s.Telemetry` |
| envtest | k3s in a container |
| `// +kubebuilder:rbac:…` | the rules, written by the end user, as [RBAC](#rbac) lists |
| `// +kubebuilder:webhook:…`, cert-manager | an `openapi` API on `httpserver` |

Three behaviours change for such a controller:

- **A panic is no longer retried.** controller-runtime recovers it and
  retries with backoff; `kube` records it and waits for a change, as
  [Panics](#panics) describes.
- **Losing the Lease exits 1, and so does a missing CRD,** at startup, so
  the pod's restarts show either.
- **Out of scope:** several versions of one resource and conversion between
  them, printer columns, short names, categories and the scale subresource,
  typed clientsets, listers and informers for a custom resource, exec
  credential plugins, and installing CRDs.
