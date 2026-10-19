//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

// The tests in this file run a whole App with failure injection, whose
// Controller reconciles Memcacheds in the k3s that
// kube_testcontainers_test.go starts, and carries the scenario a
// Memcached's annotation holds into its reconciles. client-go's fakes never
// see a request's context, so only a real API server shows the Client's
// points meeting a reconcile's scenario. kube_testcontainers_test.go says
// why they sit behind the testcontainers build tag.

import (
	"context"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	apiwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// attempts records when trust was asked about an object, which is once
// each reconcile of an object whose annotation carries a scenario, and
// trusts every object.
type attempts struct {
	mu sync.Mutex
	at []time.Time
}

func (a *attempts) trust(metav1.Object) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.at = append(a.at, time.Now())
	return true
}

func (a *attempts) times() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Time(nil), a.at...)
}

// startFaultsController starts an App on humus.OSHost() with failure
// injection from the tests' codec, whose only Service is a Controller of r,
// with settings and parts, reconciled by a deployer, in the shared k3s.
func startFaultsController(t *testing.T, r kube.Resource[MemcachedSpec, MemcachedStatus], settings kube.Settings, parts ...kube.Part) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	k3sClient(t) // k3s is started
	codec := faultsCodec(t)
	return humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		client, err := kube.NewClient(s, cluster.conn)
		if err != nil {
			return nil, err
		}
		kc, err := kubernetes.NewForConfig(client.Config())
		if err != nil {
			return nil, err
		}
		ctrl, err := kube.NewController(s, client, settings, r, deployer{kube: kc}, parts...)
		if err != nil {
			return nil, err
		}
		return []humus.Service{ctrl}, nil
	}, humustest.WithApp(
		humus.WithHost(humus.OSHost()),
		humus.WithFaults(func(context.Context, *humus.Setup) (fault.Settings, error) {
			settings := fault.DefaultSettings()
			settings.Decoder = codec
			return settings, nil
		}),
	))
}

// removeAnnotation removes the annotation scenarioAnnotation from the
// Memcached default/name, as kubectl annotate name key- does.
func removeAnnotation(t *testing.T, memcacheds dynamic.ResourceInterface, name string) {
	t.Helper()
	_, err := memcacheds.Patch(t.Context(), name, types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"`+scenarioAnnotation+`":null}}}`),
		metav1.PatchOptions{FieldManager: "kubectl-annotate"})
	require.NoError(t, err)
}

// getMemcached returns the Memcached default/name.
func getMemcached(ctx context.Context, memcacheds dynamic.ResourceInterface, name string) (kube.Object[MemcachedSpec, MemcachedStatus], error) {
	var got kube.Object[MemcachedSpec, MemcachedStatus]
	u, err := memcacheds.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return got, err
	}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &got)
	return got, err
}

func TestTrustScenario_an_annotated_Memcached_fails_its_Deployments_apply_until_the_annotation_is_removed(t *testing.T) {
	ctx := t.Context()
	client := k3sClient(t)

	r := Memcached
	r.Group = "fit.example.com"
	crd, err := r.CRD()
	require.NoError(t, err)
	createCRD(t, client, crd)

	dyn, err := dynamic.NewForConfig(client.Config())
	require.NoError(t, err)
	kc, err := kubernetes.NewForConfig(client.Config())
	require.NoError(t, err)
	memcacheds := dyn.Resource(r.GVR()).Namespace("default")
	// A name of its own, so a Deployment another test's Memcached left for
	// the garbage collector isn't in the way.
	const point = "kube:deployments.apps:patch"
	obj := annotated(memcached("fit-deployment", map[string]any{"size": int64(2), "image": "memcached:1.6"}),
		scenarioValue(t, abort(point)))
	obj.SetAPIVersion("fit.example.com/v1alpha1")
	_, err = memcacheds.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"})
	require.NoError(t, err)

	settings := controllerSettings()
	settings.RetryBaseDelay = 100 * time.Millisecond
	settings.RetryMaxDelay = 2 * time.Second
	var tried attempts
	p, status := startFaultsController(t, r, settings, kube.TrustScenario(scenarioAnnotation, tried.trust))

	// Each reconcile fails, and the Reconciler's failure condition is
	// applied, since the status's apply isn't aborted.
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, appWaitLimit, true, func(ctx context.Context) (bool, error) {
		got, err := getMemcached(ctx, memcacheds, "fit-deployment")
		if err != nil {
			return false, err
		}
		c := meta.FindStatusCondition(got.Status.Conditions, "Available")
		return c != nil && c.Reason == "ApplyFailed", nil
	})
	require.NoError(t, err, "the Reconciler's failure condition was never applied")
	got, err := getMemcached(ctx, memcacheds, "fit-deployment")
	require.NoError(t, err)
	c := meta.FindStatusCondition(got.Status.Conditions, "Available")
	require.Equal(t, metav1.ConditionUnknown, c.Status)
	require.Contains(t, c.Message, "fit: injected failure at "+point)
	_, err = kc.AppsV1().Deployments("default").Get(ctx, "fit-deployment", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "the Deployment is never applied: %v", err)

	// The object is retried with backoff: its retries keep coming, further
	// and further apart.
	err = apiwait.PollUntilContextTimeout(ctx, 100*time.Millisecond, appWaitLimit, true, func(context.Context) (bool, error) {
		return len(tried.times()) >= 6, nil
	})
	require.NoError(t, err, "the object was not retried")
	at := tried.times()
	first, last := at[1].Sub(at[0]), at[len(at)-1].Sub(at[len(at)-2])
	require.Greater(t, last, first, "the retries back off: %v", at)
	require.GreaterOrEqual(t, last, 2*settings.RetryBaseDelay, "the retries back off: %v", at)

	// Removing the annotation ends the experiment: the next reconcile
	// applies the Deployment.
	removeAnnotation(t, memcacheds, "fit-deployment")
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, appWaitLimit, true, func(ctx context.Context) (bool, error) {
		got, err := getMemcached(ctx, memcacheds, "fit-deployment")
		if err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(got.Status.Conditions, "Available"), nil
	})
	require.NoError(t, err, "the Memcached was never reconciled without the scenario")
	d, err := kc.AppsV1().Deployments("default").Get(ctx, "fit-deployment", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, int32(2), *d.Spec.Replicas)

	p.Signal(syscall.SIGTERM)
	st := appExit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

func TestTrustScenario_an_annotated_Memcached_gets_no_finalizer_until_the_annotation_is_removed(t *testing.T) {
	ctx := t.Context()
	client := k3sClient(t)

	r := Memcached
	r.Group = "fitfinalize.example.com"
	crd, err := r.CRD()
	require.NoError(t, err)
	createCRD(t, client, crd)

	dyn, err := dynamic.NewForConfig(client.Config())
	require.NoError(t, err)
	memcacheds := dyn.Resource(r.GVR()).Namespace("default")
	const point = "kube:memcacheds.fitfinalize.example.com:patch"
	obj := annotated(memcached("fit-finalizer", map[string]any{"size": int64(2), "image": "memcached:1.6"}),
		scenarioValue(t, abort(point)))
	obj.SetAPIVersion("fitfinalize.example.com/v1alpha1")
	_, err = memcacheds.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"})
	require.NoError(t, err)

	settings := controllerSettings()
	settings.RetryBaseDelay = 100 * time.Millisecond
	settings.RetryMaxDelay = 2 * time.Second
	var tried attempts
	p, status := startFaultsController(t, r, settings,
		kube.Finalize(finalizerName, noCleanup),
		kube.TrustScenario(scenarioAnnotation, tried.trust),
	)

	// The finalizer's apply is aborted, again and again, while the
	// annotation stays.
	err = apiwait.PollUntilContextTimeout(ctx, 100*time.Millisecond, appWaitLimit, true, func(context.Context) (bool, error) {
		return len(tried.times()) >= 4, nil
	})
	require.NoError(t, err, "the object was not retried")
	got, err := getMemcached(ctx, memcacheds, "fit-finalizer")
	require.NoError(t, err)
	require.Empty(t, got.Finalizers, "no finalizer while the annotation stays")
	require.Empty(t, got.Status.Conditions, "and the Reconciler is never called")

	removeAnnotation(t, memcacheds, "fit-finalizer")
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, appWaitLimit, true, func(ctx context.Context) (bool, error) {
		got, err = getMemcached(ctx, memcacheds, "fit-finalizer")
		if err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(got.Status.Conditions, "Available"), nil
	})
	require.NoError(t, err, "the Memcached was never reconciled without the scenario")
	require.Equal(t, []string{finalizerName}, got.Finalizers, "the finalizer is added once the annotation is removed")

	// Deleted while the App runs, so the finalizer doesn't hold it, and its
	// CRD, past the test.
	require.NoError(t, memcacheds.Delete(ctx, "fit-finalizer", metav1.DeleteOptions{}))
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, appWaitLimit, true, func(ctx context.Context) (bool, error) {
		_, err := memcacheds.Get(ctx, "fit-finalizer", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	require.NoError(t, err, "the Memcached was never deleted")

	p.Signal(syscall.SIGTERM)
	st := appExit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}
