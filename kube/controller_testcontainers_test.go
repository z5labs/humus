//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

// The tests in this file run a whole App, on humus.OSHost() and humustest's
// process, whose Controller reconciles Memcacheds in the k3s that
// kube_testcontainers_test.go starts. kube_testcontainers_test.go says why
// they sit behind the testcontainers build tag.

import (
	"context"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	apiwait "k8s.io/apimachinery/pkg/util/wait"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// appWaitLimit bounds every wait for the App: to reconcile, and to exit.
const appWaitLimit = 2 * time.Minute

// deployer is the end user's Reconciler of the kubebuilder book's
// Memcached operator: it applies a Deployment that the Memcached controls,
// and reports the Deployment's ready replicas, and a condition.
type deployer struct {
	kube kubernetes.Interface
}

func (r deployer) Reconcile(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
	labels := map[string]string{"app": "memcached", "memcached": m.Name}
	d, err := r.kube.AppsV1().Deployments(m.Namespace).Apply(ctx,
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
		metav1.ApplyOptions{FieldManager: controllerName, Force: true},
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
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: "Available", Status: metav1.ConditionTrue, Reason: "DeploymentApplied",
		ObservedGeneration: m.Generation,
	})
	return kube.Result[MemcachedStatus]{Status: &st}, nil
}

// controllerSettings returns the settings of the Controller the Apps run,
// whose Lease is in the default namespace.
func controllerSettings() kube.Settings {
	s := testSettings()
	s.LeaderElection.Namespace = "default"
	return s
}

// startController starts an App on humus.OSHost() whose only Service is a
// Controller of r, with parts, reconciled by a deployer, in the shared k3s.
func startController(t *testing.T, r kube.Resource[MemcachedSpec, MemcachedStatus], parts ...kube.Part) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	k3sClient(t) // k3s is started
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
}

// appExit waits for the App's exit status.
func appExit(t *testing.T, status <-chan bedrock.ExitStatus) bedrock.ExitStatus {
	t.Helper()
	select {
	case st := <-status:
		return st
	case <-time.After(appWaitLimit):
		t.Fatal("the App did not exit")
		return bedrock.ExitStatus{}
	}
}

// hasManager reports whether fields has an entry of manager's, of op, on
// subresource.
func hasManager(fields []metav1.ManagedFieldsEntry, manager string, op metav1.ManagedFieldsOperationType, subresource string) bool {
	return slices.ContainsFunc(fields, func(f metav1.ManagedFieldsEntry) bool {
		return f.Manager == manager && f.Operation == op && f.Subresource == subresource
	})
}

func TestController_reconciles_a_Memcached_in_a_whole_App(t *testing.T) {
	ctx := t.Context()
	client := k3sClient(t)

	// A group of its own, so the CRDs the other tests delete aren't in the
	// way while they terminate.
	r := Memcached
	r.Group = "controller.example.com"
	crd, err := r.CRD()
	require.NoError(t, err)
	createCRD(t, client, crd)

	dyn, err := dynamic.NewForConfig(client.Config())
	require.NoError(t, err)
	kc, err := kubernetes.NewForConfig(client.Config())
	require.NoError(t, err)
	memcacheds := dyn.Resource(r.GVR()).Namespace("default")
	obj := memcached("cache", map[string]any{"size": int64(2), "image": "memcached:1.6"})
	obj.SetAPIVersion("controller.example.com/v1alpha1")
	_, err = memcacheds.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"})
	require.NoError(t, err)

	p, status := startController(t, r)

	// The Deployment is applied, and then the Memcached's status.
	var got kube.Object[MemcachedSpec, MemcachedStatus]
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, appWaitLimit, true, func(ctx context.Context) (bool, error) {
		u, err := memcacheds.Get(ctx, "cache", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		got = kube.Object[MemcachedSpec, MemcachedStatus]{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &got); err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(got.Status.Conditions, "Available"), nil
	})
	require.NoError(t, err, "the Memcached's status was never applied")
	require.True(t, hasManager(got.ManagedFields, controllerName, metav1.ManagedFieldsOperationApply, "status"),
		"the status is applied under Settings.Name: %+v", got.ManagedFields)

	d, err := kc.AppsV1().Deployments("default").Get(ctx, "cache", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, int32(2), *d.Spec.Replicas)
	require.True(t, hasManager(d.ManagedFields, controllerName, metav1.ManagedFieldsOperationApply, ""),
		"the Deployment is applied under Settings.Name: %+v", d.ManagedFields)
	owner := metav1.GetControllerOf(d)
	require.NotNil(t, owner)
	require.Equal(t, got.UID, owner.UID, "the Memcached controls the Deployment")

	lease, err := kc.CoordinationV1().Leases("default").Get(ctx, controllerName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, identity, *lease.Spec.HolderIdentity)

	p.Signal(syscall.SIGTERM)
	st := appExit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())

	lease, err = kc.CoordinationV1().Leases("default").Get(ctx, controllerName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, lease.Spec.HolderIdentity, "the Lease is left without a holder")
}

func TestController_App_exits_1_when_the_CRD_isnt_installed(t *testing.T) {
	r := Memcached
	r.Group = "absent.example.com"
	_, status := startController(t, r)

	st := appExit(t, status)
	require.Equal(t, 1, st.Code())
	require.ErrorContains(t, st.Cause(), "kube: listing absent.example.com/v1alpha1, Resource=memcacheds: ")
}

func TestController_undoes_a_hand_made_scale_of_a_Deployment_it_owns(t *testing.T) {
	ctx := t.Context()
	client := k3sClient(t)

	r := Memcached
	r.Group = "owns.example.com"
	crd, err := r.CRD()
	require.NoError(t, err)
	createCRD(t, client, crd)

	dyn, err := dynamic.NewForConfig(client.Config())
	require.NoError(t, err)
	kc, err := kubernetes.NewForConfig(client.Config())
	require.NoError(t, err)
	obj := memcached("cache", map[string]any{"size": int64(2), "image": "memcached:1.6"})
	obj.SetAPIVersion("owns.example.com/v1alpha1")
	_, err = dyn.Resource(r.GVR()).Namespace("default").Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"})
	require.NoError(t, err)

	p, status := startController(t, r, kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments")))

	deployments := kc.AppsV1().Deployments("default")
	replicas := func(want int32) func(context.Context) (bool, error) {
		return func(ctx context.Context) (bool, error) {
			d, err := deployments.Get(ctx, "cache", metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return d.Spec.Replicas != nil && *d.Spec.Replicas == want, nil
		}
	}
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, appWaitLimit, true, replicas(2))
	require.NoError(t, err, "the Deployment was never applied")

	// A scale by hand, as kubectl scale makes it. Nothing about the
	// Memcached changes, and its resync is 10h away, so only Owns brings
	// the controller back to the Deployment.
	scaled, err := deployments.Patch(ctx, "cache", types.MergePatchType, []byte(`{"spec":{"replicas":5}}`),
		metav1.PatchOptions{FieldManager: "kubectl-scale"})
	require.NoError(t, err)
	require.Equal(t, int32(5), *scaled.Spec.Replicas)

	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, replicas(2))
	require.NoError(t, err, "the hand-made scale was never undone")

	p.Signal(syscall.SIGTERM)
	st := appExit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

func TestController_runs_its_finalizer_when_a_Memcached_is_deleted(t *testing.T) {
	ctx := t.Context()
	client := k3sClient(t)

	r := Memcached
	r.Group = "finalize.example.com"
	crd, err := r.CRD()
	require.NoError(t, err)
	createCRD(t, client, crd)

	dyn, err := dynamic.NewForConfig(client.Config())
	require.NoError(t, err)
	memcacheds := dyn.Resource(r.GVR()).Namespace("default")
	obj := memcached("cache", map[string]any{"size": int64(2), "image": "memcached:1.6"})
	obj.SetAPIVersion("finalize.example.com/v1alpha1")
	_, err = memcacheds.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"})
	require.NoError(t, err)

	var cleaned atomic.Int32
	p, status := startController(t, r, kube.Finalize(finalizerName, func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) error {
		if m.Name == "cache" && m.DeletionTimestamp != nil {
			cleaned.Add(1)
		}
		return nil
	}))

	// The finalizer is applied, and then the Memcached is reconciled.
	var got kube.Object[MemcachedSpec, MemcachedStatus]
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, appWaitLimit, true, func(ctx context.Context) (bool, error) {
		u, err := memcacheds.Get(ctx, "cache", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		got = kube.Object[MemcachedSpec, MemcachedStatus]{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &got); err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(got.Status.Conditions, "Available"), nil
	})
	require.NoError(t, err, "the Memcached was never reconciled")
	require.Equal(t, []string{finalizerName}, got.Finalizers)
	require.True(t, hasManager(got.ManagedFields, controllerName, metav1.ManagedFieldsOperationApply, ""),
		"the finalizer is applied under Settings.Name: %+v", got.ManagedFields)
	require.Zero(t, cleaned.Load())

	require.NoError(t, memcacheds.Delete(ctx, "cache", metav1.DeleteOptions{}))
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, appWaitLimit, true, func(ctx context.Context) (bool, error) {
		_, err := memcacheds.Get(ctx, "cache", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	require.NoError(t, err, "the Memcached was never deleted")
	require.Equal(t, int32(1), cleaned.Load(), "f ran once")

	p.Signal(syscall.SIGTERM)
	st := appExit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.Equal(t, int32(1), cleaned.Load(), "and never again")
}
