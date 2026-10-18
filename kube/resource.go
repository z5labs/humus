// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Scope is whether a resource's objects are in a namespace.
type Scope string

const (
	// Namespaced is the scope of a resource whose objects are each in a
	// namespace, as a Deployment is.
	Namespaced Scope = "Namespaced"

	// Cluster is the scope of a resource whose objects are in no
	// namespace, as a Namespace is.
	Cluster Scope = "Cluster"
)

// Resource declares a custom resource of one version, whose objects have a
// spec of type Spec and a status of type Status.
//
// Spec and Status are plain Go types, encoded as encoding/json encodes them.
// [Resource.CRD] reflects the resource's CustomResourceDefinition from them,
// so they need no markers and no generated code.
type Resource[Spec, Status any] struct {
	Group   string // such as cache.example.com
	Version string // such as v1alpha1
	Kind    string // such as Memcached
	Plural  string // such as memcacheds
	Scope   Scope

	// Rules constrain the fields of Spec, of Status, and of the types they
	// hold. See [Rules].
	Rules []TypeRules
}

// GVR is the resource's group, version and plural name.
func (r Resource[Spec, Status]) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: r.Group, Version: r.Version, Resource: r.Plural}
}

// CRD returns the resource's CustomResourceDefinition: one version, served
// and stored, with the status subresource, and a schema reflected from Spec
// and Status and constrained by Rules. It returns one error that names
// every mistake it finds.
//
// The CRD is named <Plural>.<Group>. Its singular name is Kind in lower
// case, and its list kind is Kind followed by List. It carries its own
// apiVersion and kind, so its YAML, as sigs.k8s.io/yaml writes it, applies
// with kubectl or Helm as it stands. The package doc lists how each Go type
// is reflected, and which fields are required.
//
// The mistakes it reports are:
//   - a missing Group, Version, Kind or Plural, or a Scope that is neither
//     Namespaced nor Cluster;
//   - a type that implements json.Marshaler or encoding.TextMarshaler and
//     has no [Schema], since its fields aren't its encoding;
//   - a map whose keys aren't strings, an interface, a channel, a function,
//     a complex number or an unsafe pointer, none of which has a schema;
//   - a type that contains itself, which a schema without $ref can't
//     express;
//   - a constraint that doesn't fit its field's type, such as Pattern on a
//     number;
//   - a [Field] that names no field of the value given to [Rules], or names
//     a nested struct's field, which only that struct's own Rules
//     constrain;
//   - two Rules, or two Schemas, for one type.
//
// CRD doesn't compile CEL. The API server compiles every rule when the CRD
// is applied.
func (r Resource[Spec, Status]) CRD() (*apiextensionsv1.CustomResourceDefinition, error) {
	var errs []error
	missing := func(field, v string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s must not be empty", "Resource."+field))
		}
	}
	missing("Group", r.Group)
	missing("Version", r.Version)
	missing("Kind", r.Kind)
	missing("Plural", r.Plural)
	if r.Scope != Namespaced && r.Scope != Cluster {
		errs = append(errs, fmt.Errorf("%s is %q, not %q or %q", "Resource.Scope", r.Scope, Namespaced, Cluster))
	}

	rules, ruleErrs := compileRules(r.Rules)
	errs = append(errs, ruleErrs...)
	ref := newReflector(rules)
	root := ref.root(reflect.TypeFor[Spec](), reflect.TypeFor[Status]())
	errs = append(errs, ref.errs...)
	if len(errs) > 0 {
		return nil, fmt.Errorf("kube: the CRD of %s: %w", r.name(), errors.Join(errs...))
	}

	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiextensionsv1.SchemeGroupVersion.String(),
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{Name: r.Plural + "." + r.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: r.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   r.Plural,
				Singular: strings.ToLower(r.Kind),
				Kind:     r.Kind,
				ListKind: r.Kind + "List",
			},
			Scope: apiextensionsv1.ResourceScope(r.Scope),
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    r.Version,
				Served:  true,
				Storage: true,
				Schema:  &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: root},
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
			}},
		},
	}, nil
}

// name names r in an error: its plural and group, as its CRD is named, or
// its Go type when either is missing.
func (r Resource[Spec, Status]) name() string {
	if r.Plural == "" || r.Group == "" {
		return reflect.TypeOf(r).String()
	}
	return r.Plural + "." + r.Group
}

// Object is one object of a Resource.
type Object[Spec, Status any] struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Spec   `json:"spec"`
	Status Status `json:"status,omitempty"`
}
