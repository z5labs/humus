// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package kube writes Kubernetes controllers as humus Services, with
// client-go, Kubernetes' own client, and no controller-runtime. It reaches
// an API server with humus's lifecycle, declares custom resources, whose
// CRDs it reflects from Go types, and reconciles their objects, or those of
// Kubernetes' own kinds.
//
// It is in the nested module github.com/z5labs/humus/kube, so client-go and
// apimachinery stay out of the module graph of end users who don't talk to
// Kubernetes. The module is always built against the core module in the
// same commit.
//
// # The Client
//
// A [Client] reaches one API server through the App's Host, from a
// [Connection] the end user's Init fills. [NewClient] builds it, and opens
// no connection:
//
//	client, err := kube.NewClient(s, conn)
//	if err != nil {
//		return nil, err
//	}
//	kc, err := kubernetes.NewForConfig(client.Config())
//	if err != nil {
//		return nil, err
//	}
//
// [Client.Config] returns a rest.Config for client-go's own clients: the
// clientset kubernetes.NewForConfig returns, and the dynamic and metadata
// clients. Every one of them reaches the API server through the Client's
// transport, which:
//
//   - dials with s.Host.Dial;
//   - runs TLS with a clone of Connection.TLS, whose ServerName defaults to
//     Connection.Server's host;
//   - uses no proxy, whatever the environment says;
//   - over HTTP/2, sends a ping after 30s without a frame, and gives up on
//     the connection when 15s pass without the answer, so a watch on a dead
//     connection doesn't wait for TCP to give up;
//   - when Connection.Token isn't nil, adds its bearer token to every
//     request;
//   - traces and measures every request with otelhttp, on s.Telemetry's
//     providers and propagator, except a watch, which lasts as long as an
//     informer runs and has no span;
//   - makes every request an injection point of s.Faults, strips the
//     failure scenario from every request, and carries a reconcile's
//     scenario on to the children it applies, as Failure injection, below,
//     says;
//   - doesn't limit the client's rate. client-go's default is 5 requests a
//     second, and the API server's priority and fairness limits each client
//     itself.
//
// # The Connection
//
// The end user fills the Connection. humus reads no KUBERNETES_SERVICE_HOST,
// no token file and no kubeconfig, so a test fills it with whatever its
// cluster needs, and nothing reads the real environment or disk behind the
// end user's back. The package reads no environment variable, reads no
// file, and defines no variable's name.
//
// In a pod, the kubelet sets KUBERNETES_SERVICE_HOST and
// KUBERNETES_SERVICE_PORT in every container, and mounts the service
// account's CA and token in /var/run/secrets/kubernetes.io/serviceaccount/.
// The end user's Init sources Server from the two variables, and reads the
// CA and the token through s.Host.FS:
//
//	const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount/"
//
//	ca, err := readFile(s.Host.FS, serviceAccount+"ca.crt") // the end user's helper
//	if err != nil {
//		return nil, err
//	}
//	roots := x509.NewCertPool()
//	roots.AppendCertsFromPEM(ca)
//
//	conn, err := config.Struct(func(r *config.Resolver) kube.Connection {
//		return kube.Connection{
//			// JoinHostPort brackets an IPv6 host.
//			Server: net.JoinHostPort(r.Get(env("KUBERNETES_SERVICE_HOST")), r.Get(env("KUBERNETES_SERVICE_PORT"))),
//			TLS:    &tls.Config{RootCAs: roots},
//			Token: func(context.Context) (string, error) {
//				b, err := readFile(s.Host.FS, serviceAccount+"token")
//				return string(bytes.TrimSpace(b)), err
//			},
//		}
//	}).Read(ctx)
//
// The Client calls Token at most once a minute, however many requests it
// sends, so a token the kubelet rotates is picked up within a minute. When a
// call fails, the Client keeps the last token, which a rotated service
// account token outlives, and the request goes on. Until a call has
// returned a token there is none to keep, so each request calls Token, and
// fails with its error. A call that returns an empty token has failed, so
// no request carries a bare "Bearer". A nil Token sends no Authorization header, for a
// client that authenticates with a certificate in Connection.TLS.
//
// For local development, the end user fills the Connection from a
// kubeconfig's embedded data, as kind and k3s write it, with client-go's
// clientcmd.Load. The end user reads the file, through s.Host.FS:
//
//	b, err := readFile(s.Host.FS, path)
//	if err != nil {
//		return kube.Connection{}, err
//	}
//	kc, err := clientcmd.Load(b)
//	if err != nil {
//		return kube.Connection{}, err
//	}
//	current := kc.Contexts[kc.CurrentContext]
//	cluster, user := kc.Clusters[current.Cluster], kc.AuthInfos[current.AuthInfo]
//	u, err := url.Parse(cluster.Server)
//	if err != nil {
//		return kube.Connection{}, err
//	}
//	roots := x509.NewCertPool()
//	roots.AppendCertsFromPEM(cluster.CertificateAuthorityData)
//	cert, err := tls.X509KeyPair(user.ClientCertificateData, user.ClientKeyData)
//	if err != nil {
//		return kube.Connection{}, err
//	}
//	return kube.Connection{
//		Server: u.Host,
//		TLS:    &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{cert}},
//	}, nil
//
// humus has no helper for this. A kubeconfig's exec plugins and auth
// providers grow with every cloud, and a Connection is three fields.
// [CheckServer] and [CheckTLS] check the two required ones, and NewClient
// reports every bad field together.
//
// # Settings
//
// [Settings] holds what configures a controller: its name, the namespace
// it watches, how many objects it reconciles at once, how often it
// resyncs, how it backs off an object whose reconcile failed, and the
// Lease its replicas elect their leader with, in its [LeaderElection].
// Every field is a plain value, [DefaultSettings] gives the package's
// defaults, and each checked field has a check that reports what is wrong
// with a value without naming the field. The package reads no environment
// variable: the end user sources the settings in their Init, with whatever
// variable names they choose, and runs each field's check as they do:
//
//	d := kube.DefaultSettings()
//	settings, err := config.Struct(func(r *config.Resolver) kube.Settings {
//		k := d
//		// No .Default: the name, and the Lease's namespace and identity, are required.
//		k.Name = r.Get(env("CONTROLLER_NAME").Validate(kube.CheckName))
//		k.Workers = r.Get(env("CONTROLLER_WORKERS").Parse(strconv.Atoi).Default(d.Workers).Validate(kube.CheckWorkers))
//		k.LeaderElection.Namespace = r.Get(env("POD_NAMESPACE").Validate(kube.CheckNamespace))
//		k.LeaderElection.Identity = r.Get(env("POD_NAME").Validate(kube.CheckIdentity))
//		return k
//	}).Read(ctx)
//
// Here env is the end user's own helper, which names a variable. A
// variable that isn't set fails as not found, and a value that fails its
// check fails Init, naming the variable, so the process exits 78.
//
// Name, LeaderElection.Namespace and LeaderElection.Identity have no
// default, so the end user always sources them, with no .Default in their
// chains. DefaultSettings leaves them "", which [CheckName],
// [CheckNamespace] and [CheckIdentity] reject. humus imposes no name and no
// namespace, as it imposes no port.
//
// In a pod, the Lease's Namespace and Identity come from POD_NAMESPACE and
// POD_NAME, as above, which the pod's spec sets from the downward API:
//
//	env:
//	  - name: POD_NAMESPACE
//	    valueFrom:
//	      fieldRef:
//	        fieldPath: metadata.namespace
//	  - name: POD_NAME
//	    valueFrom:
//	      fieldRef:
//	        fieldPath: metadata.name
//
// Each replica's pod has a name of its own, so each has an identity of its
// own in the Lease.
//
// The defaults are Kubernetes' own. RetryBaseDelay's 5ms and
// RetryMaxDelay's 1000s are the per-object backoff of client-go's
// DefaultTypedControllerRateLimiter. The Lease's 15s LeaseDuration, 10s
// RenewDeadline and 2s RetryPeriod are kube-controller-manager's defaults.
//
// Some bounds take more than one field to tell, so no one field's check
// has them: RetryMaxDelay is at least RetryBaseDelay, LeaseDuration is
// longer than RenewDeadline, and RenewDeadline is longer than 1.2
// RetryPeriods. The defaults keep all three.
//
// # Custom resources
//
// A [Resource] declares a custom resource as Go values: its group, version,
// kind, plural and scope, with its spec and status as plain Go structs. The
// end user writes no markers and generates no code:
//
//	type MemcachedSpec struct {
//		Size  int32  `json:"size"`
//		Image string `json:"image"`
//	}
//
//	type MemcachedStatus struct {
//		ReadyReplicas int32              `json:"readyReplicas,omitempty"`
//		Conditions    []metav1.Condition `json:"conditions,omitempty"`
//	}
//
//	var Memcached = kube.Resource[MemcachedSpec, MemcachedStatus]{
//		Group:   "cache.example.com",
//		Version: "v1alpha1",
//		Kind:    "Memcached",
//		Plural:  "memcacheds",
//		Scope:   kube.Namespaced,
//	}
//
// Its Rules, under Rules below, constrain the fields.
//
// One of its objects is an [Object], and [Resource.GVR] names the resource
// to client-go's dynamic client.
//
// [Resource.CRD] reflects the resource's CustomResourceDefinition from Spec
// and Status, as encoding/json encodes them, into a structural schema:
//
//	Go                                        schema
//	bool                                      type: boolean
//	int8 to int32, uint8, uint16              type: integer, format: int32
//	int, int64, uint, uint32, uint64          type: integer, format: int64
//	float32, float64                          type: number
//	string                                    type: string
//	[]byte                                    type: string, format: byte
//	a slice or an array                       type: array, with items
//	map[string]V                              type: object, with additionalProperties
//	a struct                                  type: object, with a property for each field
//	*T                                        T's schema, nullable unless the field is omitempty or omitzero
//	metav1.Time, metav1.MicroTime             type: string, format: date-time
//	metav1.Duration                           type: string
//	resource.Quantity, intstr.IntOrString     x-kubernetes-int-or-string, anyOf integer and string
//	runtime.RawExtension                      type: object, x-kubernetes-preserve-unknown-fields
//	metav1.ObjectMeta, in a template          type: object
//
// A struct has a property for each field encoding/json writes, named by its
// json tag, or by the field's name when the tag gives none. An embedded
// struct with no name in its tag is inlined, as encoding/json does, and a
// field with the string option is a string. A field is required unless its
// json tag has omitempty or omitzero, or it has a Default: the encoding
// decides it, since a field the encoder always writes is always there. A
// field promoted from an embedded pointer isn't required either, since a
// nil pointer leaves it out.
//
// The CRD's root has apiVersion, kind and metadata, a required spec, and
// status. It has one version, served and stored, with the status
// subresource.
//
// CRD returns one error that names every mistake it finds, each where in
// the object it is: a missing Group, Version, Kind or Plural, a Scope that
// is neither Namespaced nor Cluster, a type that encodes itself with a
// MarshalJSON or MarshalText method and has no Schema, a map whose keys
// aren't strings, an interface, a channel, a function, a type that
// contains itself, which a schema without $ref can't express, and every
// mistake in the resource's rules, below.
//
// # Rules
//
// [Resource.Rules] constrain the fields of Spec, of Status and of the types
// they hold, with typed values attached to each field by pointer:
//
//	var (
//		spec   MemcachedSpec
//		status MemcachedStatus
//
//		Memcached = kube.Resource[MemcachedSpec, MemcachedStatus]{
//			Group:   "cache.example.com",
//			Version: "v1alpha1",
//			Kind:    "Memcached",
//			Plural:  "memcacheds",
//			Scope:   kube.Namespaced,
//			Rules: []kube.TypeRules{
//				kube.Rules(&spec,
//					kube.Field(&spec.Size).Minimum(1).Maximum(3).Description("How many memcached pods run."),
//					kube.Field(&spec.Image).Pattern(regexp.MustCompile(`^memcached:`)).Default("memcached:1.6.26-alpine3.19"),
//				),
//				kube.Rules(&status,
//					kube.Field(&status.Conditions).ListMap("type"),
//				),
//			},
//		}
//	)
//
// [Rules] constrains the fields of one type, each named by a [Field] of a
// pointer into a value of the type. Each constraint is a method of
// [FieldOf], whose value has the field's type, so a misspelled constraint,
// or a value of the wrong type, doesn't compile:
//
//	kube.Field(&spec.Size).Minimun(1)       // kube.FieldOf[int32] has no field or method Minimun
//	kube.Field(&spec.Size).Minimum("one")   // cannot use "one" (untyped string constant) as int32 value
//	kube.Field(&spec.Image).Default(3)      // cannot use 3 (untyped int constant) as string value
//
// Each constraint becomes its counterpart on the field's schema:
//
//	FieldOf method             schema
//	Description                description
//	Default                    default, and the field is optional
//	Enum                       enum
//	Minimum, Maximum           minimum, maximum, of a number
//	MinLength, MaxLength       minLength, maxLength, of a string
//	Pattern                    pattern, of a string
//	MinItems, MaxItems         minItems, maxItems, of a list
//	ListSet                    x-kubernetes-list-type: set
//	ListMap                    x-kubernetes-list-type: map, and x-kubernetes-list-map-keys
//	Validation                 a CEL rule and its message, in x-kubernetes-validations
//
// [TypeRules.Description] and [TypeRules.Validation] describe and constrain
// the type's own schema, and [Schema] gives a type's schema whole: it is
// how a type that encodes itself, with a MarshalJSON or MarshalText method,
// gets a schema at all.
//
// Rules belong to a type, not to a place in an object. Rules of a type
// constrain its fields, those promoted from embedded structs included,
// wherever the type appears in Spec or Status, so a type such as a port or
// a template is constrained once, and constrained alike everywhere. A field
// of a nested struct, such as &spec.Template.Port, is constrained by that
// struct's own Rules, and a rule on it in the outer type's Rules is a
// mistake.
//
// A description is a value, not the field's doc comment: the schema is
// reflected from the types at run time, when doc comments are gone, so
// kubectl explain shows only what Description gives.
//
// Rules needs a value of the type for its Fields to point into, usually a
// package-level variable, as spec and status are above. Rules reads
// nothing from the value: it only finds which field each pointer names.
//
// CRD reports every mistake in the rules with the rest: a constraint that
// doesn't fit its field's type, such as Pattern on a number; a Field that
// names no field of the value given to Rules, or a nested struct's field;
// and two Rules, or two Schemas, for one type. It doesn't compile CEL: the
// API server compiles every rule when the CRD is applied, which the end
// user's tests against a cluster do.
//
// The end user checks the CRD's YAML into their repository, and a golden
// test keeps it true to the types. sigs.k8s.io/yaml writes it, and the
// test rewrites it when it runs with -update:
//
//	var update = flag.Bool("update", false, "rewrite the golden files")
//
//	func TestMemcachedCRD(t *testing.T) {
//		crd, err := app.Memcached.CRD()
//		require.NoError(t, err)
//		got, err := yaml.Marshal(crd) // sigs.k8s.io/yaml
//		require.NoError(t, err)
//
//		const path = "deploy/crd/memcacheds.yaml"
//		if *update {
//			require.NoError(t, os.WriteFile(path, got, 0o644))
//		}
//		want, err := os.ReadFile(path)
//		require.NoError(t, err)
//		require.Equal(t, string(want), string(got))
//	}
//
// The CRD carries its own apiVersion and kind, so the YAML applies with
// kubectl or Helm as it stands.
//
// The controller never installs its CRD. Installing it would give the
// controller's ServiceAccount write access to every CRD in the cluster, and
// a CRD outlives the controller's pods. Whatever deploys the controller,
// such as its Helm chart or its kubectl apply, applies the YAML.
//
// # The Controller
//
// A [Controller] reconciles the objects of one Resource, or of one [Kind]
// (see Kubernetes' own kinds). [NewController] builds it, and opens no
// connection; it is a humus.Service:
//
//	ctrl, err := kube.NewController(s, client, settings, Memcached, Reconciler{Kube: kc})
//	if err != nil {
//		return nil, err
//	}
//	return []humus.Service{ctrl}, nil
//
// NewController returns one error that names every mistake it finds: each
// field of the Settings that fails its check, the bounds between them, each
// mistake in the Resource, as [Resource.CRD] reports it, a Settings.Namespace
// for a cluster-scoped Resource, an [Owns] with no Version or no Resource,
// a [Finalize] whose name is empty, whose f is nil or whose f takes another
// type than the Controller's Object, a second Finalize, a [TrustScenario]
// whose annotation isn't a valid annotation key or whose trust is nil, a
// second TrustScenario, and a nil Reconciler.
//
// A [Reconciler] is handed its object, and returns what the object's status
// is now:
//
//	type Reconciler struct{ Kube kubernetes.Interface }
//
//	func (r Reconciler) Reconcile(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
//		d, err := r.Kube.AppsV1().Deployments(m.Namespace).Apply(ctx, deployment(m), metav1.ApplyOptions{FieldManager: "memcached-controller", Force: true})
//		st := m.Status
//		if err != nil {
//			meta.SetStatusCondition(&st.Conditions, metav1.Condition{
//				Type: "Available", Status: metav1.ConditionUnknown, Reason: "ApplyFailed",
//				Message: err.Error(), ObservedGeneration: m.Generation,
//			})
//			return kube.Result[MemcachedStatus]{Status: &st}, err
//		}
//		st.ReadyReplicas = d.Status.ReadyReplicas
//		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
//			Type: "Available", Status: metav1.ConditionTrue, Reason: "DeploymentApplied",
//			ObservedGeneration: m.Generation,
//		})
//		return kube.Result[MemcachedStatus]{Status: &st}, nil
//	}
//
// The object comes from the controller's cache, decoded into a new
// [Object], so it is the Reconciler's own copy, and there is no Get and no
// not-found to handle: an object gone from the cache is never reconciled,
// and what it owned is garbage collected through its owner references. An
// object being deleted is left alone, unless a [Finalize] holds it (see
// Finalizers).
//
// When [Result.Status] isn't nil and its JSON encoding differs from the
// object's status, the controller applies it to the status subresource
// with server-side apply, under the field manager Settings.Name, forcing,
// since the controller owns the status whole. So a status write never fails
// with "the object has been modified", and needs no Get after it. It is
// applied whether or not Reconcile returned an error, so a failure can be a
// condition and an error together: the condition is what kubectl get shows,
// and the error retries the object. A nil Status leaves the status alone,
// so an error path that returns a zero Result can't wipe the conditions.
//
// Each reconcile settles its object one way:
//   - a panic is recovered, and never retried (see Panics);
//   - an error retries it after a backoff of its own, from RetryBaseDelay,
//     doubling to RetryMaxDelay, under an overall limit of 10 retries a
//     second, bursting to 100. The error never ends the Service;
//   - otherwise, a positive [Result.After] reconciles it again after that
//     long, unless it changes first;
//   - otherwise it is reconciled again when it changes, or a resource it
//     owns changes, or when the resync comes, every Settings.Resync.
//
// Reconciles are level-triggered: a Reconciler isn't told what changed,
// only handed the object as it is now, and brings the cluster to what it
// declares. Applying a status changes the object, so it is reconciled once
// more, returns the same status, which isn't applied, and settles: two
// reconciles in all. Settings.Workers objects are reconciled at once, and
// one object is never reconciled twice at once.
//
// # Panics
//
// A panic in Reconcile, in a [Finalize]'s f, or in a [TrustScenario]'s
// trust, is recovered and recorded (see Telemetry), but never retried. A panic is a mistake in the
// controller, such as a nil pointer, an index out of range or a division by
// zero: it shows the code's assumptions about the cluster are wrong, so a
// retry would only panic again, and fill the logs and traces with the same
// bug once per backoff. So the object is settled as one that reconciled
// with no [Result.After] is: no status is applied, since there is no
// Result, and it is reconciled again only when it, or a resource it owns,
// changes, when the resync comes, or when a new leader takes over. A change
// made while the panicked reconcile ran reconciles it again once that
// reconcile is done, as after any reconcile. The rollout that ships the fix
// starts a new leader, which reconciles every object, the panicked ones
// with the fixed code.
//
// The panic never ends the Service, and the other objects go on being
// reconciled. A panic of f leaves the object with its finalizer, so its
// deletion waits for its next reconcile.
//
// This differs from controller-runtime, which turns a panic into an error
// and retries it with backoff.
//
// # Owned resources
//
// [Owns] reconciles an object again whenever a resource it controls changes
// or is deleted, such as the Deployment its Reconciler applies:
//
//	ctrl, err := kube.NewController(s, client, settings, Memcached, Reconciler{Kube: kc},
//		kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments")),
//	)
//
// So a hand-made change to the Deployment is undone, and a change in its
// status reaches the Memcached's status, with no polling. When a resource of
// an Owns is added, changed or deleted, and its owner reference with
// controller: true names the Resource's group and kind, the object that
// reference names is reconciled. An owner reference without controller:
// true, one that names another group or kind, and a resource with no owner
// reference reconcile nothing. The controller watches each owned resource
// through the metadata client, since it looks only at their owner
// references, limited to Settings.Namespace.
//
// The end user sets the controller owner reference when they apply the
// resource, with client-go's apply configurations:
//
//	d, err := r.Kube.AppsV1().Deployments(m.Namespace).Apply(ctx,
//		appsv1ac.Deployment(m.Name, m.Namespace).
//			WithOwnerReferences(metav1ac.OwnerReference().
//				WithAPIVersion(m.APIVersion).WithKind(m.Kind).WithName(m.Name).WithUID(m.UID).
//				WithController(true).WithBlockOwnerDeletion(true)).
//			WithSpec(spec(m)),
//		metav1.ApplyOptions{FieldManager: "memcached-controller", Force: true},
//	)
//
// A resource the controller doesn't own isn't mapped to its objects. A
// Reconciler that depends on one polls it, with [Result.After].
//
// # Finalizers
//
// [Finalize] keeps each object until the end user's code has cleaned up
// after it, such as something outside the cluster that owner references
// can't garbage collect:
//
//	ctrl, err := kube.NewController(s, client, settings, Memcached, Reconciler{Kube: kc},
//		kube.Finalize("cache.example.com/finalizer", func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) error {
//			return deleteBackups(ctx, m.Namespace, m.Name)
//		}),
//	)
//
// The object's type is inferred from f, and NewController checks it is the
// Controller's Object[Spec, Status].
//
// The controller adds the finalizer to an object's metadata.finalizers
// before its first reconcile: the object's first pass adds it, and the
// change that makes reconciles the object, with the finalizer on it. Once
// the object is being deleted, and the finalizer still holds it, the
// controller calls f, handed the object as the Reconciler is, and never the
// Reconciler. An error from f retries the object with backoff, as a
// reconcile's does, so f may be called again for an object it has already
// cleaned up after. A panic of f is recovered and never retried, as a
// Reconciler's is (see Panics): the finalizer stays, and the deletion waits
// for the object's next reconcile. Once f returns nil, the controller
// removes the finalizer, and the API server completes the deletion, unless
// another finalizer still holds the object. An object being deleted that
// the finalizer doesn't hold, as one deleted before its first reconcile, is
// left alone.
//
// The finalizer is applied, not updated: both adding it and removing it are
// server-side applies of the object's metadata, under the field manager
// Settings.Name, which name its uid. So they never conflict with other
// writers of the object's metadata, need no Get first, and never create an
// object that has been deleted since the cache saw it. Removing it is an
// apply of the metadata without it, which the API server removes since no
// other field manager set it. When another field manager set it too, such
// as a kubectl edit that added it by hand, it stays on the object: the
// controller then fails the reconcile with an error that says so, and
// retries, calling f again, until that field manager's finalizer is
// removed. So a Reconciler that applies its own object,
// rather than its status, does so under a field manager other than
// Settings.Name: the controller's applies of the metadata would remove the
// fields it applied under Settings.Name.
//
// # Kubernetes' own kinds
//
// A [Kind] is a kind whose Go type is defined elsewhere, such as one of
// Kubernetes' own, and whose status belongs to whoever defines it.
// [NewKindController] reconciles its objects, with the same [Controller]:
//
//	var Namespaces = kube.Kind[corev1.Namespace]{
//		GVR:   corev1.SchemeGroupVersion.WithResource("namespaces"),
//		Kind:  "Namespace",
//		Scope: kube.Cluster,
//	}
//
//	type DefaultDeny struct{ Kube kubernetes.Interface }
//
//	func (r DefaultDeny) Reconcile(ctx context.Context, ns corev1.Namespace) (time.Duration, error) {
//		_, err := r.Kube.NetworkingV1().NetworkPolicies(ns.Name).Apply(ctx, policy(ns), metav1.ApplyOptions{FieldManager: "default-deny", Force: true})
//		return 0, err
//	}
//
//	ctrl, err := kube.NewKindController(s, client, settings, Namespaces, DefaultDeny{Kube: kc},
//		kube.Owns(networkingv1.SchemeGroupVersion.WithResource("networkpolicies")),
//	)
//
// A Kind has no CRD and no Rules: the kind's own definition is the API
// server's. Its GVR's Group is "" for the core group. Its objects are
// reconciled as a Resource's are, except:
//   - a [KindReconciler] is handed client-go's own struct, decoded from the
//     cache into a new T, the Reconciler's own copy, as an Object is;
//   - no status is applied, since Kubernetes' own controllers write it. The
//     KindReconciler returns only when to reconcile the object again: a
//     positive after does so after that long, unless it changes first, and
//     zero waits for a change, or for the resync;
//   - an [Owns] matches the owner references that name the Kind's
//     GVR.Group and Kind. A namespaced resource can name a cluster-scoped
//     owner, such as its Namespace.
//
// [Finalize], the Lease, shutdown, panics, telemetry and Synced work as
// they do for a Resource, with the span named "reconcile <Kind>". A
// Finalize's f takes a T, which NewKindController checks.
//
// NewKindController returns one error that names every mistake it finds:
// each mistake in the Settings and in the parts, as NewController reports
// them, a GVR without a Version or a Resource, a missing Kind, a Scope that
// is neither Namespaced nor Cluster, a T whose pointer isn't a
// metav1.Object, a Settings.Namespace for a cluster-scoped Kind, and a nil
// KindReconciler.
//
// # Serving
//
// [Controller.Serve] first lists the resource, and each resource an Owns
// names, once, with a limit of 1. An error, such as a CRD that isn't
// installed, ends Serve with an error that names the resource, so the App
// exits 1, rather than leave a controller that waits forever. It then starts
// an informer on the resource, and one on each owned resource, limited to
// Settings.Namespace, waits for their caches to sync, and campaigns for the
// Lease.
//
// Every controller elects a leader, on a Lease named Settings.Name, in
// LeaderElection.Namespace, as LeaderElection.Identity. There is no option
// to turn it off: two replicas without it would apply the same objects
// twice. Every replica keeps its cache warm, and only the Lease's holder
// reconciles, so a new leader starts at once. When the Lease's renewal
// fails for RenewDeadline, or another replica takes it, the controller
// stops taking keys, cancels the reconciles in flight, and Serve returns
// [ErrLostLeadership], so the App exits 1. Another replica may already be
// reconciling; the pod campaigns again when it restarts.
//
// On Serve's context, as when the App shuts down, the controller stops
// taking keys, and each worker finishes the reconcile it has in flight. The
// backlog is left for the next leader, which lists every object when its
// informer starts: the work queue is shut down, never drained. Then the
// controller releases the Lease, so the next replica takes it without
// waiting out LeaseDuration, stops its informers, and Serve returns nil, so
// the App exits 0. A reconcile's context is cancelled only when
// [Controller.Shutdown]'s context is done or the Lease is lost: then
// Shutdown cancels the reconciles in flight, waits for them, and returns an
// error that wraps the context's error. humus.Main's defaults, a 20s grace
// period and no drain, fit a controller.
//
// [Controller.Synced] is unhealthy until the caches have synced, and healthy
// from then until Serve returns, on every replica, leader or not, so a
// rollout isn't held up by the replicas waiting for the Lease. It is for
// readiness, served with the health probes:
//
//	health.Readiness(ctrl.Synced())
//
// # RBAC
//
// humus generates no RBAC. A Controller needs these rules itself, and the
// end user adds their Reconciler's own:
//   - get, list and watch on its resource, and patch on its status;
//   - get, create and update on Leases, in the Lease's namespace;
//   - list and watch on each resource an [Owns] names;
//   - patch on its resource, when a [Finalize] is given, which applies the
//     finalizer.
//
// A controller of a [Kind] needs get, list and watch on its kind, in place
// of the rules on its resource and its status, since it writes no status,
// and patch on it when a [Finalize] is given.
//
// # Telemetry
//
// A Controller reports through s.Telemetry, under the scope
// github.com/z5labs/humus/kube:
//   - every reconcile is a root span, "reconcile <Kind>", with
//     k8s.namespace.name for a namespaced object, and
//     humus.kube.object.name. Reconcile's context carries it. An error sets
//     its status to Error. With a [Finalize], adding the finalizer, and
//     calling f and removing the finalizer, are each a reconcile too, and f's
//     context carries the span;
//   - a failed reconcile, a failed f among them, is one WARN record,
//     "reconcile failed", in the span's context, with the object and the
//     error;
//   - a panic of Reconcile, of f, or of a TrustScenario's trust, is an
//     exception event on the span,
//     with exception.type, exception.message and exception.stacktrace, the
//     stack of the panic, and sets the span's status to Error. It is one
//     ERROR record, "reconcile panicked", in the span's context, with the
//     object and the same three attributes, and no "reconcile failed";
//   - client-go's own records, from the reflector, the work queue and
//     leader election, reach the LoggerProvider through a logger the
//     controller puts in their context. Those klog writes by default arrive:
//     an Info at verbosity 0, as an INFO record, and every Error, as an
//     ERROR record. Nothing reaches the real stderr;
//   - every API request is a client span from otelhttp, as the Client
//     traces them, and is measured by otelhttp's client instruments;
//   - on a request that carries a failure scenario, the injection point's
//     humus.fit.evaluation event and the strip's humus.fit.strip event are
//     on the request's span, or a watch's on the span in its context, as
//     Failure injection, below, says. An object's annotation whose scenario
//     a [TrustScenario] drops is a humus.fit.strip event on the reconcile's
//     span. Setting a child's annotation adds no event;
//   - the controller's own instruments measure its work queue and its
//     reconciles, from the MeterProvider. They are below.
//
// The controller's instruments:
//
//	humus.kube.controller.queue.depth                            Int64UpDownCounter  {object}
//	    Objects waiting in the work queue for a worker: +1 when a key is
//	    added, and -1 when a worker takes it.
//	humus.kube.controller.queue.adds                             Int64Counter        {object}
//	    Objects added to the work queue, not counting one already waiting.
//	humus.kube.controller.queue.duration                         Float64Histogram    s
//	    How long an object waited in the work queue for a worker. The wait
//	    of a Result.After or of a backoff comes before the key is added, so
//	    it isn't counted.
//	humus.kube.controller.reconcile.duration                     Float64Histogram    s
//	    How long each reconcile took, by its outcome: one for each
//	    "reconcile <Kind>" span, over the same time.
//	humus.kube.controller.reconcile.unfinished_duration          Float64Gauge        s
//	    How long the reconciles in flight have run, added together.
//	humus.kube.controller.reconcile.longest_unfinished_duration  Float64Gauge        s
//	    How long the longest reconcile in flight has run.
//
// Every data point carries humus.kube.controller.name, which is
// Settings.Name. reconcile.duration's also carry
// humus.kube.controller.reconcile.outcome:
//   - error, for a reconcile whose key is retried with backoff;
//   - panic, for a recovered panic;
//   - success, for everything else, a [Result.After] included.
//
// No data point carries an object's namespace or name: an object's
// reconciles are its spans. Both histograms take OpenTelemetry's advice for
// a duration in seconds, the boundaries 0.005, 0.01, 0.025, 0.05, 0.075,
// 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5 and 10. In Prometheus, they are
// humus_kube_controller_queue_depth, humus_kube_controller_queue_adds_total,
// humus_kube_controller_queue_duration_seconds,
// humus_kube_controller_reconcile_duration_seconds,
// humus_kube_controller_reconcile_unfinished_duration_seconds and
// humus_kube_controller_reconcile_longest_unfinished_duration_seconds.
//
// Every instrument but reconcile.duration is recorded by client-go's work
// queue, which is named Settings.Name, through a workqueue.MetricsProvider
// of the controller's own, set in the queue's config. The queue updates the
// two gauges every 500ms while it runs. Two of the queue's own metrics
// aren't recorded: its work duration, which times what reconcile.duration
// times without the outcome, and its retries, which count every AddAfter,
// so every Result.After would count as a retry. The error outcome counts
// the retries exactly.
//
// A standby records them too. It runs no workers, so nothing leaves its
// queue, and its queue.depth holds a key for every object. Until it takes
// the Lease, its reconcile.duration records nothing, and its two gauges
// read 0. So read queue.depth per replica, by its resource's attributes: a
// sum over the replicas counts the standbys' keys too.
//
// # Failure injection
//
// A reconcile carries the failure scenario its object's annotation holds,
// when the controller is given a [TrustScenario] and its trust trusts the
// object. The end user names the annotation, and decides trust with a func
// of the object:
//
//	const scenarioAnnotation = "acme.example.com/failure-scenario"
//
//	ctrl, err := kube.NewController(s, client, settings, Memcached, Reconciler{Kube: kc},
//		kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments")),
//		// Only the staging namespace runs experiments. An admission policy lets
//		// only the chaos harness's ServiceAccount set the annotation there.
//		kube.TrustScenario(scenarioAnnotation, func(obj metav1.Object) bool {
//			return obj.GetNamespace() == "staging"
//		}),
//	)
//
// The annotation's value is W3C baggage that carries a scenario as the
// App's Decoder, fault.Settings.Decoder, reads it, such as jsonscenario's
// member, so a scenario written for an HTTP request's baggage header can be
// put on an object unchanged. The tool that runs the experiment writes it:
//
//	codec, _ := jsonscenario.New("acme.fault")
//	ctx, _ := fit.ContextWithScenario(context.Background(), codec, fit.Scenario{Rules: []fit.Rule{
//		{Point: "kube:deployments.apps:patch", Fault: fit.Fault{Abort: true}},
//	}})
//	value := baggage.FromContext(ctx).String()
//
// When a reconcile's span starts, the controller reads the annotation from
// the object as the cache holds it. That is before it adds or removes its
// finalizer, calls a Finalize's f, or calls the Reconciler, so each of them,
// the controller's own applies, and every request and injection point they
// reach see the scenario. The scenario belongs to the object: a reconcile
// that a change to an owned resource triggered carries the owner's
// scenario, and the owned resource's annotations aren't read.
//
// The annotation is read one of four ways:
//   - no annotation, or none of the Decoder's members in it: the reconcile
//     carries no scenario, and nothing is recorded;
//   - a value that isn't W3C baggage: the reconcile carries no scenario, and
//     its span gets a humus.fit.strip event with error.type and
//     error.message. It is the one strip with attributes: the controller
//     parses the annotation itself, so only it can tell an annotation it
//     can't read from an object it doesn't trust. The reconcile goes on;
//   - an object trust doesn't trust: the reconcile carries no scenario, and
//     its span gets a humus.fit.strip event with no attributes, as a
//     server's request does;
//   - a trusted object: the Decoder's members join the reconcile's context,
//     and no other member of the annotation does, so an annotation can't put
//     arbitrary baggage on every request a reconcile makes.
//
// trust is asked only about an object whose annotation carries a scenario,
// and only when the App has failure injection, and is handed its own copy
// of the object. A panic in trust is recovered and recorded as a panic in
// the Reconciler is (see Panics): the reconcile's outcome is panic, its key
// is forgotten, and nothing else runs, so no finalizer and no status is
// applied. Without TrustScenario, nothing is read, and no reconcile carries
// a scenario: a controller that never adds it can't be put under test by
// anyone who can annotate its objects.
//
// Adding the annotation starts an experiment, and removing it ends one, at
// once: either changes the object, which reconciles it straight away. A
// scenario stays as long as its annotation, so every retry meets it again,
// and one that aborts a write keeps the object backing off to
// RetryMaxDelay, as an outage would. The reconcile after the annotation is
// removed carries no scenario. A Decoder that knows expiry can end an
// experiment sooner.
//
// Who may set the annotation is decided outside the process, by RBAC and
// admission, and trust reads what those decisions leave visible: the
// object's namespace, a label an admission policy guards, or the object's
// owner. In the example above, a ValidatingAdmissionPolicy lets only the
// chaos harness's ServiceAccount set the annotation on objects in the
// staging namespace, so trusting every object in staging trusts the
// harness alone, and an object anywhere else is never put under test.
//
// Every request through the Client's transport, the end user's clientsets'
// included, is an injection point of s.Faults, the Injector of the Setup
// the Client is built with, as every humus client is. A request is
// evaluated with the failure scenario its context carries. One that
// carries none, or a request in an App without failure injection, is
// evaluated, stripped and recorded not at all. The informers, leader
// election, the Lease's release and the check that a resource is served
// run on Serve's context, which carries no scenario, so they always run
// without one.
//
// The point names what the request does to which resource, as the API
// server's authorizer and audit log name it:
//
//	kube:<resource>[.<group>][/<subresource>]:<verb>
//
// Its parts are these:
//   - the resource is the plural, and the group is left out for the core
//     group, as kubectl writes a resource: deployments.apps, secrets,
//     memcacheds.cache.example.com;
//   - the verb is the API server's: get, list, watch, create, update,
//     patch, delete or deletecollection. A server-side apply is a patch;
//   - a request for a path that isn't a resource, such as discovery's
//     /apis, is kube:<path>:<verb>, such as kube:/apis:get, whose verb is
//     the request's method in lower case.
//
// A point carries no namespace and no object's name. fit matches a point
// exactly, or by a prefix ending in *, so a rule can fail everything about
// one resource:
//
//	kube:*                                          every request to the API server
//	kube:deployments.apps:*                         every request about Deployments, but not their subresources
//	kube:deployments.apps:patch                     every apply of a Deployment
//	kube:memcacheds.cache.example.com/status:patch  the controller's apply of a Memcached's status
//	kube:memcacheds.cache.example.com:patch         the controller's applies of a Memcached's finalizer
//
// A Kind's controller applies no status, so only a Resource's controller
// makes the status apply.
//
// The point is evaluated in the injection layer, which sits between
// otelhttp and the token. So its humus.fit.evaluation event lands on the
// request's span, and a delay is inside the span and in
// http.client.request.duration. A watch has no span, so its event lands on
// the span in its context, such as a reconcile's.
//
// A fault is a real failure, as it is to every humus client:
//
//   - An abort fails the request as an unreachable API server would: with
//     nothing sent, and with no call to Connection.Token. The error a
//     client-go caller gets wraps a *fault.InjectedError naming the point,
//     which errors.As finds, and which matches fit.ErrInjected. client-go
//     doesn't retry it, not even a GET's: it retries only a connection that
//     was reset, ended early or lost, and a response with Retry-After. The
//     request's span has the error.type
//     github.com/z5labs/humus/fault.InjectedError.
//   - A delay is waited before the request is sent, inside the request's
//     context, and then the request goes on as usual. A delay that the
//     context cuts short fails the request with a *fault.InjectedError whose
//     Err is the context's cause, such as context.DeadlineExceeded, and that
//     doesn't match fit.ErrInjected. Nothing is sent.
//   - A delay that a Timeout the end user set on a rest.Config cuts short
//     is the one exception. client-go then runs two timers, a deadline on
//     the request's context and the http.Client's own Timeout, which go off
//     together. When net/http's has gone off by the time the transport
//     returns, net/http replaces the transport's error with a timeout error
//     of its own, which matches context.DeadlineExceeded and holds no
//     *fault.InjectedError. Either way, the request timed out, and its span
//     has InjectedError's error.type. [Client.Config] sets no Timeout, so
//     the controller's own requests always fail with the
//     *fault.InjectedError.
//
// The API server takes no part in failure injection, so the Client strips
// the scenario from every request it sends: the scenario's members are
// removed from the request's baggage header, and every other member, and
// the request's trace context, are kept. A strip adds a humus.fit.strip
// event, with no attributes, to the request's span. It happens after the
// point is evaluated, since the point reads the scenario, so an aborted
// request has nothing to strip. A watch sends no baggage, since otelhttp
// doesn't trace it, so it has nothing to strip either. There is no
// ForwardScenario, as there is for other humus clients: a field that
// should never be set would be an API for nothing. A scenario goes on to
// the reconciled object's children instead, in their annotation.
//
// # Carrying a scenario on to children
//
// An operator is often several controllers, each reconciling the objects
// the one above it applies. So in a reconcile that carries a scenario, the
// Client sets the TrustScenario's annotation on each child the reconcile
// applies, and one experiment on the top object reaches every controller
// below it. The Client changes a request that is all of these:
//   - a server-side apply, a PATCH whose content type is
//     application/apply-patch+yaml, to an object, not to one of its
//     subresources;
//   - made with the reconcile's context, or a context made from it;
//   - of an object whose metadata.ownerReferences has a controller
//     reference whose uid is the reconciled object's, as Kubernetes'
//     garbage collector names a child. The body declares the reference, so
//     the child's resource needs no [Owns].
//
// It sets metadata.annotations[<annotation>] in the request's body to the
// scenario's members as W3C baggage, without any other member of the
// reconcile's baggage, and replaces any value the body gave it. The body
// is sent on as JSON, which is YAML too. That happens after the point is
// evaluated, so an aborted apply sends nothing, and after the request's
// baggage is stripped, so the API server still gets no scenario. Setting
// the annotation adds no event: the apply's span has its
// humus.fit.evaluation and humus.fit.strip events, and the child's
// reconciles show the scenario they carry.
//
// The Reconciler applies its children as it always does:
//
//	_, err := r.Kube.AppsV1().Deployments(m.Namespace).Apply(ctx,
//		appsv1ac.Deployment(m.Name, m.Namespace).
//			WithOwnerReferences(metav1ac.OwnerReference().
//				WithAPIVersion(m.APIVersion).WithKind(m.Kind).WithName(m.Name).WithUID(m.UID).
//				WithController(true).WithBlockOwnerDeletion(true)).
//			WithSpec(spec),
//		metav1.ApplyOptions{FieldManager: "memcached-operator", Force: true},
//	)
//
// What is left as it is:
//   - a request in a reconcile with no scenario, or outside a reconcile;
//   - a create, an update, and a patch that isn't an apply. Only an apply
//     takes the annotation off again: a child created or updated with it
//     would keep it after the experiment ends, and keep failing;
//   - a body that isn't JSON or YAML. client-go sends JSON unless the end
//     user sets the rest.Config's ContentType to protobuf, or turns on its
//     ClientsAllowCBOR and ClientsPreferCBOR feature gates for CBOR. Such an
//     apply is sent as it is;
//   - the controller's applies of the reconciled object's own status and
//     finalizer. They are of the object, which isn't its own child;
//   - an object that isn't the reconciled object's child: one whose
//     controller owner reference names another object, or that has only
//     owner references that aren't a controller's.
//
// The child's controller decides whether the child's reconciles carry the
// scenario. A controller of the child's resource, with a TrustScenario of
// its own on the same annotation, carries the child's scenario into the
// child's reconciles when its trust trusts the child, and its own applies
// carry the scenario on to the grandchildren. What makes a child's
// annotation worth trusting is an admission policy that lets only the
// operator's ServiceAccount set it on the children's resources.
//
// Ending the experiment on the top object ends it all the way down.
// Removing the annotation reconciles the top object at once, without a
// scenario. Its apply of each child no longer declares the annotation, and
// a server-side apply removes what its field manager stops declaring, so
// the API server removes the annotation, unless another field manager also
// set it. That changes the child, whose controller reconciles it at once,
// without a scenario, and so on down.
//
// A child that no controller reads keeps an inert annotation for as long
// as the experiment runs, such as a Deployment. Kubernetes' own
// controllers ignore it, and a Deployment's annotations aren't its pods'
// template, so it starts no rollout.
//
// # What client-go still reaches
//
// client-go's feature gates read KUBE_FEATURE_* variables from the real
// environment, with os.LookupEnv, the first time a gate is used. The only
// way to replace them is features.ReplaceFeatureGates, which is global, so
// humus never calls it: this is an ambient leak humus can't close. Like the
// other ambient leaks the lifecycle documents, it isn't the end user's to
// pass in.
//
// client-go falls back to klog's global logger, which writes to the real
// stderr, wherever it finds no logger in its context. The Controller puts
// one in the context of everything of client-go's it runs. Its work queue
// records through the controller's own metrics provider, set in the queue's
// config, so it never reaches client-go's global provider.
//
// The package sets no global of client-go's: no klog setting, no feature
// gate, and no metrics provider. Lint denies each of them in every humus
// module, and denies rest.InClusterConfig, which reads the environment and
// the disk itself. controller-runtime and controller-tools are denied too,
// since their init functions set globals that no wrapper can avoid.
package kube
