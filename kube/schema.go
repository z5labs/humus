// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"cmp"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The types whose schemas aren't reflected from their fields, since each
// encodes itself, and the schema each has, as Kubernetes' own types use
// them.
var known = map[reflect.Type]func() apiextensionsv1.JSONSchemaProps{
	reflect.TypeFor[metav1.Time]():      dateTime,
	reflect.TypeFor[metav1.MicroTime](): dateTime,
	reflect.TypeFor[metav1.Duration](): func() apiextensionsv1.JSONSchemaProps {
		return apiextensionsv1.JSONSchemaProps{Type: "string"}
	},
	reflect.TypeFor[resource.Quantity]():  intOrString,
	reflect.TypeFor[intstr.IntOrString](): intOrString,
	reflect.TypeFor[runtime.RawExtension](): func() apiextensionsv1.JSONSchemaProps {
		preserve := true
		return apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}
	},
	// An object's metadata, in a template such as a Deployment's pods'. The
	// API server keeps it as it is given.
	reflect.TypeFor[metav1.ObjectMeta](): func() apiextensionsv1.JSONSchemaProps {
		return apiextensionsv1.JSONSchemaProps{Type: "object"}
	},
}

func dateTime() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{Type: "string", Format: "date-time"}
}

func intOrString() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		XIntOrString: true,
		AnyOf: []apiextensionsv1.JSONSchemaProps{
			{Type: "integer"},
			{Type: "string"},
		},
	}
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

// marshals reports whether t, or a pointer to t, implements json.Marshaler
// or encoding.TextMarshaler, which encoding/json encodes it with in place
// of its fields.
func marshals(t reflect.Type) bool {
	for _, m := range []reflect.Type{jsonMarshaler, textMarshaler} {
		if t.Implements(m) || reflect.PointerTo(t).Implements(m) {
			return true
		}
	}
	return false
}

// reflector reflects Go types into a structural schema, constrained by
// their rules, and records every mistake it finds, each naming where in the
// object it is.
type reflector struct {
	errs []error

	// rules are the rules of each type that has them.
	rules map[reflect.Type]*typeRules

	// onPath holds the types that hold the one being reflected, so a type
	// that contains itself is found rather than reflected forever.
	onPath map[reflect.Type]bool
}

func newReflector(rules map[reflect.Type]*typeRules) *reflector {
	return &reflector{rules: rules, onPath: map[reflect.Type]bool{}}
}

func (r *reflector) fail(path string, format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

// root returns the schema of an object whose spec is a spec and whose
// status is a status, as Object encodes them.
func (r *reflector) root(spec, status reflect.Type) *apiextensionsv1.JSONSchemaProps {
	// Object's spec has no omitempty, so a nil *Spec is null, and its
	// status has, so a nil *Status is left out.
	specSchema := r.schema("spec", spec)
	statusSchema := r.schema("status", status)
	statusSchema.Nullable = false
	return &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"apiVersion": {Type: "string"},
			"kind":       {Type: "string"},
			"metadata":   {Type: "object"},
			"spec":       specSchema,
			"status":     statusSchema,
		},
		Required: []string{"spec"},
	}
}

// schema returns t's schema, at path in the object, with its type's own
// description and validations.
func (r *reflector) schema(path string, t reflect.Type) apiextensionsv1.JSONSchemaProps {
	rules := r.rules[t]
	if rules != nil && rules.schema != nil {
		s := *rules.schema.DeepCopy()
		rules.decorate(&s)
		return s
	}
	s := r.reflectType(path, t)
	if rules != nil {
		rules.decorate(&s)
	}
	return s
}

// reflectType returns the schema reflected from t, at path in the object.
func (r *reflector) reflectType(path string, t reflect.Type) apiextensionsv1.JSONSchemaProps {
	if s, ok := known[t]; ok {
		return s()
	}
	if t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface && marshals(t) {
		r.fail(path, "%s encodes itself, with a MarshalJSON or MarshalText method, so its fields aren't its encoding", t)
		return apiextensionsv1.JSONSchemaProps{}
	}

	switch t.Kind() {
	case reflect.Bool:
		return apiextensionsv1.JSONSchemaProps{Type: "boolean"}
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Uint8, reflect.Uint16:
		return apiextensionsv1.JSONSchemaProps{Type: "integer", Format: "int32"}
	case reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return apiextensionsv1.JSONSchemaProps{Type: "integer", Format: "int64"}
	case reflect.Float32, reflect.Float64:
		return apiextensionsv1.JSONSchemaProps{Type: "number"}
	case reflect.String:
		return apiextensionsv1.JSONSchemaProps{Type: "string"}
	case reflect.Interface:
		r.fail(path, "%s is an interface, which has no schema", t)
		return apiextensionsv1.JSONSchemaProps{}
	case reflect.Chan, reflect.Func, reflect.Complex64, reflect.Complex128, reflect.UnsafePointer:
		r.fail(path, "%s is a %s, which has no JSON encoding", t, t.Kind())
		return apiextensionsv1.JSONSchemaProps{}
	}

	// The rest hold other types, so each could hold itself.
	if r.onPath[t] {
		r.fail(path, "%s contains itself, which a schema without $ref can't express", t)
		return apiextensionsv1.JSONSchemaProps{}
	}
	r.onPath[t] = true
	defer delete(r.onPath, t)

	switch t.Kind() {
	case reflect.Pointer:
		s := r.schema(path, t.Elem())
		s.Nullable = true
		return s
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 && !marshals(t.Elem()) {
			// encoding/json writes a []byte as base64.
			return apiextensionsv1.JSONSchemaProps{Type: "string", Format: "byte"}
		}
		return r.array(path, t)
	case reflect.Array:
		return r.array(path, t)
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			r.fail(path, "%s has keys of %s, not strings", t, t.Key())
			return apiextensionsv1.JSONSchemaProps{}
		}
		values := r.schema(path+"{*}", t.Elem())
		return apiextensionsv1.JSONSchemaProps{
			Type:                 "object",
			AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{Allows: true, Schema: &values},
		}
	default: // reflect.Struct
		return r.object(path, t)
	}
}

func (r *reflector) array(path string, t reflect.Type) apiextensionsv1.JSONSchemaProps {
	items := r.schema(path+"[*]", t.Elem())
	return apiextensionsv1.JSONSchemaProps{
		Type:  "array",
		Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &items},
	}
}

// object returns the schema of the struct t: a property for each field
// encoding/json writes, and every field it always writes required.
func (r *reflector) object(path string, t reflect.Type) apiextensionsv1.JSONSchemaProps {
	s := apiextensionsv1.JSONSchemaProps{Type: "object"}
	for _, f := range jsonFields(t) {
		p := r.schema(path+"."+f.name, f.typ)
		if f.quoted {
			// The ,string option writes a number or a bool as a string.
			p = apiextensionsv1.JSONSchemaProps{Type: "string"}
		}
		if f.typ.Kind() == reflect.Pointer {
			p.Nullable = !f.omit
		}
		defaulted := r.constrain(&p, t, f.index)
		if s.Properties == nil {
			s.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
		}
		s.Properties[f.name] = p
		if !f.omit && !f.viaPointer && !defaulted {
			s.Required = append(s.Required, f.name)
		}
	}
	return s
}

// constrain gives p, the schema of the field of the struct t at index, the
// field's constraints, and reports whether it has a Default. A field
// promoted from an embedded struct has its constraints from that struct's
// Rules, and then from t's, which win.
func (r *reflector) constrain(p *apiextensionsv1.JSONSchemaProps, t reflect.Type, index []int) bool {
	// owners[d] is the struct that holds index[d:].
	owners := make([]reflect.Type, len(index))
	owner := t
	for d, i := range index {
		owners[d] = owner
		owner = owner.Field(i).Type
		if owner.Kind() == reflect.Pointer {
			owner = owner.Elem()
		}
	}

	defaulted := false
	for d := len(index) - 1; d >= 0; d-- {
		rules := r.rules[owners[d]]
		if rules == nil {
			continue
		}
		if f, ok := rules.fields[indexKey(index[d:])]; ok {
			f.apply(p)
			defaulted = defaulted || f.defaulted
		}
	}
	return defaulted
}

// field is one field of a struct as encoding/json writes it.
type field struct {
	name   string
	tagged bool  // named by its json tag
	index  []int // its index, through the embedded structs that hold it
	typ    reflect.Type

	// omit is whether the field's tag has omitempty or omitzero, so it may
	// be left out.
	omit bool

	// quoted is whether the field's tag has the string option, and its
	// type is one the option writes as a string.
	quoted bool

	// viaPointer is whether the field is promoted from an embedded pointer,
	// which, when nil, leaves the field out.
	viaPointer bool
}

// jsonFields returns the fields of the struct t that encoding/json writes,
// in the order it writes them. It follows encoding/json's rules: an
// embedded struct with no name in its tag has its fields promoted, and of
// the fields with one name, the shallowest is written, a tagged one before
// an untagged one at the same depth, and none when that leaves two.
func jsonFields(t reflect.Type) []field {
	// embedded is a struct whose fields are promoted, and how it is
	// reached.
	type embedded struct {
		typ        reflect.Type
		index      []int
		viaPointer bool
	}

	var fields []field
	next := []embedded{{typ: t}}
	// count is how many times each struct of this depth is embedded at
	// it, and nextCount each of the next depth.
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}
	for len(next) > 0 {
		current := next
		next = nil
		count, nextCount = nextCount, map[reflect.Type]int{}

		for _, e := range current {
			if visited[e.typ] {
				continue
			}
			visited[e.typ] = true

			for i := range e.typ.NumField() {
				sf := e.typ.Field(i)
				if sf.Anonymous {
					ft := sf.Type
					if ft.Kind() == reflect.Pointer {
						ft = ft.Elem()
					}
					if !sf.IsExported() && ft.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if !validName(name) {
					name = ""
				}
				index := append(slices.Clone(e.index), i)

				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}

				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					f := field{
						name:       cmp.Or(name, sf.Name),
						tagged:     name != "",
						index:      index,
						typ:        sf.Type,
						viaPointer: e.viaPointer,
					}
					for _, opt := range strings.Split(opts, ",") {
						switch opt {
						case "omitempty", "omitzero":
							f.omit = true
						case "string":
							switch ft.Kind() {
							case reflect.Bool,
								reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
								reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
								reflect.Float32, reflect.Float64,
								reflect.String:
								f.quoted = true
							}
						}
					}
					fields = append(fields, f)
					if count[e.typ] > 1 {
						// The struct is embedded twice at this depth, so
						// each of its fields is there twice, and neither
						// is written.
						fields = append(fields, f)
					}
					continue
				}

				nextCount[ft]++
				if nextCount[ft] == 1 {
					next = append(next, embedded{
						typ:        ft,
						index:      index,
						viaPointer: e.viaPointer || sf.Type.Kind() == reflect.Pointer,
					})
				}
			}
		}
	}

	// Of the fields with one name, keep the dominant one, if there is one.
	slices.SortStableFunc(fields, func(a, b field) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.index), len(b.index)); c != 0 {
			return c
		}
		if a.tagged != b.tagged {
			if a.tagged {
				return -1
			}
			return 1
		}
		return slices.Compare(a.index, b.index)
	})
	var out []field
	for i := 0; i < len(fields); {
		j := i + 1
		for j < len(fields) && fields[j].name == fields[i].name {
			j++
		}
		if f, ok := dominant(fields[i:j]); ok {
			out = append(out, f)
		}
		i = j
	}
	slices.SortFunc(out, func(a, b field) int { return slices.Compare(a.index, b.index) })
	return out
}

// dominant returns the field encoding/json writes of fields, which share a
// name and are sorted shallowest first, tagged first. It reports false
// when there is none.
func dominant(fields []field) (field, bool) {
	if len(fields) > 1 && len(fields[0].index) == len(fields[1].index) && fields[0].tagged == fields[1].tagged {
		return field{}, false
	}
	return fields[0], true
}

// validName reports whether encoding/json takes name, from a json tag, as
// a field's name.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}
