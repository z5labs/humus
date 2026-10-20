// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package controller_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/z5labs/humus/example/databases/api/v1alpha1"
	"github.com/z5labs/humus/example/databases/controller"
	"github.com/z5labs/humus/kube"
)

type provision struct {
	name     string
	password string
}

type fakeAdmin struct {
	provisions []provision
	err        error
	databases  map[string]bool
	drops      []string
	dropErr    error
}

func (a *fakeAdmin) Provision(_ context.Context, name, password string) error {
	a.provisions = append(a.provisions, provision{name: name, password: password})
	return a.err
}

func (a *fakeAdmin) Drop(_ context.Context, name string) error {
	a.drops = append(a.drops, name)
	if a.dropErr != nil {
		return a.dropErr
	}
	delete(a.databases, name)
	return nil
}

func newDatabase() kube.Object[v1alpha1.DatabaseSpec, v1alpha1.DatabaseStatus] {
	return kube.Object[v1alpha1.DatabaseSpec, v1alpha1.DatabaseStatus]{
		TypeMeta: metav1.TypeMeta{APIVersion: "postgres.example.com/v1alpha1", Kind: "Database"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "orders",
			Namespace:  "shop",
			UID:        types.UID("8d1c5c1e-5b8e-4a0e-9a57-0b7f3f0c2d11"),
			Generation: 3,
		},
		Spec: v1alpha1.DatabaseSpec{Name: "orders_db", DeletionPolicy: v1alpha1.Delete},
	}
}

func newReconciler(kc *kubefake.Clientset, admin *fakeAdmin) controller.Reconciler {
	return controller.Reconciler{Kube: kc, Admin: admin, Host: "postgres.databases.svc", Port: 5432}
}

func secret(t *testing.T, kc *kubefake.Clientset) *corev1.Secret {
	t.Helper()
	s, err := kc.CoreV1().Secrets("shop").Get(t.Context(), "orders-postgres", metav1.GetOptions{})
	require.NoError(t, err)
	return s
}

func TestReconciler_a_new_Database_gets_its_role_database_and_Secret_and_is_Ready(t *testing.T) {
	kc := kubefake.NewClientset()
	admin := &fakeAdmin{}
	db := newDatabase()

	res, err := newReconciler(kc, admin).Reconcile(t.Context(), db)

	require.NoError(t, err)
	require.Len(t, admin.provisions, 1)
	require.Equal(t, "orders_db", admin.provisions[0].name)
	password := admin.provisions[0].password
	require.NotEmpty(t, password)

	s := secret(t, kc)
	require.Equal(t, map[string][]byte{
		"host":     []byte("postgres.databases.svc"),
		"port":     []byte("5432"),
		"database": []byte("orders_db"),
		"username": []byte("orders_db"),
		"password": []byte(password),
	}, s.Data)
	require.Equal(t, []metav1.OwnerReference{{
		APIVersion:         "postgres.example.com/v1alpha1",
		Kind:               "Database",
		Name:               "orders",
		UID:                db.UID,
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}}, s.OwnerReferences)

	require.NotNil(t, res.Status)
	require.Equal(t, "orders-postgres", res.Status.SecretName)
	ready := meta.FindStatusCondition(res.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	require.Equal(t, "Provisioned", ready.Reason)
	require.Equal(t, int64(3), ready.ObservedGeneration)
	require.NotContains(t, ready.Message, password)
	require.Zero(t, res.After)
}

func TestReconciler_reconciling_again_keeps_the_password_and_changes_nothing(t *testing.T) {
	kc := kubefake.NewClientset()
	admin := &fakeAdmin{}
	r := newReconciler(kc, admin)
	db := newDatabase()
	first, err := r.Reconcile(t.Context(), db)
	require.NoError(t, err)
	before := secret(t, kc)
	db.Status = *first.Status

	again, err := r.Reconcile(t.Context(), db)

	require.NoError(t, err)
	require.Len(t, admin.provisions, 2)
	require.Equal(t, admin.provisions[0], admin.provisions[1])
	require.Equal(t, before, secret(t, kc))
	require.Equal(t, db.Status, *again.Status)
}

func TestReconciler_a_Database_whose_Secret_is_gone_gets_a_new_password(t *testing.T) {
	kc := kubefake.NewClientset()
	admin := &fakeAdmin{}
	r := newReconciler(kc, admin)
	db := newDatabase()
	first, err := r.Reconcile(t.Context(), db)
	require.NoError(t, err)
	db.Status = *first.Status
	old := admin.provisions[0].password
	require.NoError(t, kc.CoreV1().Secrets("shop").Delete(t.Context(), "orders-postgres", metav1.DeleteOptions{}))

	res, err := r.Reconcile(t.Context(), db)

	require.NoError(t, err)
	require.Len(t, admin.provisions, 2)
	rotated := admin.provisions[1].password
	require.NotEmpty(t, rotated)
	require.NotEqual(t, old, rotated)
	require.Equal(t, "orders_db", admin.provisions[1].name)
	require.Equal(t, []byte(rotated), secret(t, kc).Data["password"])
	require.True(t, meta.IsStatusConditionTrue(res.Status.Conditions, "Ready"))
}

func TestReconciler_each_failure_sets_its_reason_and_returns_an_error(t *testing.T) {
	cause := errors.New("boom")
	testCases := []struct {
		name    string
		setup   func(kc *kubefake.Clientset, admin *fakeAdmin)
		reason  string
		message string
	}{
		{
			name:    "PostgreSQL fails",
			setup:   func(_ *kubefake.Clientset, admin *fakeAdmin) { admin.err = cause },
			reason:  "PostgresUnavailable",
			message: "The role and the database could not be provisioned in PostgreSQL.",
		},
		{
			name: "the Secret's apply fails",
			setup: func(kc *kubefake.Clientset, _ *fakeAdmin) {
				kc.PrependReactor("patch", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, cause
				})
			},
			reason:  "SecretApplyFailed",
			message: "The Secret could not be applied.",
		},
		{
			name: "the Secret can't be read",
			setup: func(kc *kubefake.Clientset, _ *fakeAdmin) {
				kc.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, cause
				})
			},
			reason:  "SecretApplyFailed",
			message: "The Secret could not be read.",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			kc := kubefake.NewClientset()
			admin := &fakeAdmin{}
			tc.setup(kc, admin)
			db := newDatabase()

			res, err := newReconciler(kc, admin).Reconcile(t.Context(), db)

			require.ErrorIs(t, err, cause)
			require.NotNil(t, res.Status)
			ready := meta.FindStatusCondition(res.Status.Conditions, "Ready")
			require.NotNil(t, ready)
			require.Equal(t, metav1.ConditionFalse, ready.Status)
			require.Equal(t, tc.reason, ready.Reason)
			require.Equal(t, tc.message, ready.Message)
			require.Equal(t, int64(3), ready.ObservedGeneration)
			for _, p := range admin.provisions {
				require.NotContains(t, ready.Message, p.password)
				require.NotContains(t, err.Error(), p.password)
			}
		})
	}
}

func TestReconciler_a_failure_after_Ready_turns_it_False_and_keeps_the_Secret_name(t *testing.T) {
	kc := kubefake.NewClientset()
	admin := &fakeAdmin{}
	r := newReconciler(kc, admin)
	db := newDatabase()
	first, err := r.Reconcile(t.Context(), db)
	require.NoError(t, err)
	db.Status = *first.Status
	admin.err = errors.New("connection refused")

	res, err := r.Reconcile(t.Context(), db)

	require.Error(t, err)
	require.Equal(t, "orders-postgres", res.Status.SecretName)
	ready := meta.FindStatusCondition(res.Status.Conditions, "Ready")
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, "PostgresUnavailable", ready.Reason)
	require.True(t, meta.IsStatusConditionTrue(db.Status.Conditions, "Ready"))
}
