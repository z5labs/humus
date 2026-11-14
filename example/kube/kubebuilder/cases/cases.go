// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the Memcached operator's black-box cases, run against a cluster both operators reconcile.
package cases

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	finalizer = "cache.example.com/finalizer"
	image     = "memcached:1.6.26-alpine3.19"
	poll      = 250 * time.Millisecond
)

var memcacheds = schema.GroupVersionResource{Group: "cache.example.com", Version: "v1alpha1", Resource: "memcacheds"}

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

func Deployment(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)

	m := c.create(ctx, t, "cache", 2)
	c.awaitAvailable(ctx, t, "cache", 2)

	d := c.deployment(ctx, t, "cache")
	require.Equal(t, int32(2), *d.Spec.Replicas)
	require.Len(t, d.Spec.Template.Spec.Containers, 1)
	require.Equal(t, image, d.Spec.Template.Spec.Containers[0].Image)
	require.Equal(t, int32(11211), d.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort)
	owner := metav1.GetControllerOf(d)
	require.NotNil(t, owner, "the Memcached controls the Deployment")
	require.Equal(t, "Memcached", owner.Kind)
	require.Equal(t, m.GetUID(), owner.UID)

	got := c.memcached(ctx, t, "cache")
	require.Contains(t, got.GetFinalizers(), finalizer)
}

func Resize(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)

	c.create(ctx, t, "cache", 1)
	c.awaitAvailable(ctx, t, "cache", 1)

	_, err := c.dynamic.Resource(memcacheds).Namespace(c.namespace).Patch(ctx, "cache", types.MergePatchType, []byte(`{"spec":{"size":3}}`), metav1.PatchOptions{})
	require.NoError(t, err)

	c.awaitReplicas(ctx, t, "cache", 3)
	c.awaitAvailable(ctx, t, "cache", 3)
}

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

func DeploymentDeleted(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)

	c.create(ctx, t, "cache", 2)
	c.awaitAvailable(ctx, t, "cache", 2)
	old := c.deployment(ctx, t, "cache")

	err := c.kube.AppsV1().Deployments(c.namespace).Delete(ctx, "cache", metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &old.UID},
	})
	require.NoError(t, err)

	c.await(ctx, t, "a new Deployment", func(ctx context.Context) (bool, error) {
		d, err := c.kube.AppsV1().Deployments(c.namespace).Get(ctx, "cache", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return d.UID != old.UID && d.Spec.Replicas != nil && *d.Spec.Replicas == 2, nil
	})
}

func Validation(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)
	resource := c.dynamic.Resource(memcacheds).Namespace(c.namespace)

	for _, size := range []int64{0, 4} {
		_, err := resource.Create(ctx, memcached(fmt.Sprintf("size-%d", size), size), metav1.CreateOptions{})
		require.True(t, apierrors.IsInvalid(err), "a size of %d: %v", size, err)
		require.ErrorContains(t, err, "spec.size")
	}
}

func Deletion(t testing.TB, target Target) {
	ctx := bounded(t, target)
	c := newCluster(ctx, t, target)

	m := c.create(ctx, t, "cache", 2)
	c.awaitAvailable(ctx, t, "cache", 2)
	c.await(ctx, t, "the finalizer", func(ctx context.Context) (bool, error) {
		got, err := c.dynamic.Resource(memcacheds).Namespace(c.namespace).Get(ctx, "cache", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return slices.Contains(got.GetFinalizers(), finalizer), nil
	})

	err := c.dynamic.Resource(memcacheds).Namespace(c.namespace).Delete(ctx, "cache", metav1.DeleteOptions{})
	require.NoError(t, err)

	c.await(ctx, t, "the Memcached to be gone", func(ctx context.Context) (bool, error) {
		_, err := c.dynamic.Resource(memcacheds).Namespace(c.namespace).Get(ctx, "cache", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	c.await(ctx, t, "the Deployment to be garbage collected", func(ctx context.Context) (bool, error) {
		_, err := c.kube.AppsV1().Deployments(c.namespace).Get(ctx, "cache", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})

	var deleting eventsv1.Event
	c.await(ctx, t, "the Deleting Event", func(ctx context.Context) (bool, error) {
		list, err := c.kube.EventsV1().Events(c.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for _, e := range list.Items {
			if e.Reason == "Deleting" && e.Regarding.UID == m.GetUID() {
				deleting = e
				return true, nil
			}
		}
		return false, nil
	})
	require.Equal(t, corev1.EventTypeWarning, deleting.Type)
	require.Equal(t, "DeleteCR", deleting.Action)
	require.Equal(t, "memcached-controller", deleting.ReportingController)
	require.Equal(t, "Memcached", deleting.Regarding.Kind)
	require.Equal(t, "cache", deleting.Regarding.Name)
	require.Equal(t, "Custom Resource cache is being deleted from the namespace "+c.namespace, deleting.Note)
}

func bounded(t testing.TB, target Target) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	t.Cleanup(cancel)
	return ctx
}

type cluster struct {
	kube      kubernetes.Interface
	dynamic   dynamic.Interface
	namespace string
}

func newCluster(ctx context.Context, t testing.TB, target Target) *cluster {
	t.Helper()
	kc, err := kubernetes.NewForConfig(target.Config)
	require.NoError(t, err)
	dc, err := dynamic.NewForConfig(target.Config)
	require.NoError(t, err)

	ns, err := kc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: target.Prefix + strings.ToLower(rand.Text()[:8])},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = kc.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})
	return &cluster{kube: kc, dynamic: dc, namespace: ns.Name}
}

func memcached(name string, size int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cache.example.com/v1alpha1",
		"kind":       "Memcached",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"size": size},
	}}
}

func (c *cluster) create(ctx context.Context, t testing.TB, name string, size int64) *unstructured.Unstructured {
	t.Helper()
	m, err := c.dynamic.Resource(memcacheds).Namespace(c.namespace).Create(ctx, memcached(name, size), metav1.CreateOptions{})
	require.NoError(t, err)
	return m
}

func (c *cluster) memcached(ctx context.Context, t testing.TB, name string) *unstructured.Unstructured {
	t.Helper()
	m, err := c.dynamic.Resource(memcacheds).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	return m
}

func (c *cluster) deployment(ctx context.Context, t testing.TB, name string) *appsv1.Deployment {
	t.Helper()
	d, err := c.kube.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	return d
}

func (c *cluster) awaitAvailable(ctx context.Context, t testing.TB, name string, size int) {
	t.Helper()
	want := fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", name, size)
	c.await(ctx, t, "the Memcached to be Available with "+fmt.Sprint(size)+" replicas", func(ctx context.Context) (bool, error) {
		m, err := c.dynamic.Resource(memcacheds).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		conditions, _, err := unstructured.NestedSlice(m.Object, "status", "conditions")
		if err != nil {
			return false, err
		}
		for _, raw := range conditions {
			cond, _ := raw.(map[string]any)
			if cond["type"] == "Available" {
				return cond["status"] == "True" && cond["reason"] == "Reconciling" && cond["message"] == want, nil
			}
		}
		return false, nil
	})
}

func (c *cluster) awaitReplicas(ctx context.Context, t testing.TB, name string, replicas int32) {
	t.Helper()
	c.await(ctx, t, fmt.Sprintf("the Deployment to have %d replicas", replicas), func(ctx context.Context) (bool, error) {
		d, err := c.kube.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return d.Spec.Replicas != nil && *d.Spec.Replicas == replicas, nil
	})
}

func (c *cluster) await(ctx context.Context, t testing.TB, what string, done func(ctx context.Context) (bool, error)) {
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
			require.Failf(t, "timed out", "waiting for %s in %s: %v", what, c.namespace, last)
		case <-time.After(poll):
		}
	}
}
