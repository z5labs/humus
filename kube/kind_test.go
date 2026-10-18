// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kptr "k8s.io/utils/ptr"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// ConfigMaps is the built-in kind the controllers of a Kind under test
// reconcile.
var ConfigMaps = kube.Kind[corev1.ConfigMap]{
	GVR:   corev1.SchemeGroupVersion.WithResource("configmaps"),
	Kind:  "ConfigMap",
	Scope: kube.Namespaced,
}

// Namespaces is the cluster-scoped built-in kind of the tests of an owner in
// no namespace.
var Namespaces = kube.Kind[corev1.Namespace]{
	GVR:   corev1.SchemeGroupVersion.WithResource("namespaces"),
	Kind:  "Namespace",
	Scope: kube.Cluster,
}

// configMapObject returns the ConfigMap default/name, holding data.
func configMapObject(name string, data map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": "default", "uid": name + "-uid"},
		"data":       data,
	}}
}

// namespaceObject returns the Namespace name.
func namespaceObject(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": name, "uid": name + "-uid"},
	}}
}

// newConfigMapCluster returns a fakeCluster of ConfigMaps holding objs.
func newConfigMapCluster(t *testing.T, objs ...runtime.Object) *fakeCluster {
	t.Helper()
	return newFakeClusterOf(t, ConfigMaps.GVR, "ConfigMapList", "default", objs...)
}

// newKindController returns a Controller of k on fc, reconciled by rec,
// built with s, testSettings and parts.
func newKindController[T any](t *testing.T, s *humus.Setup, fc *fakeCluster, k kube.Kind[T], rec kube.KindReconciler[T], parts ...kube.Part) *kube.Controller {
	t.Helper()
	ctrl, err := kube.NewKindController(s, testClient(t, s), testSettings(), k, rec, parts...)
	require.NoError(t, err)
	kube.SetClients(ctrl, fc.dyn, fc.meta, fc.kube.CoordinationV1())
	return ctrl
}

// handed records each object a KindReconciler, or a Finalize's f, was
// handed, and when.
type handed[T any] struct {
	mu   sync.Mutex
	objs []T
	at   []time.Time
}

func (h *handed[T]) add(obj T) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.objs = append(h.objs, obj)
	h.at = append(h.at, time.Now())
	return len(h.objs)
}

func (h *handed[T]) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.objs)
}

func (h *handed[T]) get(i int) (T, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.objs[i], h.at[i]
}

// settles is a KindReconciler that records each object, and returns 0.
func settles[T any](got *handed[T]) kube.KindReconcilerFunc[T] {
	return func(_ context.Context, obj T) (time.Duration, error) {
		got.add(obj)
		return 0, nil
	}
}

// configMapOwner returns the controller owner reference that names the
// ConfigMap default/name.
func configMapOwner(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       name,
		UID:        types.UID(name + "-uid"),
		Controller: kptr.To(true),
	}
}

func TestNewKindController_reports_every_mistake(t *testing.T) {
	t.Parallel()

	s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
	client := testClient(t, s)
	rec := settles(&handed[corev1.ConfigMap]{})
	noCleanupOf := func(context.Context, corev1.ConfigMap) error { return nil }

	cases := []struct {
		name     string
		settings func(*kube.Settings)
		kind     func(*kube.Kind[corev1.ConfigMap])
		rec      kube.KindReconciler[corev1.ConfigMap]
		noRec    bool
		parts    []kube.Part
		want     []string
	}{
		{name: "a bad Settings field", settings: func(s *kube.Settings) { s.Workers = 0 },
			want: []string{"Settings.Workers must be at least 1"}},
		{name: "a bound between Settings fields", settings: func(s *kube.Settings) { s.RetryBaseDelay, s.RetryMaxDelay = time.Second, time.Millisecond },
			want: []string{"Settings.RetryMaxDelay, 1ms, must be at least Settings.RetryBaseDelay, 1s"}},
		{name: "a GVR with no Version", kind: func(k *kube.Kind[corev1.ConfigMap]) { k.GVR.Version = "" },
			want: []string{"the Kind configmaps: Kind.GVR, /, Resource=configmaps, has no Version"}},
		{name: "a GVR with no Resource", kind: func(k *kube.Kind[corev1.ConfigMap]) { k.GVR.Resource = "" },
			want: []string{"the Kind kube.Kind[k8s.io/api/core/v1.ConfigMap]: Kind.GVR, /v1, Resource=, has no Resource"}},
		{name: "a missing Kind", kind: func(k *kube.Kind[corev1.ConfigMap]) { k.Kind = "" },
			want: []string{"the Kind configmaps: Kind.Kind must not be empty"}},
		{name: "a Scope that is neither", kind: func(k *kube.Kind[corev1.ConfigMap]) { k.Scope = "Global" },
			want: []string{`Kind.Scope is "Global", not "Namespaced" or "Cluster"`}},
		{name: "a cluster-scoped Kind limited to a namespace",
			settings: func(s *kube.Settings) { s.Namespace = "default" },
			kind:     func(k *kube.Kind[corev1.ConfigMap]) { k.Scope = kube.Cluster },
			want:     []string{`Settings.Namespace is "default", but configmaps is cluster-scoped, so its objects are in no namespace`}},
		{name: "a nil Reconciler", noRec: true,
			want: []string{"the Reconciler must not be nil"}},
		{name: "a nil KindReconcilerFunc", rec: kube.KindReconcilerFunc[corev1.ConfigMap](nil),
			want: []string{"the Reconciler must not be a nil KindReconcilerFunc"}},
		{name: "a nil Part", parts: []kube.Part{nil},
			want: []string{"parts[0] must not be nil"}},
		{name: "an Owns with no Version", parts: []kube.Part{kube.Owns(schema.GroupVersionResource{Group: "apps", Resource: "deployments"})},
			want: []string{"parts[0], Owns(apps/, Resource=deployments), has no Version"}},
		{name: "a Finalize of a Resource's Object", parts: []kube.Part{kube.Finalize(finalizerName, noCleanup)},
			want: []string{`parts[0], Finalize("cache.example.com/finalizer"), takes a kube.Object[`, "but the Controller's objects are a v1.ConfigMap"}},
		{name: "a Finalize of another kind", parts: []kube.Part{kube.Finalize(finalizerName, func(context.Context, corev1.Secret) error { return nil })},
			want: []string{`parts[0], Finalize("cache.example.com/finalizer"), takes a v1.Secret, but the Controller's objects are a v1.ConfigMap`}},
		{name: "a Finalize of a pointer", parts: []kube.Part{kube.Finalize(finalizerName, func(context.Context, *corev1.ConfigMap) error { return nil })},
			want: []string{"takes a *v1.ConfigMap, but the Controller's objects are a v1.ConfigMap"}},
		{name: "a second Finalize", parts: []kube.Part{kube.Finalize(finalizerName, noCleanupOf), kube.Finalize("cache.example.com/other", noCleanupOf)},
			want: []string{`parts[1], Finalize("cache.example.com/other"), is a second Finalize, after parts[0]`}},
		{name: "a Finalize with an empty name and a nil f", parts: []kube.Part{kube.Finalize[corev1.ConfigMap]("", nil)},
			want: []string{"parts[0], a Finalize, has an empty name", `parts[0], Finalize(""), has a nil f`}},
		{name: "several together",
			settings: func(s *kube.Settings) {
				s.Name = ""
				s.LeaderElection.Identity = ""
			},
			kind: func(k *kube.Kind[corev1.ConfigMap]) {
				k.GVR = schema.GroupVersionResource{}
				k.Kind = ""
				k.Scope = ""
			},
			noRec: true,
			parts: []kube.Part{kube.Owns(schema.GroupVersionResource{Version: "v1"})},
			want: []string{
				"Settings.Name must not be empty",
				"Settings.LeaderElection.Identity must not be empty",
				"has no Version",
				"has no Resource",
				"Kind.Kind must not be empty",
				`Kind.Scope is "", not "Namespaced" or "Cluster"`,
				"the Reconciler must not be nil",
				"parts[0], Owns(/v1, Resource=), has no Resource",
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			settings := testSettings()
			if tc.settings != nil {
				tc.settings(&settings)
			}
			k := ConfigMaps
			if tc.kind != nil {
				tc.kind(&k)
			}
			var rc kube.KindReconciler[corev1.ConfigMap] = rec
			switch {
			case tc.noRec:
				rc = nil
			case tc.rec != nil:
				rc = tc.rec
			}
			ctrl, err := kube.NewKindController(s, client, settings, k, rc, tc.parts...)
			require.Nil(t, ctrl)
			require.ErrorContains(t, err, "kube: NewKindController: ")
			for _, want := range tc.want {
				require.ErrorContains(t, err, want)
			}
		})
	}

	t.Run("a T whose pointer isn't a metav1.Object", func(t *testing.T) {
		t.Parallel()
		k := kube.Kind[corev1.ConfigMapList]{GVR: ConfigMaps.GVR, Kind: "ConfigMapList", Scope: kube.Namespaced}
		_, err := kube.NewKindController(s, client, testSettings(), k, settles(&handed[corev1.ConfigMapList]{}))
		require.ErrorContains(t, err, "the Kind's objects are a v1.ConfigMapList, but a *v1.ConfigMapList isn't a metav1.Object")

		_, err = kube.NewKindController(s, client, testSettings(), kube.Kind[*corev1.ConfigMap]{GVR: ConfigMaps.GVR, Kind: "ConfigMap", Scope: kube.Namespaced},
			settles(&handed[*corev1.ConfigMap]{}))
		require.ErrorContains(t, err, "the Kind's objects are a *v1.ConfigMap, but a **v1.ConfigMap isn't a metav1.Object")
	})

	t.Run("a nil Setup and a nil Client", func(t *testing.T) {
		t.Parallel()
		_, err := kube.NewKindController(nil, nil, testSettings(), ConfigMaps, rec)
		require.ErrorContains(t, err, "the Setup must not be nil")
		require.ErrorContains(t, err, "the Client must not be nil")
	})

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		ctrl, err := kube.NewKindController(s, client, testSettings(), ConfigMaps, rec, kube.Owns(deployments), kube.Finalize(finalizerName, noCleanupOf))
		require.NoError(t, err)
		require.NotNil(t, ctrl)

		settings := testSettings()
		settings.Namespace = ""
		ctrl, err = kube.NewKindController(s, client, settings, Namespaces, settles(&handed[corev1.Namespace]{}))
		require.NoError(t, err, "a cluster-scoped Kind in every namespace")
		require.NotNil(t, ctrl)
	})
}

func TestKindController_hands_the_Reconciler_client_gos_struct_and_reconciles_again_after(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newConfigMapCluster(t, configMapObject("settings", map[string]any{"level": "debug"}))
		var got handed[corev1.ConfigMap]
		const after = 10 * time.Minute
		rec := kube.KindReconcilerFunc[corev1.ConfigMap](func(_ context.Context, cm corev1.ConfigMap) (time.Duration, error) {
			if got.add(*cm.DeepCopy()) < 3 {
				// The Reconciler's own copy: nothing it does reaches the
				// cache.
				cm.Data["level"] = "changed"
				return after, nil
			}
			return 0, nil
		})
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newKindController(t, s, fc, ConfigMaps, rec))
		time.Sleep(settle)

		require.Equal(t, 1, got.count(), "reconciled once, with no status to apply")
		cm, first := got.get(0)
		require.Equal(t, "settings", cm.Name)
		require.Equal(t, "default", cm.Namespace)
		require.Equal(t, "ConfigMap", cm.Kind)
		require.Equal(t, "v1", cm.APIVersion)
		require.Equal(t, map[string]string{"level": "debug"}, cm.Data)

		time.Sleep(after)
		require.Equal(t, 2, got.count(), "after reconciles it again")
		cm, second := got.get(1)
		require.Equal(t, after, second.Sub(first))
		require.Equal(t, "debug", cm.Data["level"], "handed its own copy, as the cache holds it")

		time.Sleep(2 * after)
		require.Equal(t, 3, got.count(), "and again, after the second after")
		time.Sleep(2 * after)
		require.Equal(t, 3, got.count(), "zero waits for a change or the resync")

		require.Empty(t, fc.statusApplies(), "no status is ever applied")
		require.Empty(t, fc.metadataApplies(), "nor anything else, without a Finalize")

		var spans int
		for _, span := range tel.Spans() {
			if span.Name() != "reconcile ConfigMap" {
				continue
			}
			spans++
			require.Equal(t, codes.Unset, span.Status().Code)
			attrs := map[string]string{}
			for _, kv := range span.Attributes() {
				attrs[string(kv.Key)] = kv.Value.String()
			}
			require.Equal(t, "default", attrs["k8s.namespace.name"])
			require.Equal(t, "settings", attrs["humus.kube.object.name"])
		}
		require.Equal(t, 3, spans, "each reconcile is a span named after the Kind")
		counts, _ := outcomes(t, controllerMetrics(t, tel))
		require.Equal(t, map[string]uint64{"success": 3}, counts)

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestKindController_retries_an_error_with_backoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newConfigMapCluster(t, configMapObject("settings", nil))
		var got handed[corev1.ConfigMap]
		rec := kube.KindReconcilerFunc[corev1.ConfigMap](func(_ context.Context, cm corev1.ConfigMap) (time.Duration, error) {
			if got.add(cm) == 1 {
				return time.Hour, errors.New("not yet")
			}
			return 0, nil
		})
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newKindController(t, s, fc, ConfigMaps, rec))
		time.Sleep(settle)

		require.Equal(t, 2, got.count(), "retried with backoff, not after an hour")
		counts, _ := outcomes(t, controllerMetrics(t, tel))
		require.Equal(t, map[string]uint64{"error": 1, "success": 1}, counts)

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestKindController_Owns_reconciles_the_owner(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newConfigMapCluster(t, configMapObject("settings", nil), configMapObject("other", nil))
		var got handed[corev1.ConfigMap]
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		sv := serve(t, newKindController(t, s, fc, ConfigMaps, settles(&got), kube.Owns(deployments)))
		time.Sleep(settle)
		require.Equal(t, 2, got.count(), "each ConfigMap once")

		fc.createDeployment(t, deployment("app", configMapOwner("settings")))
		time.Sleep(settle)
		require.Equal(t, 3, got.count(), "a resource it owns reconciles it")
		cm, _ := got.get(2)
		require.Equal(t, "settings", cm.Name)

		fc.changeDeployment(t, "app")
		time.Sleep(settle)
		require.Equal(t, 4, got.count(), "a change to it does too")

		// A reference naming a ConfigMap of another group names another
		// kind.
		other := configMapOwner("other")
		other.APIVersion = "example.com/v1"
		fc.createDeployment(t, deployment("elsewhere", other))
		time.Sleep(settle)
		require.Equal(t, 4, got.count(), "an owner of another group reconciles nothing")

		require.Empty(t, fc.statusApplies())
		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestKindController_Owns_reconciles_a_cluster_scoped_owner(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeClusterOf(t, Namespaces.GVR, "NamespaceList", "", namespaceObject("default"), namespaceObject("kube-system"))
		var got handed[corev1.Namespace]
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		sv := serve(t, newKindController(t, s, fc, Namespaces, settles(&got), kube.Owns(deployments)))
		time.Sleep(settle)
		require.Equal(t, 2, got.count(), "each Namespace once")

		// A Deployment in the default namespace, which the Namespace
		// default controls.
		fc.createDeployment(t, deployment("app", metav1.OwnerReference{
			APIVersion: "v1",
			Kind:       "Namespace",
			Name:       "default",
			UID:        "default-uid",
			Controller: kptr.To(true),
		}))
		time.Sleep(settle)
		require.Equal(t, 3, got.count(), "the namespaced resource reconciles its cluster-scoped owner")
		ns, _ := got.get(2)
		require.Equal(t, "default", ns.Name)
		require.Empty(t, ns.Namespace)

		fc.deleteDeployment(t, "app")
		time.Sleep(settle)
		require.Equal(t, 4, got.count(), "and so does its deletion")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestKindController_Finalize_runs_with_no_status_applied(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newConfigMapCluster(t, configMapObject("settings", map[string]any{"level": "debug"}))
		var got, cleaned handed[corev1.ConfigMap]
		finalize := kube.Finalize(finalizerName, func(_ context.Context, cm corev1.ConfigMap) error {
			cleaned.add(cm)
			return nil
		})
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		sv := serve(t, newKindController(t, s, fc, ConfigMaps, settles(&got), finalize))
		time.Sleep(settle)

		require.Equal(t, 1, got.count(), "reconciled once the finalizer is on it")
		cm, _ := got.get(0)
		require.Equal(t, []string{finalizerName}, cm.Finalizers)
		applies := fc.metadataApplies()
		require.Len(t, applies, 1)
		require.Equal(t, "v1", applies[0].patch["apiVersion"])
		require.Equal(t, "ConfigMap", applies[0].patch["kind"])
		require.Equal(t, controllerName, applies[0].options.FieldManager)
		require.Zero(t, cleaned.count())

		fc.deleting(t, "settings")
		time.Sleep(settle)
		require.Equal(t, 1, cleaned.count(), "f runs once it is being deleted")
		cm, _ = cleaned.get(0)
		require.Equal(t, "settings", cm.Name)
		require.NotNil(t, cm.DeletionTimestamp)
		require.Equal(t, "debug", cm.Data["level"], "handed client-go's struct")
		require.Equal(t, 1, got.count(), "in place of the Reconciler")
		require.True(t, fc.gone("settings"), "the finalizer is removed, and the deletion completes")
		require.Len(t, fc.metadataApplies(), 2)

		require.Empty(t, fc.statusApplies(), "no status is ever applied")
		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestKindController_recovers_a_panic_and_never_retries_it(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newConfigMapCluster(t, configMapObject("settings", nil))
		var got handed[corev1.ConfigMap]
		rec := kube.KindReconcilerFunc[corev1.ConfigMap](func(_ context.Context, cm corev1.ConfigMap) (time.Duration, error) {
			got.add(cm)
			var keys []string
			return 0, errors.New(keys[len(cm.Data)])
		})
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		ctrl, err := kube.NewKindController(s, testClient(t, s), panicSettings(), ConfigMaps, rec)
		require.NoError(t, err)
		kube.SetClients(ctrl, fc.dyn, fc.meta, fc.kube.CoordinationV1())
		sv := serve(t, ctrl)
		time.Sleep(settle)

		require.Equal(t, 1, got.count(), "no retry, though RetryMaxDelay has passed many times over")
		var panicked int
		for _, span := range tel.Spans() {
			if span.Name() == "reconcile ConfigMap" && span.Status().Code == codes.Error && span.Status().Description == "reconcile panicked" {
				panicked++
			}
		}
		require.Equal(t, 1, panicked)
		counts, _ := outcomes(t, controllerMetrics(t, tel))
		require.Equal(t, map[string]uint64{"panic": 1}, counts)
		require.False(t, sv.returned(), "a panic never ends the Service")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestKindController_is_a_humus_Service(t *testing.T) {
	t.Parallel()
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		fc := newConfigMapCluster(t, configMapObject("settings", nil))
		return newKindController(t, s, fc, ConfigMaps, settles(&handed[corev1.ConfigMap]{}))
	})
}
