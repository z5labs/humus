// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
)

// Owns reconciles an object again when a resource of gvr that it controls
// changes: one whose controller owner reference names the object.
//
// When a resource of gvr is added, changed or deleted, and its owner
// reference with controller: true names the Resource's group and kind, the
// object that reference names is reconciled. The reference's version isn't
// compared. A reference without controller: true, or one that names another
// group or kind, reconciles nothing, and neither does a resource with no
// owner reference. The end user sets
// the controller owner reference when they apply the resource, with
// client-go's apply configurations.
//
// The Controller watches gvr's resources through the metadata client, since
// it looks only at their owner references, limited to Settings.Namespace.
// It needs list and watch on them.
func Owns(gvr schema.GroupVersionResource) Part {
	return owns{gvr: gvr}
}

// owns is the Part Owns returns.
type owns struct {
	gvr schema.GroupVersionResource
}

func (owns) part() {}

// ownedResource returns the metadata client of gvr's resources in
// Settings.Namespace, or in every namespace when it is "".
func (c *Controller) ownedResource(gvr schema.GroupVersionResource) metadata.ResourceInterface {
	r := c.meta.Resource(gvr)
	if ns := c.settings.Namespace; ns != "" {
		return r.Namespace(ns)
	}
	return r
}

// ownerKey returns the key of the object that obj, a resource of an Owns,
// is controlled by: the one its owner reference with controller: true
// names, when that reference names the Resource's group and kind. Its
// version isn't compared: a reference written with another version of the
// Resource names the same object. ok is false when obj has no such
// reference.
func (c *Controller) ownerKey(obj any) (key string, ok bool) {
	if d, isTombstone := obj.(cache.DeletedFinalStateUnknown); isTombstone {
		obj = d.Obj
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return "", false
	}
	ref := metav1.GetControllerOfNoCopy(m)
	if ref == nil || ref.Kind != c.kind {
		return "", false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil || gv.Group != c.group {
		return "", false
	}
	if !c.namespaced {
		return ref.Name, true
	}
	// A namespaced owner is in its child's namespace.
	ns := m.GetNamespace()
	if ns == "" {
		return "", false
	}
	return ns + "/" + ref.Name, true
}
