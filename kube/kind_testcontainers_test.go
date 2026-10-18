//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

// The test in this file runs a whole App, on humus.OSHost() and humustest's
// process, whose Controller reconciles Namespaces, one of Kubernetes' own
// kinds, in the k3s that kube_testcontainers_test.go starts.

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiwait "k8s.io/apimachinery/pkg/util/wait"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// The name of the Namespace controller, its Lease's and its field
// manager's, and of the ConfigMap it applies in each Namespace.
const (
	namespaceControllerName = "namespace-defaults"
	defaultsName            = "humus-defaults"
)

// defaults is the end user's KindReconciler of Namespaces: it applies a
// ConfigMap in each Namespace, which the Namespace controls. A ConfigMap
// needs no pod, which k3s without its agent can't run.
type defaults struct {
	kube kubernetes.Interface
}

func (r defaults) Reconcile(ctx context.Context, ns corev1.Namespace) (time.Duration, error) {
	if ns.DeletionTimestamp != nil {
		return 0, nil
	}
	_, err := r.kube.CoreV1().ConfigMaps(ns.Name).Apply(ctx,
		corev1ac.ConfigMap(defaultsName, ns.Name).
			WithOwnerReferences(metav1ac.OwnerReference().
				WithAPIVersion("v1").WithKind("Namespace").WithName(ns.Name).WithUID(ns.UID).
				WithController(true)).
			WithData(map[string]string{"namespace": ns.Name}),
		metav1.ApplyOptions{FieldManager: namespaceControllerName, Force: true},
	)
	return 0, err
}

func TestKindController_gives_each_Namespace_a_ConfigMap_in_a_whole_App(t *testing.T) {
	ctx := t.Context()
	client := k3sClient(t)
	kc, err := kubernetes.NewForConfig(client.Config())
	require.NoError(t, err)

	ns, err := kc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kind-defaults"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = kc.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
	})

	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		client, err := kube.NewClient(s, cluster.conn)
		if err != nil {
			return nil, err
		}
		kc, err := kubernetes.NewForConfig(client.Config())
		if err != nil {
			return nil, err
		}
		settings := controllerSettings()
		settings.Name = namespaceControllerName
		ctrl, err := kube.NewKindController(s, client, settings, Namespaces, defaults{kube: kc},
			kube.Owns(corev1.SchemeGroupVersion.WithResource("configmaps")),
		)
		if err != nil {
			return nil, err
		}
		return []humus.Service{ctrl}, nil
	}, humustest.WithApp(humus.WithHost(humus.OSHost())))

	configMaps := kc.CoreV1().ConfigMaps(ns.Name)
	applied := func(ctx context.Context) (bool, error) {
		_, err := configMaps.Get(ctx, defaultsName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, appWaitLimit, true, applied)
	require.NoError(t, err, "the ConfigMap was never applied")

	cm, err := configMaps.Get(ctx, defaultsName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"namespace": ns.Name}, cm.Data)
	owner := metav1.GetControllerOf(cm)
	require.NotNil(t, owner)
	require.Equal(t, "Namespace", owner.Kind)
	require.Equal(t, ns.UID, owner.UID, "the Namespace controls the ConfigMap")
	require.True(t, hasManager(cm.ManagedFields, namespaceControllerName, metav1.ManagedFieldsOperationApply, ""),
		"the ConfigMap is applied under the Reconciler's field manager: %+v", cm.ManagedFields)

	got, err := kc.CoreV1().Namespaces().Get(ctx, ns.Name, metav1.GetOptions{})
	require.NoError(t, err)
	for _, f := range got.ManagedFields {
		require.NotEqual(t, namespaceControllerName, f.Manager, "nothing is applied to the Namespace, its status least of all")
	}

	// A deletion by hand. Nothing about the Namespace changes, and its
	// resync is 10h away, so only Owns brings the controller back to it.
	require.NoError(t, configMaps.Delete(ctx, defaultsName, metav1.DeleteOptions{}))
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, applied)
	require.NoError(t, err, "the deleted ConfigMap was never put back")

	p.Signal(syscall.SIGTERM)
	st := appExit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}
