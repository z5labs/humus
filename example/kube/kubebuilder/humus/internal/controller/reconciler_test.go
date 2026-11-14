// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package controller

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/z5labs/humus/example/kube/kubebuilder/humus/api/v1alpha1"
	"github.com/z5labs/humus/kube"
)

func cache() kube.Object[v1alpha1.MemcachedSpec, v1alpha1.MemcachedStatus] {
	return kube.Object[v1alpha1.MemcachedSpec, v1alpha1.MemcachedStatus]{
		TypeMeta:   metav1.TypeMeta{APIVersion: "cache.example.com/v1alpha1", Kind: "Memcached"},
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "default", UID: "cache-uid", ResourceVersion: "7", Generation: 1},
		Spec:       v1alpha1.MemcachedSpec{Size: 2},
	}
}

func TestReconciler_applies_a_Deployment_the_Memcached_controls(t *testing.T) {
	kc := kubefake.NewClientset()
	m := cache()

	res, err := Reconciler{Kube: kc}.Reconcile(t.Context(), m)
	require.NoError(t, err)

	d, err := kc.AppsV1().Deployments("default").Get(t.Context(), "cache", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, int32(2), *d.Spec.Replicas)
	require.Equal(t, map[string]string{"app.kubernetes.io/name": "memcached", "app.kubernetes.io/instance": "cache"}, d.Spec.Selector.MatchLabels)
	require.Equal(t, d.Spec.Selector.MatchLabels, d.Spec.Template.Labels)
	c := d.Spec.Template.Spec.Containers[0]
	require.Equal(t, "memcached:1.6.26-alpine3.19", c.Image)
	require.Equal(t, []string{"memcached", "--memory-limit=64", "-o", "modern", "-v"}, c.Command)
	require.Equal(t, int32(11211), c.Ports[0].ContainerPort)
	owner := metav1.GetControllerOf(d)
	require.NotNil(t, owner)
	require.Equal(t, m.UID, owner.UID, "the Memcached controls the Deployment")
	require.True(t, *owner.BlockOwnerDeletion)

	require.NotNil(t, res.Status)
	cond := meta.FindStatusCondition(res.Status.Conditions, "Available")
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "Reconciling", cond.Reason)
	require.Equal(t, "Deployment for custom resource (cache) with 2 replicas created successfully", cond.Message)
	require.Zero(t, res.After)
}

func TestReconciler_applies_a_new_size(t *testing.T) {
	kc := kubefake.NewClientset()
	m := cache()
	_, err := Reconciler{Kube: kc}.Reconcile(t.Context(), m)
	require.NoError(t, err)

	m.Spec.Size = 3
	res, err := Reconciler{Kube: kc}.Reconcile(t.Context(), m)
	require.NoError(t, err)

	d, err := kc.AppsV1().Deployments("default").Get(t.Context(), "cache", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, int32(3), *d.Spec.Replicas)
	cond := meta.FindStatusCondition(res.Status.Conditions, "Available")
	require.NotNil(t, cond)
	require.Equal(t, "Deployment for custom resource (cache) with 3 replicas created successfully", cond.Message)
}

func TestReconciler_reports_a_failed_apply_as_a_condition_and_an_error(t *testing.T) {
	kc := kubefake.NewClientset()
	kc.PrependReactor("patch", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("admission webhook denied the request")
	})

	res, err := Reconciler{Kube: kc}.Reconcile(t.Context(), cache())
	require.ErrorContains(t, err, "admission webhook denied the request")
	require.NotNil(t, res.Status, "the status is applied with the error")
	cond := meta.FindStatusCondition(res.Status.Conditions, "Available")
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, "Reconciling", cond.Reason)
	require.Contains(t, cond.Message, "admission webhook denied the request")
}

func TestReconciler_Finalize_records_a_Deleting_event(t *testing.T) {
	kc := kubefake.NewClientset()
	m := cache()
	m.DeletionTimestamp = &metav1.Time{}

	err := Reconciler{Kube: kc, Instance: "controller-manager-0"}.Finalize(t.Context(), m)
	require.NoError(t, err)

	list, err := kc.EventsV1().Events("default").List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	e := list.Items[0]
	require.Regexp(t, `^cache\.[0-9a-f]+$`, e.Name, "named as client-go's recorders name an Event")
	require.Equal(t, corev1.EventTypeWarning, e.Type)
	require.Equal(t, "Deleting", e.Reason)
	require.Equal(t, "DeleteCR", e.Action)
	require.Equal(t, "memcached-controller", e.ReportingController)
	require.Equal(t, "controller-manager-0", e.ReportingInstance)
	require.False(t, e.EventTime.IsZero())
	require.Equal(t, corev1.ObjectReference{
		APIVersion: "cache.example.com/v1alpha1", Kind: "Memcached", Name: "cache", Namespace: "default",
		UID: "cache-uid", ResourceVersion: "7",
	}, e.Regarding)
	require.Equal(t, "Custom Resource cache is being deleted from the namespace default", e.Note)
}

func TestReconciler_Finalize_returns_an_error_that_retries_it(t *testing.T) {
	kc := kubefake.NewClientset()
	kc.PrependReactor("create", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server is unavailable")
	})

	err := Reconciler{Kube: kc, Instance: "controller-manager-0"}.Finalize(t.Context(), cache())
	require.ErrorContains(t, err, "the API server is unavailable")
}
