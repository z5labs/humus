// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package kube reaches a Kubernetes API server with humus's lifecycle, with
// client-go, Kubernetes' own client, and no controller-runtime, and
// declares custom resources, whose CRDs it reflects from Go types.
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
// # What client-go still reaches
//
// client-go's feature gates read KUBE_FEATURE_* variables from the real
// environment, with os.LookupEnv, the first time a gate is used. The only
// way to replace them is features.ReplaceFeatureGates, which is global, so
// humus never calls it: this is an ambient leak humus can't close. Like the
// other ambient leaks the lifecycle documents, it isn't the end user's to
// pass in.
//
// The package sets no global of client-go's: no klog setting, no feature
// gate, and no metrics provider. Lint denies each of them in every humus
// module, and denies rest.InClusterConfig, which reads the environment and
// the disk itself. controller-runtime and controller-tools are denied too,
// since their init functions set globals that no wrapper can avoid.
package kube
