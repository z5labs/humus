// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package v1alpha1 is the Database resource of postgres.example.com/v1alpha1.
package v1alpha1

import (
	"regexp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/z5labs/humus/kube"
)

type DeletionPolicy string

const (
	Retain DeletionPolicy = "Retain"
	Delete DeletionPolicy = "Delete"
)

type DatabaseSpec struct {
	Name           string         `json:"name"`
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

type DatabaseStatus struct {
	SecretName string             `json:"secretName,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

var (
	spec   DatabaseSpec
	status DatabaseStatus

	Database = kube.Resource[DatabaseSpec, DatabaseStatus]{
		Group:   "postgres.example.com",
		Version: "v1alpha1",
		Kind:    "Database",
		Plural:  "databases",
		Scope:   kube.Namespaced,
		Rules: []kube.TypeRules{
			kube.Rules(&spec,
				kube.Field(&spec.Name).
					Description("The name of the database in the server, and of the role that owns it.").
					Pattern(regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)).
					Validation("self == oldSelf", "name is immutable"),
				kube.Field(&spec.DeletionPolicy).
					Description("What deleting the Database does: Retain keeps the database and its role, and Delete drops both.").
					Enum(Retain, Delete).
					Default(Retain),
			),
			kube.Rules(&status,
				kube.Field(&status.SecretName).Description("The Secret holding the database's credentials."),
				kube.Field(&status.Conditions).ListMap("type"),
			),
		},
	}
)
