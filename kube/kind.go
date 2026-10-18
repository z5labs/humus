// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/z5labs/humus"
)

// Kind names a kind whose Go type is defined elsewhere, such as one of
// Kubernetes' own, corev1.Namespace. Its objects decode into T. Its status
// belongs to whoever defines the kind, so a controller of a Kind applies no
// status.
//
// A Kind has no CRD and no Rules: the kind's own definition is the API
// server's.
type Kind[T any] struct {
	GVR   schema.GroupVersionResource // such as corev1.SchemeGroupVersion.WithResource("namespaces")
	Kind  string                      // such as Namespace
	Scope Scope
}

// name names k in an error: its resource and group, as kubectl names them,
// or its Go type when its resource is missing.
func (k Kind[T]) name() string {
	if k.GVR.Resource == "" {
		return reflect.TypeOf(k).String()
	}
	return k.GVR.GroupResource().String()
}

// mistakes returns each mistake in k, each on its own.
func (k Kind[T]) mistakes() []error {
	var errs []error
	if k.GVR.Version == "" {
		errs = append(errs, fmt.Errorf("Kind.GVR, %s, has no Version", k.GVR))
	}
	if k.GVR.Resource == "" {
		errs = append(errs, fmt.Errorf("Kind.GVR, %s, has no Resource", k.GVR))
	}
	if k.Kind == "" {
		errs = append(errs, errors.New("Kind.Kind must not be empty"))
	}
	if k.Scope != Namespaced && k.Scope != Cluster {
		errs = append(errs, fmt.Errorf("Kind.Scope is %q, not %q or %q", k.Scope, Namespaced, Cluster))
	}
	if _, ok := any(new(T)).(metav1.Object); !ok {
		errs = append(errs, fmt.Errorf("the Kind's objects are a %s, but a %s isn't a metav1.Object",
			reflect.TypeFor[T](), reflect.TypeFor[*T]()))
	}
	return errs
}

// KindReconciler brings the cluster to what an object of a Kind declares.
type KindReconciler[T any] interface {
	// Reconcile is handed obj, its own copy of the object as the cache last
	// saw it. A positive after reconciles obj again after that long, unless
	// it changes first. Zero waits for a change, or for the resync. A
	// Reconcile that returns an error is retried with backoff instead.
	Reconcile(ctx context.Context, obj T) (after time.Duration, err error)
}

// KindReconcilerFunc is a function that is a KindReconciler.
type KindReconcilerFunc[T any] func(ctx context.Context, obj T) (time.Duration, error)

// Reconcile calls f(ctx, obj).
func (f KindReconcilerFunc[T]) Reconcile(ctx context.Context, obj T) (time.Duration, error) {
	return f(ctx, obj)
}

// NewKindController returns a Controller of k's objects, which rec
// reconciles. It opens no connection: Serve does, through c. *T must be a
// metav1.Object, as every client-go type's is.
//
// A Kind's objects are reconciled as a Resource's are, except that each is
// decoded into a new T, no status is ever applied, and the key settles with
// the after rec returns. An Owns matches the owner references that name
// k.GVR.Group and k.Kind.
//
// It returns one error that names every mistake it finds:
//   - each mistake in settings, and in parts, as NewController reports
//     them, with a Finalize's f checked to take a T;
//   - a GVR without a Version or a Resource. Its Group is "" for the core
//     group;
//   - a missing Kind, or a Scope that is neither Namespaced nor Cluster;
//   - a T whose pointer isn't a metav1.Object;
//   - a Settings.Namespace that isn't "" for a Kind whose Scope is Cluster,
//     whose objects are in no namespace;
//   - a nil KindReconciler, a nil Setup or a nil Client;
//   - an error creating one of its instruments.
//
// The Controller reports through s.Telemetry, as NewController's does.
func NewKindController[T any](s *humus.Setup, c *Client, settings Settings, k Kind[T], rec KindReconciler[T], parts ...Part) (*Controller, error) {
	var recErr error
	if rec == nil {
		recErr = errors.New("the Reconciler must not be nil")
	} else if f, ok := rec.(KindReconcilerFunc[T]); ok && f == nil {
		recErr = errors.New("the Reconciler must not be a nil KindReconcilerFunc")
	}
	var errs []error
	if kindErrs := k.mistakes(); len(kindErrs) > 0 {
		errs = append(errs, fmt.Errorf("the Kind %s: %w", k.name(), errors.Join(kindErrs...)))
	}
	return newController[T](s, c, settings, target{
		ctor:      "NewKindController",
		name:      k.name(),
		gvr:       k.GVR,
		kind:      k.Kind,
		scope:     k.Scope,
		mistakes:  errs,
		recErr:    recErr,
		reconcile: kindReconcilerOf(rec),
	}, parts)
}

// kindReconcilerOf returns the reconcileFunc that hands rec a T decoded from
// the cache's object, its own copy, as reconcilerOf does. It never returns
// a status: a Kind's belongs to whoever defines it.
func kindReconcilerOf[T any](rec KindReconciler[T]) reconcileFunc {
	return func(ctx context.Context, u *unstructured.Unstructured) (any, time.Duration, error) {
		obj, err := decode[T](u)
		if err != nil {
			return nil, 0, err
		}
		after, err := rec.Reconcile(ctx, obj)
		return nil, after, err
	}
}
