// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package controller reconciles each Database into its role, database, Secret and Ready condition.
package controller

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus/example/databases/api/v1alpha1"
	"github.com/z5labs/humus/example/databases/postgres"
	"github.com/z5labs/humus/kube"
)

const fieldManager = "databases-controller"

type database = kube.Object[v1alpha1.DatabaseSpec, v1alpha1.DatabaseStatus]

type Reconciler struct {
	Kube  kubernetes.Interface
	Admin postgres.Admin
	Host  string
	Port  int
}

var _ kube.Reconciler[v1alpha1.DatabaseSpec, v1alpha1.DatabaseStatus] = Reconciler{}

func (r Reconciler) Reconcile(ctx context.Context, db database) (kube.Result[v1alpha1.DatabaseStatus], error) {
	st := db.Status
	st.Conditions = slices.Clone(st.Conditions)
	name := db.Name + "-postgres"

	password, uid, err := r.password(ctx, db.Namespace, name)
	if err != nil {
		return r.failed(db, st, "SecretApplyFailed", "The Secret could not be read.", fmt.Errorf("reading Secret %s/%s: %w", db.Namespace, name, err))
	}

	if err := r.Admin.Provision(ctx, db.Spec.Name, password); err != nil {
		return r.failed(db, st, "PostgresUnavailable", "The role and the database could not be provisioned in PostgreSQL.", err)
	}

	err = r.applySecret(ctx, db, name, uid, password)
	if uid != "" && apierrors.IsConflict(err) {
		password = rand.Text()
		if err := r.Admin.Provision(ctx, db.Spec.Name, password); err != nil {
			return r.failed(db, st, "PostgresUnavailable", "The role and the database could not be provisioned in PostgreSQL.", err)
		}
		err = r.applySecret(ctx, db, name, "", password)
	}
	if err != nil {
		return r.failed(db, st, "SecretApplyFailed", "The Secret could not be applied.", fmt.Errorf("applying Secret %s/%s: %w", db.Namespace, name, err))
	}

	st.SecretName = name
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Provisioned",
		Message:            "The role, the database and the Secret are provisioned.",
		ObservedGeneration: db.Generation,
	})
	return kube.Result[v1alpha1.DatabaseStatus]{Status: &st}, nil
}

func (r Reconciler) password(ctx context.Context, namespace, name string) (string, types.UID, error) {
	secret, err := r.Kube.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return rand.Text(), "", nil
	}
	if err != nil {
		return "", "", err
	}
	if password := string(secret.Data["password"]); password != "" {
		return password, secret.UID, nil
	}
	return rand.Text(), secret.UID, nil
}

func (r Reconciler) applySecret(ctx context.Context, db database, name string, uid types.UID, password string) error {
	secret := corev1ac.Secret(name, db.Namespace).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(v1alpha1.Database.Group + "/" + v1alpha1.Database.Version).
			WithKind(v1alpha1.Database.Kind).
			WithName(db.Name).
			WithUID(db.UID).
			WithController(true).
			WithBlockOwnerDeletion(true)).
		WithData(map[string][]byte{
			"host":     []byte(r.Host),
			"port":     []byte(strconv.Itoa(r.Port)),
			"database": []byte(db.Spec.Name),
			"username": []byte(db.Spec.Name),
			"password": []byte(password),
		})
	if uid != "" {
		secret.WithUID(uid)
	}
	_, err := r.Kube.CoreV1().Secrets(db.Namespace).Apply(ctx, secret, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
	return err
}

func (r Reconciler) failed(db database, st v1alpha1.DatabaseStatus, reason, message string, err error) (kube.Result[v1alpha1.DatabaseStatus], error) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: db.Generation,
	})
	return kube.Result[v1alpha1.DatabaseStatus]{Status: &st}, err
}
