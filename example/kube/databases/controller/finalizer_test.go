// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package controller_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/z5labs/humus/example/kube/databases/api/v1alpha1"
)

func TestFinalize_Delete_drops_the_database_and_its_role(t *testing.T) {
	admin := &fakeAdmin{databases: map[string]bool{"orders_db": true}}
	db := newDatabase()
	db.Spec.DeletionPolicy = v1alpha1.Delete

	err := newReconciler(kubefake.NewClientset(), admin).Finalize(t.Context(), db)

	require.NoError(t, err)
	require.Equal(t, []string{"orders_db"}, admin.drops)
	require.NotContains(t, admin.databases, "orders_db")
}

func TestFinalize_Retain_drops_nothing(t *testing.T) {
	admin := &fakeAdmin{databases: map[string]bool{"orders_db": true}}
	db := newDatabase()
	db.Spec.DeletionPolicy = v1alpha1.Retain

	err := newReconciler(kubefake.NewClientset(), admin).Finalize(t.Context(), db)

	require.NoError(t, err)
	require.Empty(t, admin.drops)
	require.Contains(t, admin.databases, "orders_db")
}

func TestFinalize_no_deletionPolicy_drops_nothing(t *testing.T) {
	admin := &fakeAdmin{databases: map[string]bool{"orders_db": true}}
	db := newDatabase()
	db.Spec.DeletionPolicy = ""

	err := newReconciler(kubefake.NewClientset(), admin).Finalize(t.Context(), db)

	require.NoError(t, err)
	require.Empty(t, admin.drops)
	require.Contains(t, admin.databases, "orders_db")
}

func TestFinalize_a_failed_drop_returns_its_error(t *testing.T) {
	dropErr := errors.New("dropping orders_db: connection refused")
	admin := &fakeAdmin{databases: map[string]bool{"orders_db": true}, dropErr: dropErr}
	db := newDatabase()
	db.Spec.DeletionPolicy = v1alpha1.Delete

	err := newReconciler(kubefake.NewClientset(), admin).Finalize(t.Context(), db)

	require.ErrorIs(t, err, dropErr)
	require.Equal(t, []string{"orders_db"}, admin.drops)
	require.Contains(t, admin.databases, "orders_db")
}

func TestFinalize_Delete_of_a_database_already_gone_succeeds(t *testing.T) {
	admin := &fakeAdmin{databases: map[string]bool{}}
	db := newDatabase()
	db.Spec.DeletionPolicy = v1alpha1.Delete

	err := newReconciler(kubefake.NewClientset(), admin).Finalize(t.Context(), db)

	require.NoError(t, err)
	require.Equal(t, []string{"orders_db"}, admin.drops)
}
