// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package v1alpha1 is the Memcached custom resource, declared as Go values.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/z5labs/humus/kube"
)

type MemcachedSpec struct {
	Size int32 `json:"size,omitempty"`
}

type MemcachedStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

var (
	spec   MemcachedSpec
	status MemcachedStatus

	Memcached = kube.Resource[MemcachedSpec, MemcachedStatus]{
		Group:   "cache.example.com",
		Version: "v1alpha1",
		Kind:    "Memcached",
		Plural:  "memcacheds",
		Scope:   kube.Namespaced,
		Rules: []kube.TypeRules{
			kube.Rules(&spec,
				kube.Field(&spec.Size).Minimum(1).Maximum(3).Description("size defines the number of Memcached instances"),
			).Description("spec defines the desired state of Memcached"),
			kube.Rules(&status,
				kube.Field(&status.Conditions).ListMap("type").Description("conditions represent the current state of the Memcached resource."),
			).Description("status defines the observed state of Memcached"),
		},
	}
)
