// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/z5labs/humus/kube"
)

// holder is a spec with one field, f, of type T.
type holder[T any] struct {
	F T `json:"f"`
}

// constrained returns the schema CRD reflects for a field of type T,
// constrained by rule, and whether the field is required.
func constrained[T any](t *testing.T, rule func(kube.FieldOf[T]) kube.FieldOf[T]) (apiextensionsv1.JSONSchemaProps, bool) {
	t.Helper()
	var v holder[T]
	r := resourceOf[holder[T], struct{}]()
	r.Rules = []kube.TypeRules{kube.Rules(&v, rule(kube.Field(&v.F)))}
	crd, err := r.CRD()
	require.NoError(t, err)
	s := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	return s.Properties["f"], len(s.Required) > 0
}

// ruled returns the spec schema CRD reflects for Spec, constrained by
// rules.
func ruled[Spec any](t *testing.T, rules ...kube.TypeRules) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	r := resourceOf[Spec, struct{}]()
	r.Rules = rules
	crd, err := r.CRD()
	require.NoError(t, err)
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
}

// ruleError returns CRD's error for Spec, constrained by rules.
func ruleError[Spec any](t *testing.T, rules ...kube.TypeRules) error {
	t.Helper()
	r := resourceOf[Spec, struct{}]()
	r.Rules = rules
	crd, err := r.CRD()
	require.Error(t, err)
	require.Nil(t, crd)
	return err
}

func raw(s string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(s)} }

func ptr[T any](v T) *T { return &v }

// quoted is a spec whose field has the string option.
type quoted struct {
	N int64 `json:"n,string"`
}

func TestFieldOf_each_constraint_gives_its_schema(t *testing.T) {
	t.Parallel()

	str := apiextensionsv1.JSONSchemaProps{Type: "string"}
	tests := []struct {
		name     string
		got      func(*testing.T) (apiextensionsv1.JSONSchemaProps, bool)
		want     apiextensionsv1.JSONSchemaProps
		required bool
	}{
		{
			"Description",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[int32]) kube.FieldOf[int32] { return f.Description("How many.") })
			},
			apiextensionsv1.JSONSchemaProps{Type: "integer", Format: "int32", Description: "How many."},
			true,
		},
		{
			"Default",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[string]) kube.FieldOf[string] { return f.Default("memcached") })
			},
			apiextensionsv1.JSONSchemaProps{Type: "string", Default: raw(`"memcached"`)},
			false,
		},
		{
			"Default of a struct",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[Port]) kube.FieldOf[Port] {
					return f.Default(Port{Name: "http", Port: 80})
				})
			},
			apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"name": str,
					"port": {Type: "integer", Format: "int32"},
				},
				Required: []string{"name", "port"},
				Default:  raw(`{"name":"http","port":80}`),
			},
			false,
		},
		{
			"Enum",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[kube.Scope]) kube.FieldOf[kube.Scope] {
					return f.Enum(kube.Namespaced, kube.Cluster)
				})
			},
			apiextensionsv1.JSONSchemaProps{Type: "string", Enum: []apiextensionsv1.JSON{*raw(`"Namespaced"`), *raw(`"Cluster"`)}},
			true,
		},
		{
			"Minimum and Maximum of an integer",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[int32]) kube.FieldOf[int32] { return f.Minimum(1).Maximum(3) })
			},
			apiextensionsv1.JSONSchemaProps{Type: "integer", Format: "int32", Minimum: ptr(1.0), Maximum: ptr(3.0)},
			true,
		},
		{
			"Minimum and Maximum of a number",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[float64]) kube.FieldOf[float64] { return f.Minimum(-0.5).Maximum(0.5) })
			},
			apiextensionsv1.JSONSchemaProps{Type: "number", Minimum: ptr(-0.5), Maximum: ptr(0.5)},
			true,
		},
		{
			"Minimum of a pointer",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[*uint16]) kube.FieldOf[*uint16] { return f.Minimum(ptr[uint16](2)) })
			},
			apiextensionsv1.JSONSchemaProps{Type: "integer", Format: "int32", Nullable: true, Minimum: ptr(2.0)},
			true,
		},
		{
			"MinLength, MaxLength and Pattern",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[string]) kube.FieldOf[string] {
					return f.MinLength(1).MaxLength(63).Pattern(regexp.MustCompile(`^[a-z]+$`))
				})
			},
			apiextensionsv1.JSONSchemaProps{Type: "string", MinLength: ptr[int64](1), MaxLength: ptr[int64](63), Pattern: "^[a-z]+$"},
			true,
		},
		{
			"MinItems and MaxItems",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[[]string]) kube.FieldOf[[]string] { return f.MinItems(0).MaxItems(4) })
			},
			apiextensionsv1.JSONSchemaProps{
				Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &str},
				MinItems: ptr[int64](0), MaxItems: ptr[int64](4),
			},
			true,
		},
		{
			"ListSet",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[[]string]) kube.FieldOf[[]string] { return f.ListSet() })
			},
			apiextensionsv1.JSONSchemaProps{
				Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &str},
				XListType: ptr("set"),
			},
			true,
		},
		{
			"ListMap",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[[2]*Port]) kube.FieldOf[[2]*Port] { return f.ListMap("name", "port") })
			},
			apiextensionsv1.JSONSchemaProps{
				Type: "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"name": str,
						"port": {Type: "integer", Format: "int32"},
					},
					Required: []string{"name", "port"},
					Nullable: true,
				}},
				XListType:    ptr("map"),
				XListMapKeys: []string{"name", "port"},
			},
			true,
		},
		{
			"Validation",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[string]) kube.FieldOf[string] {
					return f.Validation("self.startsWith('a')", "starts with a").Validation("size(self) < 9", "")
				})
			},
			apiextensionsv1.JSONSchemaProps{Type: "string", XValidations: apiextensionsv1.ValidationRules{
				{Rule: "self.startsWith('a')", Message: "starts with a"},
				{Rule: "size(self) < 9"},
			}},
			true,
		},
		{
			"the later of two constraints of one kind",
			func(t *testing.T) (apiextensionsv1.JSONSchemaProps, bool) {
				return constrained(t, func(f kube.FieldOf[[]string]) kube.FieldOf[[]string] {
					return f.MinItems(1).MinItems(2).ListSet()
				})
			},
			apiextensionsv1.JSONSchemaProps{
				Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &str},
				MinItems: ptr[int64](2), XListType: ptr("set"),
			},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, required := tc.got(t)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.required, required)
		})
	}
}

func TestFieldOf_Default_of_a_quoted_field_is_a_string(t *testing.T) {
	t.Parallel()

	var v quoted
	s := ruled[quoted](t, kube.Rules(&v, kube.Field(&v.N).Default(5).Enum(5, 6)))
	require.Equal(t, apiextensionsv1.JSONSchemaProps{
		Type:    "string",
		Default: raw(`"5"`),
		Enum:    []apiextensionsv1.JSON{*raw(`"5"`), *raw(`"6"`)},
	}, s.Properties["n"])
	require.Empty(t, s.Required)
}

func TestFieldOf_each_method_returns_a_copy(t *testing.T) {
	t.Parallel()

	type pair struct {
		A int32 `json:"a"`
		B int32 `json:"b"`
	}
	var v pair
	base := kube.Field(&v.A).Minimum(1)
	a := base.Maximum(2)
	b := base.Maximum(3)
	s := ruled[pair](t, kube.Rules(&v, a))
	require.Equal(t, ptr(2.0), s.Properties["a"].Maximum)
	s = ruled[pair](t, kube.Rules(&v, b))
	require.Equal(t, ptr(3.0), s.Properties["a"].Maximum)
	s = ruled[pair](t, kube.Rules(&v, base))
	require.Nil(t, s.Properties["a"].Maximum)
	require.Equal(t, ptr(1.0), s.Properties["a"].Minimum)
}

func TestTypeRules_describe_and_constrain_the_type(t *testing.T) {
	t.Parallel()

	var v Port
	rules := kube.Rules(&v).Description("A port.")
	withRule := rules.Validation("self.port > 0", "a port is positive")
	s := ruled[holder[Port]](t, withRule.Validation("self.name != ''", ""))
	require.Equal(t, "A port.", s.Properties["f"].Description)
	require.Equal(t, apiextensionsv1.ValidationRules{
		{Rule: "self.port > 0", Message: "a port is positive"},
		{Rule: "self.name != ''"},
	}, s.Properties["f"].XValidations)

	s = ruled[holder[Port]](t, rules)
	require.Empty(t, s.Properties["f"].XValidations, "Validation returns a copy")
}

// ports is a list type of its own, constrained as a whole.
type ports []Port

func TestRules_of_a_type_that_isnt_a_struct(t *testing.T) {
	t.Parallel()

	var (
		v  holder[ports]
		ps ports
	)
	s := ruled[holder[ports]](t, kube.Rules(&ps).Validation("self.all(p, p.port > 0)", "every port is positive"))
	require.Equal(t, apiextensionsv1.ValidationRules{{Rule: "self.all(p, p.port > 0)", Message: "every port is positive"}}, s.Properties["f"].XValidations)

	err := ruleError[holder[ports]](t, kube.Rules(&ps, kube.Field(&v.F)))
	require.ErrorContains(t, err, "Resource.Rules[0], of kube_test.ports: fields[0], a Field of type kube_test.ports, names no field of the value given to Rules")
}

func TestRules_names_a_promoted_field(t *testing.T) {
	t.Parallel()

	type spec struct {
		Base
		Size int32 `json:"size"`
	}
	var v spec
	s := ruled[spec](t, kube.Rules(&v,
		kube.Field(&v.Name).MinLength(1),
		kube.Field(&v.Base.Shade).Enum("red"),
	))
	require.Equal(t, ptr[int64](1), s.Properties["name"].MinLength)
	require.Equal(t, []apiextensionsv1.JSON{*raw(`"red"`)}, s.Properties["shade"].Enum)
}

func TestRules_of_an_embedded_struct_constrain_its_promoted_fields(t *testing.T) {
	t.Parallel()

	type spec struct {
		Base
		*Extra
	}
	var (
		v     spec
		base  Base
		extra Extra
	)
	s := ruled[spec](t,
		kube.Rules(&base, kube.Field(&base.Name).MinLength(1).MaxLength(9)),
		kube.Rules(&extra, kube.Field(&extra.Note).Pattern(regexp.MustCompile(`^n`))),
		kube.Rules(&v, kube.Field(&v.Name).MaxLength(5)),
	)
	require.Equal(t, ptr[int64](1), s.Properties["name"].MinLength, "Base's own rule")
	require.Equal(t, ptr[int64](5), s.Properties["name"].MaxLength, "the outer type's rule wins")
	require.Equal(t, "^n", s.Properties["note"].Pattern, "promoted through a pointer")
}

func TestRules_constrain_a_type_alike_wherever_it_appears(t *testing.T) {
	t.Parallel()

	type spec struct {
		Main   Port            `json:"main"`
		Backup *Port           `json:"backup,omitempty"`
		Others []Port          `json:"others"`
		ByName map[string]Port `json:"byName"`
	}
	var p Port
	s := ruled[spec](t, kube.Rules(&p, kube.Field(&p.Port).Minimum(1).Maximum(65535)))
	for _, got := range []apiextensionsv1.JSONSchemaProps{
		s.Properties["main"],
		s.Properties["backup"],
		*s.Properties["others"].Items.Schema,
		*s.Properties["byName"].AdditionalProperties.Schema,
	} {
		require.Equal(t, ptr(1.0), got.Properties["port"].Minimum)
		require.Equal(t, ptr(65535.0), got.Properties["port"].Maximum)
	}
}

func TestRules_constrain_a_nested_struct_through_its_own_rules(t *testing.T) {
	t.Parallel()

	type spec struct {
		Template Template `json:"template"`
	}
	var (
		v    spec
		tmpl Template
	)
	s := ruled[spec](t,
		kube.Rules(&v, kube.Field(&v.Template).Description("The pods' template.")),
		kube.Rules(&tmpl, kube.Field(&tmpl.Image).Default("memcached")),
	)
	template := s.Properties["template"]
	require.Equal(t, "The pods' template.", template.Description)
	require.Equal(t, raw(`"memcached"`), template.Properties["image"].Default)
	require.Empty(t, template.Required, "a field with a Default is optional")
}

func TestRules_of_the_spec_and_the_status(t *testing.T) {
	t.Parallel()

	crd, err := Memcached.CRD()
	require.NoError(t, err)
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	require.Equal(t, ptr(1.0), root.Properties["spec"].Properties["size"].Minimum)
	require.Equal(t, ptr("map"), root.Properties["status"].Properties["conditions"].XListType)
}

// semver encodes itself as a string, so its schema is given whole.
type semver struct{ Major, Minor int }

func (semver) MarshalJSON() ([]byte, error) { return []byte(`"1.2"`), nil }

func TestSchema_gives_a_type_that_encodes_itself_its_schema(t *testing.T) {
	t.Parallel()

	type spec struct {
		Version  semver   `json:"version"`
		Previous *semver  `json:"previous"`
		History  []semver `json:"history"`
	}
	var v spec
	s := ruled[spec](t,
		kube.Schema[semver](apiextensionsv1.JSONSchemaProps{Type: "string", Pattern: `^\d+\.\d+$`}).
			Description("A version.").
			Validation("self != '0.0'", "a version isn't 0.0"),
		kube.Rules(&v, kube.Field(&v.Version).Default(semver{1, 2}).Description("The version to run.")),
	)
	want := apiextensionsv1.JSONSchemaProps{
		Type:         "string",
		Pattern:      `^\d+\.\d+$`,
		Description:  "A version.",
		XValidations: apiextensionsv1.ValidationRules{{Rule: "self != '0.0'", Message: "a version isn't 0.0"}},
	}
	require.Equal(t, want, *s.Properties["history"].Items.Schema)

	nullable := want
	nullable.Nullable = true
	require.Equal(t, nullable, s.Properties["previous"])

	version := want
	version.Description = "The version to run."
	version.Default = raw(`"1.2"`)
	require.Equal(t, version, s.Properties["version"])
	require.Equal(t, []string{"previous", "history"}, s.Required)
}

// numberish has a schema of an integer, but encodes as a string.
type numberish struct{}

func (numberish) MarshalJSON() ([]byte, error) { return []byte(`"one"`), nil }

// mixed has a field of each kind a mistake is made on.
type mixed struct {
	Size     int32             `json:"size"`
	Image    string            `json:"image"`
	Tags     []string          `json:"tags"`
	Ports    []Port            `json:"ports"`
	Memory   resource.Quantity `json:"memory"`
	Ratio    float64           `json:"ratio"`
	Count    numberish         `json:"count"`
	Window   [2]int32          `json:"window"`
	Template Template          `json:"template"`
	Skipped  string            `json:"-"`
	Ptr      *Extra            `json:"ptr"`
	hidden   string
}

func TestResource_CRD_reports_each_mistake_in_its_rules(t *testing.T) {
	t.Parallel()

	var (
		v     mixed
		other mixed
		tmpl  Template
	)
	count := kube.Schema[numberish](apiextensionsv1.JSONSchemaProps{Type: "integer"})
	const of = "Resource.Rules[0], of kube_test.mixed: "

	tests := []struct {
		name  string
		rules []kube.TypeRules
		want  string
	}{
		{"Pattern on a number", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Size).Pattern(regexp.MustCompile(`^1`)))},
			of + "the field size: Pattern is for a string, but the field is an integer"},
		{"MinLength on a list", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Tags).MinLength(1))},
			of + "the field tags: MinLength is for a string, but the field is an array"},
		{"MaxLength on a number", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Ratio).MaxLength(1))},
			of + "the field ratio: MaxLength is for a string, but the field is a number"},
		{"Minimum on a string", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Image).Minimum("a"))},
			of + "the field image: Minimum is for a number, but the field is a string"},
		{"Maximum on a quantity", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Memory).Maximum(resource.MustParse("1Gi")))},
			of + "the field memory: Maximum is for a number, but the field is an int or a string"},
		{"MinItems on a string", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Image).MinItems(1))},
			of + "the field image: MinItems is for an array, but the field is a string"},
		{"MaxItems on a struct", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Template).MaxItems(1))},
			of + "the field template: MaxItems is for an array, but the field is an object"},
		{"ListSet on a number", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Size).ListSet())},
			of + "the field size: ListSet is for an array, but the field is an integer"},
		{"ListMap on a string", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Image).ListMap("name"))},
			of + "the field image: ListMap is for an array, but the field is a string"},
		{"ListMap on a list of strings", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Tags).ListMap("name"))},
			of + "the field tags: ListMap is for a list of objects, but the list's items are a string"},
		{"ListMap with no key", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Ports).ListMap())},
			of + "the field ports: ListMap has no key"},
		{"ListMap with a key that is no field", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Ports).ListMap("name", "nmae"))},
			of + `the field ports: ListMap's key "nmae" is no field of the list's items`},
		{"a negative MinLength", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Image).MinLength(-1))},
			of + "the field image: MinLength is -1, which is negative"},
		{"a negative MaxItems", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Tags).MaxItems(-2))},
			of + "the field tags: MaxItems is -2, which is negative"},
		{"a nil Pattern", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Image).Pattern(nil))},
			of + "the field image: Pattern is given a nil regular expression"},
		{"an Enum of no value", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Image).Enum())},
			of + "the field image: Enum has no value"},
		{"a field Validation with an empty rule", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Image).Validation("", "never"))},
			of + "the field image: Validation has an empty rule"},
		{"a type Validation with an empty rule", []kube.TypeRules{kube.Rules(&v).Validation("", "never")},
			of + "Validation has an empty rule"},
		{"a Default that doesn't encode", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Ratio).Default(math.NaN()))},
			of + "the field ratio: Default's value doesn't encode: json: unsupported value: NaN"},
		{"a Minimum that encodes as no number", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Count).Minimum(numberish{})), count},
			of + `the field count: Minimum's value encodes as "one", not a number`},
		{"a Field of another value", []kube.TypeRules{kube.Rules(&v, kube.Field(&other.Size).Minimum(1))},
			of + "fields[0], a Field of type int32, names no field of the value given to Rules"},
		{"a Field of a nested struct's field", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Template.Image).Default("a"))},
			of + "fields[0], a Field of type string, names Image, a field of kube_test.Template, which only Rules of kube_test.Template constrain"},
		{"a Field of a field encoding/json skips", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Skipped))},
			of + "fields[0], a Field of type string, names Skipped, a field encoding/json doesn't write"},
		{"a Field of an unexported field", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.hidden))},
			of + "fields[0], a Field of type string, names hidden, a field encoding/json doesn't write"},
		{"a Field of an item of an array", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Window[1]))},
			of + "fields[0], a Field of type int32, names an item of an array, not a field"},
		{"a Field of the value itself", []kube.TypeRules{kube.Rules(&v, kube.Field(&v))},
			of + "fields[0], a Field of type kube_test.mixed, names no field of the value given to Rules"},
		{"Field(nil)", []kube.TypeRules{kube.Rules(&v, kube.Field[string](nil))},
			of + "fields[0] points to nothing: it is Field(nil), or the zero FieldOf"},
		{"the zero FieldOf", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Size), kube.FieldOf[int]{})},
			of + "fields[1] points to nothing: it is Field(nil), or the zero FieldOf"},
		{"a nil FieldRule", []kube.TypeRules{kube.Rules(&v, nil)},
			of + "fields[0] is nil"},
		{"two Fields of one field", []kube.TypeRules{kube.Rules(&v, kube.Field(&v.Size).Minimum(1), kube.Field(&v.Size).Maximum(3))},
			of + "fields[1] names the field size, as fields[0] does"},
		{"a nil value", []kube.TypeRules{kube.Rules[mixed](nil)},
			of + "Rules is given a nil value, which no Field can point into"},
		{"the zero TypeRules", []kube.TypeRules{{}},
			"Resource.Rules[0] is the zero TypeRules"},
		{"two Rules for one type", []kube.TypeRules{kube.Rules(&tmpl), kube.Rules(&v), kube.Rules(&tmpl)},
			"Resource.Rules[2] constrains kube_test.Template, as Resource.Rules[0] does"},
		{"two Schemas for one type", []kube.TypeRules{count, count},
			"Resource.Rules[1] constrains kube_test.numberish, as Resource.Rules[0] does"},
		{"Rules and a Schema for one type", []kube.TypeRules{kube.Rules(&v), kube.Schema[mixed](apiextensionsv1.JSONSchemaProps{Type: "object"})},
			"Resource.Rules[1] constrains kube_test.mixed, as Resource.Rules[0] does"},
		{"Rules of fields of a type that encodes itself", []kube.TypeRules{func() kube.TypeRules {
			var s semverFields
			return kube.Rules(&s, kube.Field(&s.Major).Minimum(0))
		}()}, "Resource.Rules[0], of kube_test.semverFields: kube_test.semverFields encodes itself, so its fields aren't its schema: give it a Schema"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ruleError[mixed](t, tc.rules...)
			require.ErrorContains(t, err, "kube: the CRD of widgets.test.humus.dev: ")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// semverFields encodes itself, and has fields.
type semverFields struct {
	Major int `json:"major"`
}

func (semverFields) MarshalText() ([]byte, error) { return []byte("1"), nil }

func TestResource_CRD_reports_every_mistake_in_its_rules_together(t *testing.T) {
	t.Parallel()

	var (
		v    mixed
		tmpl Template
	)
	r := kube.Resource[mixed, struct{}]{
		Group: "test.humus.dev", Version: "v1", Kind: "Widget", Plural: "widgets", Scope: kube.Namespaced,
		Rules: []kube.TypeRules{
			kube.Rules(&v,
				kube.Field(&v.Size).Pattern(regexp.MustCompile(`^1`)).MinItems(1),
				kube.Field(&v.Template.Image),
			),
			kube.Rules(&tmpl, kube.Field(&tmpl.Image).Minimum("x")),
			kube.Rules(&tmpl),
		},
	}
	crd, err := r.CRD()
	require.Nil(t, crd)
	msg := err.Error()
	for _, want := range []string{
		"Resource.Rules[2] constrains kube_test.Template, as Resource.Rules[1] does",
		"Resource.Rules[0], of kube_test.mixed: fields[1], a Field of type string, names Image",
		"Resource.Rules[0], of kube_test.mixed: the field size: Pattern is for a string",
		"Resource.Rules[0], of kube_test.mixed: the field size: MinItems is for an array",
		"Resource.Rules[1], of kube_test.Template: the field image: Minimum is for a number",
		"spec.count: kube_test.numberish encodes itself",
	} {
		require.Contains(t, msg, want)
	}
	require.Len(t, strings.Split(msg, "\n"), 6, "one line for each mistake: %s", msg)
}
