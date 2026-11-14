// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type MemcachedSpec struct {
	// size defines the number of Memcached instances
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	// +optional
	Size *int32 `json:"size,omitempty"`
}

type MemcachedStatus struct {
	// conditions represent the current state of the Memcached resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Memcached is the Schema for the memcacheds API
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type Memcached struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Memcached
	// +required
	Spec MemcachedSpec `json:"spec"`

	// status defines the observed state of Memcached
	// +optional
	Status MemcachedStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type MemcachedList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Memcached `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Memcached{}, &MemcachedList{})
		return nil
	})
}
