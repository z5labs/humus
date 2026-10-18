// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Finalize keeps every object until f has cleaned up after it. The
// controller adds the finalizer name to each object it reconciles. Once an
// object is being deleted, it calls f in place of the Reconciler until f
// returns nil, and then removes name. T is the Controller's object type,
// Object[Spec, Status] for a Resource, which NewController checks.
//
// T is inferred from f:
//
//	kube.Finalize("cache.example.com/finalizer", func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) error {
//		return deleteBackups(ctx, m.Namespace, m.Name)
//	})
//
// The controller adds name to the object's metadata.finalizers, and later
// removes it, with server-side apply under the field manager Settings.Name,
// so it never conflicts with other writers of the object's metadata. An
// error from f retries the object with backoff, as a reconcile's does, so f
// may be called again for an object it has already cleaned up after.
func Finalize[T any](name string, f func(ctx context.Context, obj T) error) Part {
	return finalize{name: name, f: f, nilF: f == nil, typ: reflect.TypeFor[T]()}
}

// finalize is the Part Finalize returns. f is its func(context.Context, T)
// error, which NewController asserts is of the Controller's object type,
// and typ is T, which names it when it isn't.
type finalize struct {
	name string
	f    any
	nilF bool
	typ  reflect.Type
}

func (finalize) part() {}

// finalizeFunc calls a Finalize's f with an object from the cache.
type finalizeFunc func(ctx context.Context, u *unstructured.Unstructured) error

// finalizerOf returns the finalizeFunc that hands f an Object decoded from
// the cache's object, its own copy, as reconcilerOf does.
func finalizerOf[Spec, Status any](f func(context.Context, Object[Spec, Status]) error) finalizeFunc {
	return func(ctx context.Context, u *unstructured.Unstructured) error {
		obj, err := decodeObject[Spec, Status](u)
		if err != nil {
			return err
		}
		return f(ctx, obj)
	}
}

// hasFinalizer reports whether u's metadata.finalizers holds name.
func hasFinalizer(u *unstructured.Unstructured, name string) bool {
	return slices.Contains(u.GetFinalizers(), name)
}

// applyFinalizer applies u's metadata with the controller's finalizer, when
// add is true, or without it, with server-side apply under the field manager
// Settings.Name. The finalizer is that field manager's only field of the
// object's own, so applying the metadata without it removes it, and the API
// server completes a deletion it was holding.
//
// The metadata names u's uid. An apply creates an object that doesn't
// exist, but not one whose uid it names, so an object deleted since the
// cache saw it fails with a conflict, and isn't created again.
func (c *Controller) applyFinalizer(ctx context.Context, u *unstructured.Unstructured, add bool) error {
	metadata := map[string]any{"name": u.GetName(), "uid": string(u.GetUID())}
	if ns := u.GetNamespace(); ns != "" {
		metadata["namespace"] = ns
	}
	if add {
		metadata["finalizers"] = []any{c.finalizer}
	}
	patch := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": c.apiVersion,
		"kind":       c.kind,
		"metadata":   metadata,
	}}
	verb := "removing"
	if add {
		verb = "adding"
	}
	got, err := c.resource(u.GetNamespace()).Apply(ctx, u.GetName(), patch, metav1.ApplyOptions{
		FieldManager: c.settings.Name,
		Force:        true,
	})
	if err != nil {
		return fmt.Errorf("kube: %s the finalizer %s of %s: %w", verb, c.finalizer, objectName(u), err)
	}
	// Another field manager that also set the finalizer keeps it on the
	// object, and f would be called for it again and again.
	if !add && got != nil && hasFinalizer(got, c.finalizer) {
		return fmt.Errorf("kube: removing the finalizer %s of %s: another field manager also set it, so it is still on the object",
			c.finalizer, objectName(u))
	}
	return nil
}
