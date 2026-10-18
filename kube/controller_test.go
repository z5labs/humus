// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	kptr "k8s.io/utils/ptr"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// The names the controllers under test are given.
const (
	controllerName = "memcached-controller"
	leaseNamespace = "kube-system"
	identity       = "replica-1"
)

// settle is longer than anything a test waits for in fake time, other than
// what it waits for on purpose: an acquired Lease, a synced cache, and a
// reconcile and its status's reconcile.
const settle = time.Minute

// testSettings returns DefaultSettings with the required fields filled.
func testSettings() kube.Settings {
	s := kube.DefaultSettings()
	s.Name = controllerName
	s.LeaderElection.Namespace = leaseNamespace
	s.LeaderElection.Identity = identity
	return s
}

// noDial is a Host's Dial that reaches nothing: a controller under test
// reaches the API server only through client-go's fakes.
func noDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("no network")
}

// testClient returns a Client that reaches nothing.
func testClient(t *testing.T, s *humus.Setup) *kube.Client {
	t.Helper()
	c, err := kube.NewClient(s, kube.Connection{Server: "apiserver:6443", TLS: &tls.Config{}})
	require.NoError(t, err)
	return c
}

// memcachedObject returns a Memcached named name, in the default namespace,
// of size.
func memcachedObject(name string, size int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cache.example.com/v1alpha1",
		"kind":       "Memcached",
		"metadata":   map[string]any{"name": name, "namespace": "default", "uid": name + "-uid"},
		"spec":       map[string]any{"size": size, "image": "memcached:1.6"},
	}}
}

// apply is one server-side apply the fake dynamic client was sent.
type apply struct {
	namespace, name string
	subresource     string
	options         metav1.PatchOptions
	patch           map[string]any
}

// fakeCluster is client-go's fakes, standing in for an API server: a
// dynamic client of one resource, Memcacheds unless newFakeClusterOf names
// another, a metadata client of the resources a
// controller owns, and a clientset for the Lease. The dynamic
// fake can't apply an unstructured object, so a reactor of the test's own
// applies a status, setting the object's status to the patch's, and the
// object's metadata, as applyMetadata says.
type fakeCluster struct {
	dyn  *dynamicfake.FakeDynamicClient
	meta *metadatafake.FakeMetadataClient
	kube *k8sfake.Clientset

	// gvr is the dynamic client's resource, and namespace the namespace of
	// the objects the helpers below name: "default", or "" for a
	// cluster-scoped resource.
	gvr       schema.GroupVersionResource
	namespace string

	mu      sync.Mutex
	applies []apply
	// finalizers are the finalizers each field manager's last apply of an
	// object's metadata set, by field manager and the object's
	// namespace/name.
	finalizers map[string][]string
}

func newFakeCluster(t *testing.T, objs ...runtime.Object) *fakeCluster {
	t.Helper()
	return newFakeClusterOf(t, Memcached.GVR(), "MemcachedList", "default", objs...)
}

// newFakeClusterOf returns a fakeCluster whose dynamic client serves gvr,
// whose list kind is listKind, and whose helpers name objects in namespace.
func newFakeClusterOf(t *testing.T, gvr schema.GroupVersionResource, listKind, namespace string, objs ...runtime.Object) *fakeCluster {
	t.Helper()
	fc := &fakeCluster{
		dyn: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{gvr: listKind}, objs...),
		meta:      metadatafake.NewSimpleMetadataClient(metadatafake.NewTestScheme()),
		kube:      k8sfake.NewClientset(),
		gvr:       gvr,
		namespace: namespace,
	}
	fc.dyn.PrependReactor("patch", gvr.Resource, func(a clienttesting.Action) (bool, runtime.Object, error) {
		p, ok := a.(clienttesting.PatchActionImpl)
		if !ok || p.PatchType != types.ApplyPatchType {
			return false, nil, nil
		}
		var patch map[string]any
		require.NoError(t, utiljson.Unmarshal(p.Patch, &patch))
		fc.mu.Lock()
		fc.applies = append(fc.applies, apply{
			namespace:   p.Namespace,
			name:        p.Name,
			subresource: p.Subresource,
			options:     p.PatchOptions,
			patch:       patch,
		})
		fc.mu.Unlock()

		got, err := fc.dyn.Tracker().Get(gvr, p.Namespace, p.Name)
		if err != nil {
			return true, nil, err
		}
		u := got.(*unstructured.Unstructured).DeepCopy()
		switch p.Subresource {
		case "status":
			u.Object["status"] = runtime.DeepCopyJSONValue(patch["status"])
		case "":
			return fc.applyMetadata(u, p.PatchOptions.FieldManager, patch)
		}
		return true, u, fc.dyn.Tracker().Update(gvr, u, p.Namespace)
	})
	return fc
}

// applyMetadata applies patch's metadata to u as the API server applies
// manager's finalizers: those manager's last apply set and this one doesn't
// are removed, unless another manager's last apply set them too, and those
// this one sets are added. An object being deleted is
// deleted once it has no finalizer left. A patch that names another uid is
// a conflict, as it is from the API server.
func (fc *fakeCluster) applyMetadata(u *unstructured.Unstructured, manager string, patch map[string]any) (bool, runtime.Object, error) {
	gvr := fc.gvr
	md, _ := patch["metadata"].(map[string]any)
	if uid, ok := md["uid"].(string); ok && uid != string(u.GetUID()) {
		return true, nil, apierrors.NewConflict(gvr.GroupResource(), u.GetName(), errors.New("uid mismatch"))
	}
	var applied []string
	if fs, ok := md["finalizers"].([]any); ok {
		for _, f := range fs {
			applied = append(applied, f.(string))
		}
	}

	fc.mu.Lock()
	if fc.finalizers == nil {
		fc.finalizers = map[string][]string{}
	}
	object := u.GetNamespace() + "/" + u.GetName()
	key := manager + " " + object
	before := fc.finalizers[key]
	fc.finalizers[key] = applied
	// What another manager's apply set, which this apply can't remove.
	var others []string
	for k, fs := range fc.finalizers {
		if k != key && strings.HasSuffix(k, " "+object) {
			others = append(others, fs...)
		}
	}
	fc.mu.Unlock()

	finalizers := []string{}
	for _, f := range u.GetFinalizers() {
		if !slices.Contains(before, f) || slices.Contains(applied, f) || slices.Contains(others, f) {
			finalizers = append(finalizers, f)
		}
	}
	for _, f := range applied {
		if !slices.Contains(finalizers, f) {
			finalizers = append(finalizers, f)
		}
	}
	if slices.Equal(finalizers, u.GetFinalizers()) {
		// An apply that changes nothing writes nothing, so it is no event.
		return true, u, nil
	}
	u.SetFinalizers(finalizers)
	if u.GetDeletionTimestamp() != nil && len(finalizers) == 0 {
		return true, u, fc.dyn.Tracker().Delete(gvr, u.GetNamespace(), u.GetName())
	}
	return true, u, fc.dyn.Tracker().Update(gvr, u, u.GetNamespace())
}

// metadataApplies returns the applies of an object's metadata the fakes
// were sent.
func (fc *fakeCluster) metadataApplies() []apply {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var out []apply
	for _, a := range fc.applies {
		if a.subresource == "" {
			out = append(out, a)
		}
	}
	return out
}

// deleting marks the object default/name as being deleted, as the API
// server does while a finalizer holds it.
func (fc *fakeCluster) deleting(t *testing.T, name string) {
	t.Helper()
	u := fc.object(t, name).DeepCopy()
	u.SetDeletionTimestamp(kptr.To(metav1.NewTime(time.Now())))
	require.NoError(t, fc.dyn.Tracker().Update(fc.gvr, u, fc.namespace))
}

// gone reports whether the object default/name is gone from the fakes.
func (fc *fakeCluster) gone(name string) bool {
	_, err := fc.dyn.Tracker().Get(fc.gvr, fc.namespace, name)
	return apierrors.IsNotFound(err)
}

// statusApplies returns the applies of a status the fakes were sent.
func (fc *fakeCluster) statusApplies() []apply {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	var out []apply
	for _, a := range fc.applies {
		if a.subresource == "status" {
			out = append(out, a)
		}
	}
	return out
}

// object returns the object default/name as the fakes hold it.
func (fc *fakeCluster) object(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	got, err := fc.dyn.Tracker().Get(fc.gvr, fc.namespace, name)
	require.NoError(t, err)
	return got.(*unstructured.Unstructured)
}

// holder returns the holder of the controller's Lease, or "" when it has
// none or there is no Lease.
func (fc *fakeCluster) holder(t *testing.T) string {
	t.Helper()
	lease, err := fc.kube.CoordinationV1().Leases(leaseNamespace).Get(t.Context(), controllerName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ""
	}
	require.NoError(t, err)
	return kptr.Deref(lease.Spec.HolderIdentity, "")
}

// newController returns a Controller of Memcached on fc, reconciled by rec,
// built with s, settings and parts.
func newController(t *testing.T, s *humus.Setup, fc *fakeCluster, settings kube.Settings, rec kube.Reconciler[MemcachedSpec, MemcachedStatus], parts ...kube.Part) *kube.Controller {
	t.Helper()
	ctrl, err := kube.NewController(s, testClient(t, s), settings, Memcached, rec, parts...)
	require.NoError(t, err)
	kube.SetClients(ctrl, fc.dyn, fc.meta, fc.kube.CoordinationV1())
	return ctrl
}

// served is a Controller's Serve, running on a goroutine of its own.
type served struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// serve calls ctrl.Serve on a goroutine of its own, with a context the test
// cancels through the returned served.
func serve(t *testing.T, ctrl *kube.Controller) *served {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	sv := &served{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(sv.done)
		sv.err = ctrl.Serve(ctx)
	}()
	return sv
}

// returned reports whether Serve has returned.
func (sv *served) returned() bool {
	select {
	case <-sv.done:
		return true
	default:
		return false
	}
}

// wait waits for Serve to return, and returns its error.
func (sv *served) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-sv.done:
		return sv.err
	case <-time.After(settle):
		t.Fatal("Serve did not return")
		return nil
	}
}

// reconciles records each reconcile a Reconciler was handed.
type reconciles struct {
	mu   sync.Mutex
	objs []kube.Object[MemcachedSpec, MemcachedStatus]
	at   []time.Time
}

func (r *reconciles) add(obj kube.Object[MemcachedSpec, MemcachedStatus]) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.objs = append(r.objs, obj)
	r.at = append(r.at, time.Now())
	return len(r.objs)
}

func (r *reconciles) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.objs)
}

func (r *reconciles) times() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.at...)
}

// readyReplicas is a Reconciler that reports the object's size as its ready
// replicas.
func readyReplicas(got *reconciles) kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus] {
	return func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
		got.add(m)
		st := m.Status
		st.ReadyReplicas = m.Spec.Size
		return kube.Result[MemcachedStatus]{Status: &st}, nil
	}
}

func TestNewController_reports_every_mistake(t *testing.T) {
	t.Parallel()

	s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
	client := testClient(t, s)
	rec := readyReplicas(&reconciles{})

	cases := []struct {
		name     string
		settings func(*kube.Settings)
		resource func(*kube.Resource[MemcachedSpec, MemcachedStatus])
		rec      kube.Reconciler[MemcachedSpec, MemcachedStatus]
		noRec    bool
		parts    []kube.Part
		want     []string
	}{
		{name: "a bad Name", settings: func(s *kube.Settings) { s.Name = "" },
			want: []string{"Settings.Name must not be empty"}},
		{name: "a bad Namespace", settings: func(s *kube.Settings) { s.Namespace = "Default" },
			want: []string{"Settings.Namespace must be a DNS-1123 label"}},
		{name: "a bad Workers", settings: func(s *kube.Settings) { s.Workers = 0 },
			want: []string{"Settings.Workers must be at least 1"}},
		{name: "a bad Resync", settings: func(s *kube.Settings) { s.Resync = 0 },
			want: []string{"Settings.Resync must be positive"}},
		{name: "a bad RetryBaseDelay", settings: func(s *kube.Settings) { s.RetryBaseDelay = -1 },
			want: []string{"Settings.RetryBaseDelay must be positive"}},
		{name: "a bad RetryMaxDelay", settings: func(s *kube.Settings) { s.RetryMaxDelay = 0 },
			want: []string{"Settings.RetryMaxDelay must be positive"}},
		{name: "a bad Lease namespace", settings: func(s *kube.Settings) { s.LeaderElection.Namespace = "" },
			want: []string{"Settings.LeaderElection.Namespace must not be empty"}},
		{name: "a bad Identity", settings: func(s *kube.Settings) { s.LeaderElection.Identity = "" },
			want: []string{"Settings.LeaderElection.Identity must not be empty"}},
		{name: "a bad RetryPeriod", settings: func(s *kube.Settings) { s.LeaderElection.RetryPeriod = 0 },
			want: []string{"Settings.LeaderElection.RetryPeriod must be positive"}},
		{name: "a RetryMaxDelay below RetryBaseDelay", settings: func(s *kube.Settings) { s.RetryBaseDelay, s.RetryMaxDelay = time.Second, time.Millisecond },
			want: []string{"Settings.RetryMaxDelay, 1ms, must be at least Settings.RetryBaseDelay, 1s"}},
		{name: "a LeaseDuration not longer than RenewDeadline", settings: func(s *kube.Settings) { s.LeaderElection.LeaseDuration = 10 * time.Second },
			want: []string{"Settings.LeaderElection.LeaseDuration, 10s, must be longer than Settings.LeaderElection.RenewDeadline, 10s"}},
		{name: "a RenewDeadline not longer than 1.2 RetryPeriods", settings: func(s *kube.Settings) { s.LeaderElection.RetryPeriod = 9 * time.Second },
			want: []string{"Settings.LeaderElection.RenewDeadline, 10s, must be longer than 1.2 Settings.LeaderElection.RetryPeriods, 10.8s"}},
		{name: "a mistake in the Resource", resource: func(r *kube.Resource[MemcachedSpec, MemcachedStatus]) { r.Kind = "" },
			want: []string{"the CRD of memcacheds.cache.example.com: Resource.Kind must not be empty"}},
		{name: "a cluster-scoped Resource limited to a namespace",
			settings: func(s *kube.Settings) { s.Namespace = "default" },
			resource: func(r *kube.Resource[MemcachedSpec, MemcachedStatus]) { r.Scope = kube.Cluster },
			want:     []string{`Settings.Namespace is "default", but memcacheds.cache.example.com is cluster-scoped`}},
		{name: "a nil Reconciler", noRec: true,
			want: []string{"the Reconciler must not be nil"}},
		{name: "a nil ReconcilerFunc", rec: kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](nil),
			want: []string{"the Reconciler must not be a nil ReconcilerFunc"}},
		{name: "a nil Part", parts: []kube.Part{nil},
			want: []string{"parts[0] must not be nil"}},
		{name: "an Owns with no Version", parts: []kube.Part{kube.Owns(schema.GroupVersionResource{Group: "apps", Resource: "deployments"})},
			want: []string{"parts[0], Owns(apps/, Resource=deployments), has no Version"}},
		{name: "an Owns with no Resource", parts: []kube.Part{kube.Owns(deployments), kube.Owns(schema.GroupVersionResource{Version: "v1"})},
			want: []string{"parts[1], Owns(/v1, Resource=), has no Resource"}},
		{name: "an Owns with neither", parts: []kube.Part{kube.Owns(schema.GroupVersionResource{})},
			want: []string{"parts[0], Owns(/, Resource=), has no Version", "parts[0], Owns(/, Resource=), has no Resource"}},
		{name: "a Finalize with an empty name", parts: []kube.Part{kube.Finalize("", noCleanup)},
			want: []string{"parts[0], a Finalize, has an empty name"}},
		{name: "a Finalize with a nil f", parts: []kube.Part{kube.Owns(deployments), kube.Finalize[kube.Object[MemcachedSpec, MemcachedStatus]](finalizerName, nil)},
			want: []string{`parts[1], Finalize("cache.example.com/finalizer"), has a nil f`}},
		{name: "a Finalize of another type", parts: []kube.Part{kube.Finalize(finalizerName, func(context.Context, *unstructured.Unstructured) error { return nil })},
			want: []string{`parts[0], Finalize("cache.example.com/finalizer"), takes a *unstructured.Unstructured, but the Controller's objects are a kube.Object[`}},
		{name: "a Finalize of another Object", parts: []kube.Part{kube.Finalize(finalizerName, func(context.Context, kube.Object[MemcachedSpec, struct{}]) error { return nil })},
			want: []string{`parts[0], Finalize("cache.example.com/finalizer"), takes a kube.Object[`}},
		{name: "a Finalize with nothing right", parts: []kube.Part{kube.Finalize[string]("", nil)},
			want: []string{"parts[0], a Finalize, has an empty name", `parts[0], Finalize(""), has a nil f`, `parts[0], Finalize(""), takes a string, but`}},
		{name: "a second Finalize", parts: []kube.Part{kube.Finalize(finalizerName, noCleanup), kube.Owns(deployments), kube.Finalize("cache.example.com/other", noCleanup)},
			want: []string{`parts[2], Finalize("cache.example.com/other"), is a second Finalize, after parts[0]`}},
		{name: "several together",
			settings: func(s *kube.Settings) {
				s.Name = ""
				s.Workers = 0
				s.LeaderElection.Identity = ""
			},
			resource: func(r *kube.Resource[MemcachedSpec, MemcachedStatus]) { r.Group = "" },
			noRec:    true,
			want: []string{
				"Settings.Name must not be empty",
				"Settings.Workers must be at least 1",
				"Settings.LeaderElection.Identity must not be empty",
				"Resource.Group must not be empty",
				"the Reconciler must not be nil",
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			settings := testSettings()
			if tc.settings != nil {
				tc.settings(&settings)
			}
			r := Memcached
			if tc.resource != nil {
				tc.resource(&r)
			}
			var rc kube.Reconciler[MemcachedSpec, MemcachedStatus] = rec
			switch {
			case tc.noRec:
				rc = nil
			case tc.rec != nil:
				rc = tc.rec
			}
			ctrl, err := kube.NewController(s, client, settings, r, rc, tc.parts...)
			require.Nil(t, ctrl)
			require.ErrorContains(t, err, "kube: NewController: ")
			for _, want := range tc.want {
				require.ErrorContains(t, err, want)
			}
		})
	}

	t.Run("a nil Setup and a nil Client", func(t *testing.T) {
		t.Parallel()
		_, err := kube.NewController(nil, nil, testSettings(), Memcached, rec)
		require.ErrorContains(t, err, "the Setup must not be nil")
		require.ErrorContains(t, err, "the Client must not be nil")
	})

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		ctrl, err := kube.NewController(s, client, testSettings(), Memcached, rec, kube.Owns(deployments), kube.Finalize(finalizerName, noCleanup))
		require.NoError(t, err)
		require.NotNil(t, ctrl)
	})
}

func TestController_reconciles_an_object_once_for_itself_and_once_for_its_status(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var got reconciles
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		ctrl := newController(t, s, fc, testSettings(), readyReplicas(&got))

		sv := serve(t, ctrl)
		time.Sleep(settle)

		require.Equal(t, 2, got.count(), "once for the object, and once for its own status")
		require.Equal(t, MemcachedSpec{Size: 3, Image: "memcached:1.6"}, got.objs[0].Spec)
		require.Equal(t, "cache", got.objs[0].Name)
		require.Equal(t, "default", got.objs[0].Namespace)
		require.Equal(t, "Memcached", got.objs[0].Kind)
		require.Zero(t, got.objs[0].Status.ReadyReplicas)
		require.Equal(t, int32(3), got.objs[1].Status.ReadyReplicas, "the second reconcile is of the applied status")

		applies := fc.statusApplies()
		require.Len(t, applies, 1, "the second reconcile returns the status the object has, which isn't applied")
		a := applies[0]
		require.Equal(t, "default", a.namespace)
		require.Equal(t, "cache", a.name)
		require.Equal(t, controllerName, a.options.FieldManager)
		require.Equal(t, kptr.To(true), a.options.Force)
		require.Equal(t, map[string]any{
			"apiVersion": "cache.example.com/v1alpha1",
			"kind":       "Memcached",
			"metadata":   map[string]any{"name": "cache", "namespace": "default"},
			"status":     map[string]any{"readyReplicas": int64(3)},
		}, a.patch)
		require.Equal(t, map[string]any{"readyReplicas": int64(3)}, fc.object(t, "cache").Object["status"])

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestController_hands_each_reconcile_its_own_copy(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var got reconciles
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			if got.add(m) > 1 {
				return kube.Result[MemcachedStatus]{}, nil
			}
			// Changes to the copy, which the cache must not see.
			m.Labels = map[string]string{"changed": "yes"}
			m.Spec.Size = 1
			return kube.Result[MemcachedStatus]{After: time.Second}, nil
		})
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		sv := serve(t, newController(t, s, fc, testSettings(), rec))
		time.Sleep(settle)

		require.Equal(t, 2, got.count())
		require.Empty(t, got.objs[1].Labels)
		require.Equal(t, int32(3), got.objs[1].Spec.Size)
		require.Empty(t, fc.statusApplies(), "a nil Status leaves the status alone")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestController_reconciles_again_After(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var got reconciles
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			got.add(m)
			return kube.Result[MemcachedStatus]{After: 30 * time.Second}, nil
		})
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		sv := serve(t, newController(t, s, fc, testSettings(), rec))
		time.Sleep(time.Second)
		require.Equal(t, 1, got.count())

		time.Sleep(95 * time.Second)
		at := got.times()
		require.Len(t, at, 4, "at the first reconcile, and 30s, 60s and 90s after it")
		for i := 1; i < len(at); i++ {
			require.Equal(t, 30*time.Second, at[i].Sub(at[i-1]))
		}

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestController_retries_an_error_with_backoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var got reconciles
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			if got.add(m) <= 5 {
				return kube.Result[MemcachedStatus]{}, errors.New("the Deployment wasn't applied")
			}
			return kube.Result[MemcachedStatus]{}, nil
		})
		settings := testSettings()
		settings.RetryBaseDelay = time.Second
		settings.RetryMaxDelay = 4 * time.Second
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, settings, rec))
		time.Sleep(settle)

		at := got.times()
		require.Len(t, at, 6, "five failures, then a success, which is forgotten")
		var gaps []time.Duration
		for i := 1; i < len(at); i++ {
			gaps = append(gaps, at[i].Sub(at[i-1]))
		}
		require.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}, gaps,
			"doubling from RetryBaseDelay to RetryMaxDelay")
		require.False(t, sv.returned(), "a reconcile error never ends the Service")

		var failed int
		for _, span := range tel.Spans() {
			if span.Name() == "reconcile Memcached" && span.Status().Code == codes.Error {
				require.Equal(t, "the Deployment wasn't applied", span.Status().Description)
				failed++
			}
		}
		require.Equal(t, 5, failed)
		var warned int
		for _, r := range tel.Logs() {
			if r.Body().AsString() != "reconcile failed" {
				continue
			}
			warned++
			require.Equal(t, log.SeverityWarn, r.Severity())
			require.True(t, r.TraceID().IsValid(), "in the span's context")
			attrs := recordAttrs(&r)
			require.Equal(t, "default", attrs["k8s.namespace.name"])
			require.Equal(t, "cache", attrs["humus.kube.object.name"])
			require.Equal(t, "the Deployment wasn't applied", attrs["exception.message"])
		}
		require.Equal(t, 5, warned)

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestController_applies_the_status_on_an_error(t *testing.T) {
	t.Parallel()

	t.Run("a status", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFakeCluster(t, memcachedObject("cache", 3))
			var got reconciles
			rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
				got.add(m)
				st := m.Status
				st.Conditions = []metav1.Condition{{
					Type: "Available", Status: metav1.ConditionUnknown, Reason: "ApplyFailed", Message: "refused",
					LastTransitionTime: metav1.NewTime(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)),
				}}
				return kube.Result[MemcachedStatus]{Status: &st}, errors.New("refused")
			})
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			sv := serve(t, newController(t, s, fc, testSettings(), rec))
			time.Sleep(settle)

			applies := fc.statusApplies()
			require.Len(t, applies, 1, "applied once, and then equal to the object's")
			conditions := fc.object(t, "cache").Object["status"].(map[string]any)["conditions"].([]any)
			require.Len(t, conditions, 1)
			require.Equal(t, "ApplyFailed", conditions[0].(map[string]any)["reason"])
			require.Greater(t, got.count(), 2, "and the error is retried")

			sv.cancel()
			require.NoError(t, sv.wait(t))
		})
	})

	t.Run("no status", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFakeCluster(t, memcachedObject("cache", 3))
			var got reconciles
			rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
				got.add(m)
				return kube.Result[MemcachedStatus]{}, errors.New("refused")
			})
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			sv := serve(t, newController(t, s, fc, testSettings(), rec))
			time.Sleep(settle)

			require.Greater(t, got.count(), 2)
			require.Empty(t, fc.statusApplies(), "a nil Status leaves the status alone")
			require.NotContains(t, fc.object(t, "cache").Object, "status")

			sv.cancel()
			require.NoError(t, sv.wait(t))
		})
	})

	t.Run("a failed apply", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFakeCluster(t, memcachedObject("cache", 3))
			fc.dyn.PrependReactor("patch", Memcached.Plural, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(Memcached.GVR().GroupResource(), "cache", errors.New("no RBAC"))
			})
			var got reconciles
			tel := humustest.NewTelemetry()
			s := newSetup(t, humus.Host{Dial: noDial}, tel)
			sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got)))
			time.Sleep(settle)

			require.Greater(t, got.count(), 2, "a failed apply is retried, as an error is")
			var failed bool
			for _, span := range tel.Spans() {
				if span.Status().Code == codes.Error {
					require.Contains(t, span.Status().Description, "kube: applying the status of default/cache: ")
					failed = true
				}
			}
			require.True(t, failed)

			sv.cancel()
			require.NoError(t, sv.wait(t))
		})
	})
}

func TestController_leaves_an_object_alone(t *testing.T) {
	t.Parallel()

	t.Run("being deleted", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			obj := memcachedObject("cache", 3)
			obj.SetDeletionTimestamp(kptr.To(metav1.NewTime(time.Now())))
			obj.SetFinalizers([]string{"example.com/someone-elses"})
			fc := newFakeCluster(t, obj)
			var got reconciles
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got)))
			time.Sleep(settle)

			require.Zero(t, got.count())
			sv.cancel()
			require.NoError(t, sv.wait(t))
		})
	})

	t.Run("gone", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFakeCluster(t, memcachedObject("cache", 3))
			var got reconciles
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got)))
			time.Sleep(settle)
			require.Equal(t, 2, got.count())

			require.NoError(t, fc.dyn.Resource(Memcached.GVR()).Namespace("default").Delete(t.Context(), "cache", metav1.DeleteOptions{}))
			time.Sleep(settle)
			require.Equal(t, 2, got.count(), "an object gone from the cache needs nothing")

			sv.cancel()
			require.NoError(t, sv.wait(t))
		})
	})
}

func TestController_holds_the_Lease_and_releases_it_on_shutdown(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var got reconciles
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		ctrl := newController(t, s, fc, testSettings(), readyReplicas(&got))
		sv := serve(t, ctrl)
		time.Sleep(settle)

		require.Equal(t, identity, fc.holder(t))

		require.NoError(t, ctrl.Shutdown(t.Context()))
		require.NoError(t, sv.wait(t), "Serve has returned nil")
		require.Empty(t, fc.holder(t), "the Lease is released")
	})
}

func TestController_shutdown_leaves_the_backlog(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("a", 1), memcachedObject("b", 2), memcachedObject("c", 3))
		var got reconciles
		inFlight, finish := make(chan struct{}), make(chan struct{})
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			if got.add(m) == 1 {
				close(inFlight)
				<-finish
			}
			return kube.Result[MemcachedStatus]{}, ctx.Err()
		})
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		sv := serve(t, newController(t, s, fc, testSettings(), rec))
		<-inFlight

		sv.cancel()
		synctest.Wait()
		require.False(t, sv.returned(), "Serve waits for the reconcile in flight")
		require.Equal(t, identity, fc.holder(t), "and holds the Lease while it does")

		close(finish)
		require.NoError(t, sv.wait(t))
		require.Equal(t, 1, got.count(), "only the reconcile in flight ran; the two queued keys are left")
		require.Empty(t, fc.holder(t), "the Lease is released")
	})
}

func TestController_Shutdown_cancels_the_reconciles_in_flight_when_its_context_is_done(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		inFlight := make(chan struct{})
		var cause error
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(ctx context.Context, _ kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			close(inFlight)
			<-ctx.Done()
			cause = ctx.Err()
			return kube.Result[MemcachedStatus]{}, ctx.Err()
		})
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		ctrl := newController(t, s, fc, testSettings(), rec)
		sv := serve(t, ctrl)
		<-inFlight

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		start := time.Now()
		err := ctrl.Shutdown(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "kube: shutting down the Controller: ")
		require.Equal(t, 5*time.Second, time.Since(start), "Shutdown waited for its context")
		require.ErrorIs(t, cause, context.Canceled, "the reconcile in flight was cancelled")
		require.NoError(t, sv.wait(t), "Serve has returned nil")
	})
}

func TestController_Serve_returns_ErrLostLeadership(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		inFlight := make(chan struct{})
		var cause error
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(ctx context.Context, _ kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			close(inFlight)
			<-ctx.Done()
			cause = ctx.Err()
			return kube.Result[MemcachedStatus]{}, ctx.Err()
		})
		// Once unreachable is set, every renewal fails, as it does while the
		// API server is unreachable.
		var unreachable atomic.Bool
		fc.kube.PrependReactor("update", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			if !unreachable.Load() {
				return false, nil, nil
			}
			return true, nil, errors.New("the API server is unreachable")
		})
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		sv := serve(t, newController(t, s, fc, testSettings(), rec))
		<-inFlight

		lost := time.Now()
		unreachable.Store(true)
		err := sv.wait(t)
		require.ErrorIs(t, err, kube.ErrLostLeadership)
		require.LessOrEqual(t, time.Since(lost), 10*time.Second+2*time.Second,
			"within RenewDeadline, after the last renewal")
		require.ErrorIs(t, cause, context.Canceled, "the reconcile in flight was cancelled")
	})
}

func TestController_Serve_fails_on_a_resource_that_isnt_served(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t)
		fc.dyn.PrependReactor("list", Memcached.Plural, func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(Memcached.GVR().GroupResource(), "")
		})
		ctrl := newController(t, newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry()), fc, testSettings(), readyReplicas(&reconciles{}))
		err := ctrl.Serve(t.Context())
		require.ErrorContains(t, err, "kube: listing cache.example.com/v1alpha1, Resource=memcacheds: ")
		require.True(t, apierrors.IsNotFound(err))
		require.Empty(t, fc.holder(t), "no Lease was campaigned for")
	})
}

func TestController_Synced(t *testing.T) {
	t.Parallel()

	t.Run("on the leader", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFakeCluster(t, memcachedObject("cache", 3))
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			ctrl := newController(t, s, fc, testSettings(), readyReplicas(&reconciles{}))
			synced := ctrl.Synced()
			requireHealthy(t, synced, false, "before Serve")

			sv := serve(t, ctrl)
			time.Sleep(settle)
			requireHealthy(t, synced, true, "once the cache has synced")

			sv.cancel()
			require.NoError(t, sv.wait(t))
			requireHealthy(t, synced, false, "once Serve has returned")
		})
	})

	t.Run("on a replica that doesn't lead", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFakeCluster(t, memcachedObject("cache", 3))
			// Another replica holds the Lease.
			now := metav1.NewMicroTime(time.Now())
			_, err := fc.kube.CoordinationV1().Leases(leaseNamespace).Create(t.Context(), &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: controllerName, Namespace: leaseNamespace},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       kptr.To("replica-2"),
					LeaseDurationSeconds: kptr.To[int32](15),
					AcquireTime:          &now,
					RenewTime:            &now,
				},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			var got reconciles
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			ctrl := newController(t, s, fc, testSettings(), readyReplicas(&got))
			sv := serve(t, ctrl)
			// Less than the LeaseDuration, which replica-2 doesn't renew.
			time.Sleep(10 * time.Second)

			requireHealthy(t, ctrl.Synced(), true, "on every replica, leader or not")
			require.Zero(t, got.count(), "only the Lease's holder reconciles")
			require.Equal(t, "replica-2", fc.holder(t))

			sv.cancel()
			require.NoError(t, sv.wait(t))
			require.Equal(t, "replica-2", fc.holder(t), "a replica releases only its own Lease")
		})
	})

	t.Run("before Serve fails", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFakeCluster(t)
			fc.dyn.PrependReactor("list", Memcached.Plural, func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("refused")
			})
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			ctrl := newController(t, s, fc, testSettings(), readyReplicas(&reconciles{}))
			require.Error(t, ctrl.Serve(t.Context()))
			requireHealthy(t, ctrl.Synced(), false, "a Serve that failed")
			require.NoError(t, ctrl.Shutdown(t.Context()))
			requireHealthy(t, ctrl.Synced(), false, "after Shutdown")
		})
	})
}

// requireHealthy fails the test unless m reports want.
func requireHealthy(t *testing.T, m interface {
	Healthy(context.Context) (bool, error)
}, want bool, when string) {
	t.Helper()
	healthy, err := m.Healthy(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, healthy, when)
}

func TestController_telemetry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var got reconciles
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		spanCtx := make(chan bool, 2)
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			spanCtx <- spanFromContext(ctx)
			return readyReplicas(&got)(ctx, m)
		})
		sv := serve(t, newController(t, s, fc, testSettings(), rec))
		time.Sleep(settle)
		sv.cancel()
		require.NoError(t, sv.wait(t))

		require.True(t, <-spanCtx, "Reconcile's context carries the reconcile's span")
		var spans int
		for _, span := range tel.Spans() {
			if span.InstrumentationScope().Name != "github.com/z5labs/humus/kube" {
				continue
			}
			spans++
			require.Equal(t, "reconcile Memcached", span.Name())
			require.False(t, span.Parent().IsValid(), "a root span")
			require.Equal(t, codes.Unset, span.Status().Code)
			require.ElementsMatch(t, []attribute.KeyValue{
				attribute.String("k8s.namespace.name", "default"),
				attribute.String("humus.kube.object.name", "cache"),
			}, span.Attributes())
		}
		require.Equal(t, 2, spans, "every reconcile is a span")

		bodies := map[string]bool{}
		for _, r := range tel.Logs() {
			require.Equal(t, "github.com/z5labs/humus/kube", r.InstrumentationScope().Name)
			bodies[r.Body().AsString()] = true
		}
		require.True(t, bodies["Attempting to acquire leader lease..."], "leader election's records reach the LoggerProvider: %v", bodies)
		require.True(t, bodies["Successfully acquired lease"], "leader election's records reach the LoggerProvider: %v", bodies)
		require.True(t, bodies["Released lease"], "%v", bodies)
	})
}

func TestController_is_a_humus_Service(t *testing.T) {
	t.Parallel()
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var got reconciles
		return newController(t, s, fc, testSettings(), readyReplicas(&got))
	})
}

// spanFromContext reports whether ctx carries a span.
func spanFromContext(ctx context.Context) bool {
	return trace.SpanFromContext(ctx).SpanContext().IsValid()
}

// recordAttrs returns r's attributes, each by its key, as a string.
func recordAttrs(r interface {
	WalkAttributes(func(attribute.KeyValue) bool)
}) map[string]string {
	attrs := map[string]string{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value.String()
		return true
	})
	return attrs
}
