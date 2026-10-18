// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/kube"
	"github.com/z5labs/humus/telemetry"
)

// The Kubernetes page of the docs site quotes the code in this file, and
// TestDocs_code_samples checks that every quote still matches it. The
// Memcached Resource it reconciles is in resource_test.go, and the
// Namespaces Kind in kind_test.go. The tests in init_test.go run Init,
// inPod, fromKubeconfig, the Controllers and the probes, and the tests at
// the end of this file are the end user's own: the golden CRD, and the
// Reconciler as a function.

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

// serviceAccount is where the kubelet mounts the pod's service account: its
// token, and the cluster's CA.
const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount/"

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

// readFile reads the whole file name through fsys.
func readFile(fsys humus.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// buildTelemetry is the controller's telemetry.Builder, which names the
// service.
func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "memcached-controller"
	return telemetry.New(ctx, s, dial, p.Stdout)
}

// A controller's main is one call to humus.Main, whose defaults fit a
// controller, with an Init that sources the Settings and the Connection and
// returns the Controller. This example has no Output comment, so it is
// compiled but never run: running it would exit the test binary.
func Example_main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}

// backups is where the end user keeps each Memcached's backups, outside the
// cluster, where owner references can't garbage collect them.
type backups interface {
	Delete(ctx context.Context, namespace, name string) error
}

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

// defaultDeny returns the Controller that gives every Namespace
// DefaultDeny's policy, and applies it again when it changes.
func defaultDeny(s *humus.Setup, client *kube.Client, settings kube.Settings, kc kubernetes.Interface) (*kube.Controller, error) {
	return kube.NewKindController(s, client, settings, Namespaces, DefaultDeny{Kube: kc},
		kube.Owns(networkingv1.SchemeGroupVersion.WithResource("networkpolicies")),
	)
}

// withProbes returns the Services of a controller's pod, which has no server
// of its own: the Controller, and a probes server made with httphealth,
// whose readiness waits for the Controller's caches to sync.
func withProbes(s *humus.Setup, ctrl *kube.Controller, probeSettings httphealth.Settings) ([]humus.Service, error) {
	probes, err := httphealth.NewService(s, probeSettings, health.Readiness(ctrl.Synced()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{ctrl, probes}, nil
}

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

func TestReconciler_copies_the_Deployments_Available_condition(t *testing.T) {
	kc := kubefake.NewClientset()
	m := cacheObject()
	_, err := Reconciler{Kube: kc}.Reconcile(t.Context(), m)
	require.NoError(t, err)

	// The Deployment controller's view of the Deployment.
	d, err := kc.AppsV1().Deployments("default").Get(t.Context(), "cache", metav1.GetOptions{})
	require.NoError(t, err)
	d.Status.ReadyReplicas = 1
	d.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse,
		Reason: "MinimumReplicasUnavailable", Message: "Deployment does not have minimum availability.",
	}}
	_, err = kc.AppsV1().Deployments("default").UpdateStatus(t.Context(), d, metav1.UpdateOptions{})
	require.NoError(t, err)

	res, err := Reconciler{Kube: kc}.Reconcile(t.Context(), m)
	require.NoError(t, err)
	require.Equal(t, int32(1), res.Status.ReadyReplicas)
	c := meta.FindStatusCondition(res.Status.Conditions, "Available")
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionFalse, c.Status)
	require.Equal(t, "MinimumReplicasUnavailable", c.Reason)
	require.Equal(t, "Deployment does not have minimum availability.", c.Message)
	require.Equal(t, int64(1), c.ObservedGeneration)
}

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
