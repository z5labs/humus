// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package controller is the Memcached controller's Reconciler and its finalizer, on humus's kube module.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus/example/kube/kubebuilder/humus/api/v1alpha1"
	"github.com/z5labs/humus/kube"
)

const (
	controllerName         = "memcached-controller"
	memcachedContainerName = "memcached"
	typeAvailableMemcached = "Available"
	reasonReconciling      = "Reconciling"
)

type Reconciler struct {
	Kube     kubernetes.Interface
	Instance string
}

func (r Reconciler) Reconcile(ctx context.Context, m kube.Object[v1alpha1.MemcachedSpec, v1alpha1.MemcachedStatus]) (kube.Result[v1alpha1.MemcachedStatus], error) {
	st := m.Status
	_, err := r.Kube.AppsV1().Deployments(m.Namespace).Apply(ctx, deploymentForMemcached(m), metav1.ApplyOptions{FieldManager: controllerName, Force: true})
	if err != nil {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typeAvailableMemcached,
			Status: metav1.ConditionFalse, Reason: reasonReconciling,
			Message: fmt.Sprintf("Failed to apply the Deployment for the custom resource (%s): (%s)", m.Name, err)})
		return kube.Result[v1alpha1.MemcachedStatus]{Status: &st}, err
	}

	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typeAvailableMemcached,
		Status: metav1.ConditionTrue, Reason: reasonReconciling,
		Message: fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", m.Name, m.Spec.Size)})
	return kube.Result[v1alpha1.MemcachedStatus]{Status: &st}, nil
}

func (r Reconciler) Finalize(ctx context.Context, m kube.Object[v1alpha1.MemcachedSpec, v1alpha1.MemcachedStatus]) error {
	now := time.Now()
	_, err := r.Kube.EventsV1().Events(m.Namespace).Create(ctx, &eventsv1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: fmt.Sprintf("%s.%x", m.Name, now.UnixNano())},
		EventTime:           metav1.NewMicroTime(now),
		ReportingController: controllerName,
		ReportingInstance:   r.Instance,
		Action:              "DeleteCR",
		Reason:              "Deleting",
		Type:                corev1.EventTypeWarning,
		Regarding: corev1.ObjectReference{
			APIVersion: m.APIVersion, Kind: m.Kind, Name: m.Name, Namespace: m.Namespace,
			UID: m.UID, ResourceVersion: m.ResourceVersion,
		},
		Note: fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s", m.Name, m.Namespace),
	}, metav1.CreateOptions{})
	return err
}

func deploymentForMemcached(m kube.Object[v1alpha1.MemcachedSpec, v1alpha1.MemcachedStatus]) *appsv1ac.DeploymentApplyConfiguration {
	ls := labelsForMemcached(m.Name)
	return appsv1ac.Deployment(m.Name, m.Namespace).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(m.APIVersion).WithKind(m.Kind).WithName(m.Name).WithUID(m.UID).
			WithController(true).WithBlockOwnerDeletion(true)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(m.Spec.Size).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(ls)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(ls).
				WithSpec(corev1ac.PodSpec().WithContainers(corev1ac.Container().
					WithImage("memcached:1.6.26-alpine3.19").
					WithName(memcachedContainerName).
					WithPorts(corev1ac.ContainerPort().WithContainerPort(11211).WithName(memcachedContainerName)).
					WithCommand("memcached", "--memory-limit=64", "-o", "modern", "-v")))))
}

func labelsForMemcached(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "memcached",
		"app.kubernetes.io/instance": name,
	}
}
