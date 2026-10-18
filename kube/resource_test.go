// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/z5labs/humus/kube"
)

// update rewrites the golden files from what the tests reflect:
//
//	go -C kube test -run CRD -update .
var update = flag.Bool("update", false, "rewrite the golden files in testdata")

type MemcachedSpec struct {
	Size  int32  `json:"size"`
	Image string `json:"image"`
}

type MemcachedStatus struct {
	ReadyReplicas int32              `json:"readyReplicas,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
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
				kube.Field(&spec.Size).Minimum(1).Maximum(3).Description("How many memcached pods run."),
				kube.Field(&spec.Image).Pattern(regexp.MustCompile(`^memcached:`)).Default("memcached:1.6.26-alpine3.19"),
			),
			kube.Rules(&status,
				kube.Field(&status.Conditions).ListMap("type"),
			),
		},
	}
)

// resourceOf returns a valid Resource of Spec and Status.
func resourceOf[Spec, Status any]() kube.Resource[Spec, Status] {
	return kube.Resource[Spec, Status]{
		Group:   "test.humus.dev",
		Version: "v1",
		Kind:    "Widget",
		Plural:  "widgets",
		Scope:   kube.Cluster,
	}
}

// specSchema returns the schema CRD reflects for Spec.
func specSchema[Spec any](t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	crd, err := resourceOf[Spec, struct{}]().CRD()
	require.NoError(t, err)
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
}

// fieldSchema returns the schema CRD reflects for a field of type T, named
// f, with no omitempty.
func fieldSchema[T any](t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	s := specSchema[struct {
		F T `json:"f"`
	}](t)
	require.Equal(t, []string{"f"}, s.Required)
	return s.Properties["f"]
}

// crdError returns CRD's error for Spec.
func crdError[Spec any](t *testing.T) error {
	t.Helper()
	crd, err := resourceOf[Spec, struct{}]().CRD()
	require.Error(t, err)
	require.Nil(t, crd)
	return err
}

func TestResource_GVR(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		schema.GroupVersionResource{Group: "cache.example.com", Version: "v1alpha1", Resource: "memcacheds"},
		Memcached.GVR(),
	)
}

func TestResource_CRD(t *testing.T) {
	t.Parallel()

	crd, err := Memcached.CRD()
	require.NoError(t, err)

	require.Equal(t, "apiextensions.k8s.io/v1", crd.APIVersion)
	require.Equal(t, "CustomResourceDefinition", crd.Kind)
	require.Equal(t, "memcacheds.cache.example.com", crd.Name)
	require.Equal(t, "cache.example.com", crd.Spec.Group)
	require.Equal(t, apiextensionsv1.CustomResourceDefinitionNames{
		Plural:   "memcacheds",
		Singular: "memcached",
		Kind:     "Memcached",
		ListKind: "MemcachedList",
	}, crd.Spec.Names)
	require.Equal(t, apiextensionsv1.NamespaceScoped, crd.Spec.Scope)

	require.Len(t, crd.Spec.Versions, 1)
	v := crd.Spec.Versions[0]
	require.Equal(t, "v1alpha1", v.Name)
	require.True(t, v.Served)
	require.True(t, v.Storage)
	require.Equal(t, &apiextensionsv1.CustomResourceSubresources{
		Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
	}, v.Subresources)

	root := v.Schema.OpenAPIV3Schema
	require.Equal(t, "object", root.Type)
	require.Equal(t, []string{"spec"}, root.Required)
	require.Equal(t, apiextensionsv1.JSONSchemaProps{Type: "string"}, root.Properties["apiVersion"])
	require.Equal(t, apiextensionsv1.JSONSchemaProps{Type: "string"}, root.Properties["kind"])
	require.Equal(t, apiextensionsv1.JSONSchemaProps{Type: "object"}, root.Properties["metadata"])
	one, three := 1.0, 3.0
	require.Equal(t, apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"size": {
				Type: "integer", Format: "int32",
				Description: "How many memcached pods run.",
				Minimum:     &one, Maximum: &three,
			},
			"image": {
				Type:    "string",
				Pattern: "^memcached:",
				Default: &apiextensionsv1.JSON{Raw: []byte(`"memcached:1.6.26-alpine3.19"`)},
			},
		},
		Required: []string{"size"},
	}, root.Properties["spec"], "a field with a Default is optional")
	status := root.Properties["status"]
	require.Equal(t, "object", status.Type)
	require.Empty(t, status.Required)
	require.Equal(t, apiextensionsv1.JSONSchemaProps{Type: "integer", Format: "int32"}, status.Properties["readyReplicas"])
	conditions := status.Properties["conditions"]
	require.Equal(t, "array", conditions.Type)
	require.Equal(t, "map", *conditions.XListType)
	require.Equal(t, []string{"type"}, conditions.XListMapKeys)
}

func TestResource_CRD_of_a_cluster_scoped_resource(t *testing.T) {
	t.Parallel()

	crd, err := resourceOf[MemcachedSpec, MemcachedStatus]().CRD()
	require.NoError(t, err)
	require.Equal(t, apiextensionsv1.ClusterScoped, crd.Spec.Scope)
	require.Equal(t, "widgets.test.humus.dev", crd.Name)
}

type inner struct {
	A string `json:"a"`
}

func TestResource_CRD_maps_each_Go_type(t *testing.T) {
	t.Parallel()

	str := apiextensionsv1.JSONSchemaProps{Type: "string"}
	int32Schema := apiextensionsv1.JSONSchemaProps{Type: "integer", Format: "int32"}
	int64Schema := apiextensionsv1.JSONSchemaProps{Type: "integer", Format: "int64"}
	number := apiextensionsv1.JSONSchemaProps{Type: "number"}
	intOrString := apiextensionsv1.JSONSchemaProps{
		XIntOrString: true,
		AnyOf:        []apiextensionsv1.JSONSchemaProps{{Type: "integer"}, {Type: "string"}},
	}
	preserve := true

	tests := []struct {
		name string
		got  func(*testing.T) apiextensionsv1.JSONSchemaProps
		want apiextensionsv1.JSONSchemaProps
	}{
		{"bool", fieldSchema[bool], apiextensionsv1.JSONSchemaProps{Type: "boolean"}},
		{"int8", fieldSchema[int8], int32Schema},
		{"int16", fieldSchema[int16], int32Schema},
		{"int32", fieldSchema[int32], int32Schema},
		{"uint8", fieldSchema[uint8], int32Schema},
		{"uint16", fieldSchema[uint16], int32Schema},
		{"int", fieldSchema[int], int64Schema},
		{"int64", fieldSchema[int64], int64Schema},
		{"uint", fieldSchema[uint], int64Schema},
		{"uint32", fieldSchema[uint32], int64Schema},
		{"uint64", fieldSchema[uint64], int64Schema},
		{"float32", fieldSchema[float32], number},
		{"float64", fieldSchema[float64], number},
		{"string", fieldSchema[string], str},
		{"a named string", fieldSchema[kube.Scope], str},
		{"[]byte", fieldSchema[[]byte], apiextensionsv1.JSONSchemaProps{Type: "string", Format: "byte"}},
		{"a slice", fieldSchema[[]string], apiextensionsv1.JSONSchemaProps{
			Type:  "array",
			Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &str},
		}},
		{"an array", fieldSchema[[4]int64], apiextensionsv1.JSONSchemaProps{
			Type:  "array",
			Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &int64Schema},
		}},
		{"an array of bytes", fieldSchema[[2]byte], apiextensionsv1.JSONSchemaProps{
			Type:  "array",
			Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &int32Schema},
		}},
		{"map[string]V", fieldSchema[map[string]float64], apiextensionsv1.JSONSchemaProps{
			Type:                 "object",
			AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{Allows: true, Schema: &number},
		}},
		{"a map keyed by a named string", fieldSchema[map[kube.Scope]string], apiextensionsv1.JSONSchemaProps{
			Type:                 "object",
			AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{Allows: true, Schema: &str},
		}},
		{"a struct", fieldSchema[inner], apiextensionsv1.JSONSchemaProps{
			Type:       "object",
			Properties: map[string]apiextensionsv1.JSONSchemaProps{"a": str},
			Required:   []string{"a"},
		}},
		{"an empty struct", fieldSchema[struct{}], apiextensionsv1.JSONSchemaProps{Type: "object"}},
		{"*T", fieldSchema[*string], apiextensionsv1.JSONSchemaProps{Type: "string", Nullable: true}},
		{"metav1.Time", fieldSchema[metav1.Time], apiextensionsv1.JSONSchemaProps{Type: "string", Format: "date-time"}},
		{"*metav1.Time", fieldSchema[*metav1.Time], apiextensionsv1.JSONSchemaProps{Type: "string", Format: "date-time", Nullable: true}},
		{"metav1.MicroTime", fieldSchema[metav1.MicroTime], apiextensionsv1.JSONSchemaProps{Type: "string", Format: "date-time"}},
		{"metav1.Duration", fieldSchema[metav1.Duration], str},
		{"resource.Quantity", fieldSchema[resource.Quantity], intOrString},
		{"intstr.IntOrString", fieldSchema[intstr.IntOrString], intOrString},
		{"runtime.RawExtension", fieldSchema[runtime.RawExtension], apiextensionsv1.JSONSchemaProps{
			Type:                   "object",
			XPreserveUnknownFields: &preserve,
		}},
		{"metav1.ObjectMeta", fieldSchema[metav1.ObjectMeta], apiextensionsv1.JSONSchemaProps{Type: "object"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, tc.got(t))
		})
	}
}

func TestResource_CRD_reflects_a_template(t *testing.T) {
	t.Parallel()

	type template struct {
		Metadata metav1.ObjectMeta `json:"metadata,omitempty"`
		Port     int32             `json:"port"`
	}
	s := fieldSchema[template](t)
	require.Equal(t, apiextensionsv1.JSONSchemaProps{Type: "object"}, s.Properties["metadata"])
	require.Equal(t, []string{"port"}, s.Required)
}

func TestResource_CRD_requires_every_field_it_always_writes(t *testing.T) {
	t.Parallel()

	s := specSchema[struct {
		Plain     string `json:"plain"`
		Untagged  string
		Empty     string            `json:"empty,omitempty"`
		Zero      metav1.Time       `json:"zero,omitzero"`
		Both      int               `json:"both,omitempty,omitzero"`
		Slice     []string          `json:"slice"`
		Map       map[string]string `json:"map,omitempty"`
		Quoted    int64             `json:"quoted,string"`
		Skipped   string            `json:"-"`
		unwritten string
	}](t)

	require.Equal(t, []string{"plain", "Untagged", "slice", "quoted"}, s.Required)
	require.ElementsMatch(t,
		[]string{"plain", "Untagged", "empty", "zero", "both", "slice", "map", "quoted"},
		keys(s.Properties),
	)
	require.Equal(t, apiextensionsv1.JSONSchemaProps{Type: "string"}, s.Properties["quoted"], "the string option writes a number as a string")
}

func TestResource_CRD_marks_a_pointer_nullable_unless_it_is_left_out(t *testing.T) {
	t.Parallel()

	s := specSchema[struct {
		Null  *int32   `json:"null"`
		Empty *int32   `json:"empty,omitempty"`
		Zero  *int32   `json:"zero,omitzero"`
		Items []*int32 `json:"items"`
		Value int32    `json:"value"`
	}](t)

	require.True(t, s.Properties["null"].Nullable)
	require.False(t, s.Properties["empty"].Nullable)
	require.False(t, s.Properties["zero"].Nullable)
	require.True(t, s.Properties["items"].Items.Schema.Nullable, "a nil item is written as null")
	require.False(t, s.Properties["value"].Nullable)
	require.Equal(t, []string{"null", "items", "value"}, s.Required)
}

func TestResource_CRD_reflects_a_pointer_spec_and_status(t *testing.T) {
	t.Parallel()

	crd, err := resourceOf[*MemcachedSpec, *MemcachedStatus]().CRD()
	require.NoError(t, err)
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	require.True(t, root.Properties["spec"].Nullable, "Object's spec has no omitempty")
	require.False(t, root.Properties["status"].Nullable, "Object's status has omitempty")
	require.Equal(t, []string{"size", "image"}, root.Properties["spec"].Required)
}

type Base struct {
	Name  string `json:"name"`
	Shade string `json:"shade,omitempty"`
}

type Extra struct {
	Note string `json:"note"`
}

type hidden struct {
	Secret string `json:"secret"`
}

// Left and Right are embedded side by side. Their Clash fields are
// untagged, and Left's Winner is tagged with the name Right's has untagged.
type Left struct {
	Clash  string
	Winner string `json:"Winner"`
}

type Right struct {
	Clash  string
	Winner string
}

func TestResource_CRD_inlines_an_embedded_struct(t *testing.T) {
	t.Parallel()

	s := specSchema[struct {
		Base
		*Extra
		hidden
		Left
		Right
		Named Base `json:"named"`
		Shade int  `json:"shade"`
	}](t)

	// Base's name is promoted, and its shade is hidden by the shallower
	// field. Extra's note is promoted through a pointer, which, when nil,
	// leaves it out. hidden is unexported, but its exported field is still
	// promoted. Left's and Right's Clash annihilate each other, and Left's
	// tagged Winner wins over Right's untagged one.
	require.ElementsMatch(t,
		[]string{"name", "note", "secret", "Winner", "named", "shade"},
		keys(s.Properties),
	)
	require.Equal(t, []string{"name", "secret", "Winner", "named", "shade"}, s.Required)
	require.Equal(t, apiextensionsv1.JSONSchemaProps{Type: "integer", Format: "int64"}, s.Properties["shade"])
	require.Equal(t, "object", s.Properties["named"].Type, "an embedded struct with a name in its tag isn't inlined")
}

func TestResource_CRD_drops_a_struct_embedded_twice_at_one_depth(t *testing.T) {
	t.Parallel()

	type Twice struct{ Name string }
	type A struct{ Twice }
	type B struct{ Twice }
	s := specSchema[struct {
		A
		B
		Own string `json:"own"`
	}](t)
	require.Equal(t, []string{"own"}, keys(s.Properties))
}

type marshalsItself struct{ V int }

func (marshalsItself) MarshalJSON() ([]byte, error) { return []byte("0"), nil }

type marshalsByPointer struct{ V int }

func (*marshalsByPointer) MarshalJSON() ([]byte, error) { return []byte("0"), nil }

type node struct {
	Next *node `json:"next,omitempty"`
}

type list []list

func TestResource_CRD_reports_each_mistake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  func(*testing.T) error
		want string
	}{
		{"a json.Marshaler", crdError[struct {
			F marshalsItself `json:"f"`
		}], "spec.f: kube_test.marshalsItself encodes itself"},
		{"a json.Marshaler by pointer", crdError[struct {
			F marshalsByPointer `json:"f"`
		}], "spec.f: kube_test.marshalsByPointer encodes itself"},
		{"time.Time", crdError[struct {
			F time.Time `json:"f"`
		}], "spec.f: time.Time encodes itself"},
		{"an encoding.TextMarshaler", crdError[struct {
			F net.IP `json:"f"`
		}], "spec.f: net.IP encodes itself"},
		{"a spec that encodes itself", crdError[marshalsItself], "spec: kube_test.marshalsItself encodes itself"},
		{"a map keyed by integers", crdError[struct {
			F map[int]string `json:"f"`
		}], "spec.f: map[int]string has keys of int, not strings"},
		{"an interface", crdError[struct {
			F any `json:"f"`
		}], "spec.f: interface {} is an interface, which has no schema"},
		{"a channel", crdError[struct {
			F chan int `json:"f"`
		}], "spec.f: chan int is a chan, which has no JSON encoding"},
		{"a function", crdError[struct {
			F func() `json:"f"`
		}], "spec.f: func() is a func, which has no JSON encoding"},
		{"a complex number", crdError[struct {
			F complex128 `json:"f"`
		}], "spec.f: complex128 is a complex128, which has no JSON encoding"},
		{"a struct that contains itself", crdError[node], "spec.next: kube_test.node contains itself"},
		{"a slice that contains itself", crdError[list], "spec[*]: kube_test.list contains itself"},
		{"a mistake in an item", crdError[struct {
			F []map[string]any `json:"f"`
		}], "spec.f[*]{*}: interface {} is an interface"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.err(t)
			require.ErrorContains(t, err, "kube: the CRD of widgets.test.humus.dev: ")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestResource_CRD_reports_a_missing_name_or_a_bad_scope(t *testing.T) {
	t.Parallel()

	_, err := kube.Resource[MemcachedSpec, MemcachedStatus]{}.CRD()
	require.ErrorContains(t, err, "kube: the CRD of kube.Resource[")
	require.ErrorContains(t, err, "Resource.Group must not be empty")
	require.ErrorContains(t, err, "Resource.Version must not be empty")
	require.ErrorContains(t, err, "Resource.Kind must not be empty")
	require.ErrorContains(t, err, "Resource.Plural must not be empty")
	require.ErrorContains(t, err, `Resource.Scope is "", not "Namespaced" or "Cluster"`)

	r := Memcached
	r.Scope = "namespaced"
	_, err = r.CRD()
	require.EqualError(t, err, `kube: the CRD of memcacheds.cache.example.com: Resource.Scope is "namespaced", not "Namespaced" or "Cluster"`)
}

func TestResource_CRD_reports_every_mistake_together(t *testing.T) {
	t.Parallel()

	r := kube.Resource[struct {
		Handler func()      `json:"handler"`
		When    time.Time   `json:"when"`
		Next    *node       `json:"next"`
		Data    map[int]any `json:"data"`
	}, struct {
		Ch chan string `json:"ch"`
	}]{Group: "test.humus.dev", Kind: "Widget", Plural: "widgets", Scope: kube.Namespaced}

	crd, err := r.CRD()
	require.Nil(t, crd)
	msg := err.Error()
	for _, want := range []string{
		"Resource.Version must not be empty",
		"spec.handler: func()",
		"spec.when: time.Time encodes itself",
		"spec.next.next: *kube_test.node contains itself",
		"spec.data: map[int]interface {} has keys of int",
		"status.ch: chan string",
	} {
		require.Contains(t, msg, want)
	}
	require.Len(t, strings.Split(msg, "\n"), 6, "one line for each mistake: %s", msg)
}

// GoldenSpec is a spec with a field of each kind CRD maps.
type GoldenSpec struct {
	Base     `json:",inline"`
	Replicas *int32                         `json:"replicas,omitempty"`
	Paused   bool                           `json:"paused"`
	Weight   float64                        `json:"weight,omitempty"`
	Seed     []byte                         `json:"seed,omitempty"`
	Labels   map[string]string              `json:"labels,omitempty"`
	Ports    []Port                         `json:"ports"`
	Template Template                       `json:"template"`
	Timeout  metav1.Duration                `json:"timeout"`
	Memory   resource.Quantity              `json:"memory"`
	Target   intstr.IntOrString             `json:"target,omitzero"`
	Config   runtime.RawExtension           `json:"config,omitzero"`
	Limits   map[string]resource.Quantity   `json:"limits,omitempty"`
	Extra    *Extra                         `json:"extra"`
	Matrix   [][]int64                      `json:"matrix,omitempty"`
	Started  *metav1.Time                   `json:"started,omitempty"`
	Windows  map[string][]metav1.MicroTime  `json:"windows,omitempty"`
	Selector *metav1.LabelSelector          `json:"selector,omitempty"`
	Volumes  map[string]map[string]*float32 `json:"volumes,omitempty"`
	Tags     []string                       `json:"tags,omitempty"`
	Version  Version                        `json:"version,omitzero"`
}

// Version is a version, such as 1.2.3, that encodes itself as a string, so
// its schema is given whole with kube.Schema.
type Version struct{ Major, Minor, Patch int }

func (v Version) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch))
}

// Port is a port of GoldenSpec's.
type Port struct {
	Name string `json:"name"`
	Port uint16 `json:"port"`
}

// Template is a template of GoldenSpec's.
type Template struct {
	Metadata metav1.ObjectMeta `json:"metadata,omitempty"`
	Image    string            `json:"image"`
}

// GoldenStatus is a status as Kubernetes' conventions have it.
type GoldenStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

var (
	widget       GoldenSpec
	widgetPort   Port
	widgetStatus GoldenStatus

	// Widget is a resource with every mapped type, and a constraint of
	// every kind.
	Widget = kube.Resource[GoldenSpec, GoldenStatus]{
		Group:   "test.humus.dev",
		Version: "v1beta1",
		Kind:    "Widget",
		Plural:  "widgets",
		Scope:   kube.Namespaced,
		Rules: []kube.TypeRules{
			kube.Rules(&widget,
				kube.Field(&widget.Name).MinLength(1).MaxLength(63).Description("The widget's name."),
				kube.Field(&widget.Shade).Enum("red", "green", "blue"),
				kube.Field(&widget.Paused).Default(false),
				kube.Field(&widget.Weight).Minimum(0).Maximum(1),
				kube.Field(&widget.Ports).MinItems(1).MaxItems(8).ListMap("name"),
				kube.Field(&widget.Tags).ListSet(),
				kube.Field(&widget.Template).Validation("self.image != ''", "a template names its image"),
			).Description("A widget.").Validation("!has(self.replicas) || self.replicas >= 0", "replicas must not be negative"),
			kube.Rules(&widgetPort,
				kube.Field(&widgetPort.Port).Minimum(1),
			),
			kube.Rules(&widgetStatus,
				kube.Field(&widgetStatus.Conditions).ListMap("type"),
			),
			kube.Schema[Version](apiextensionsv1.JSONSchemaProps{
				Type:    "string",
				Pattern: `^[0-9]+\.[0-9]+\.[0-9]+$`,
			}).Description("A version, such as 1.2.3."),
		},
	}
)

// golden compares got with the file testdata/name, or rewrites the file
// with got when the tests run with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run go test -update to write it")
	require.Equal(t, string(want), string(got), "run go test -update to rewrite %s", path)
}

func TestResource_CRD_golden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		file string
		crd  func() (*apiextensionsv1.CustomResourceDefinition, error)
	}{
		{"memcached.yaml", Memcached.CRD},
		{"widget.yaml", Widget.CRD},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			crd, err := tc.crd()
			require.NoError(t, err)
			got, err := yaml.Marshal(crd)
			require.NoError(t, err)
			golden(t, tc.file, got)
		})
	}
}

func TestObject_encodes_as_an_object_of_its_Resource(t *testing.T) {
	t.Parallel()

	obj := kube.Object[MemcachedSpec, MemcachedStatus]{
		TypeMeta:   metav1.TypeMeta{APIVersion: "cache.example.com/v1alpha1", Kind: "Memcached"},
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "default"},
		Spec:       MemcachedSpec{Size: 2, Image: "memcached:1.6"},
	}
	b, err := json.Marshal(obj)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"apiVersion": "cache.example.com/v1alpha1",
		"kind": "Memcached",
		"metadata": {"name": "cache", "namespace": "default"},
		"spec": {"size": 2, "image": "memcached:1.6"},
		"status": {}
	}`, string(b))
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
