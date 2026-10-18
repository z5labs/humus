// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// TypeRules constrain the fields of one Go type, wherever the type appears
// in a schema. [Rules] and [Schema] return them, and [Resource.Rules] holds
// them.
type TypeRules struct {
	typ reflect.Type

	// schema is Schema's, or nil for Rules.
	schema *apiextensionsv1.JSONSchemaProps

	description string
	validations []apiextensionsv1.ValidationRule
	fields      []fieldRules

	// errs are the mistakes Rules found, which CRD reports.
	errs []string
}

// fieldRules are the constraints of one field of a TypeRules' type.
type fieldRules struct {
	pos int   // the Field's position among Rules' fields
	f   field // the field, as jsonFields gives it
	cs  []constraint
}

// Rules constrains the fields of T. v only names them: each of fields is a
// [Field] of a field of *v, such as kube.Field(&v.Size), and Rules reads no
// value from *v. A field promoted from an embedded struct is T's own, and
// is named through v as encoding/json writes it, as &v.Name. One promoted
// from an embedded pointer can't be named, since it isn't inside *v.
//
// The rules belong to T, not to a place in an object: they constrain T's
// fields wherever T appears in Spec or Status. A field of a nested struct,
// such as &v.Template.Port, is constrained by that struct's own Rules.
//
// Rules returns no error. [Resource.CRD] reports every mistake in it.
func Rules[T any](v *T, fields ...FieldRule) TypeRules {
	r := TypeRules{typ: reflect.TypeFor[T]()}
	if v == nil {
		r.errs = append(r.errs, "Rules is given a nil value, which no Field can point into")
		return r
	}
	base := reflect.ValueOf(v).Pointer()
	var written []field
	if r.typ.Kind() == reflect.Struct {
		written = jsonFields(r.typ)
	}

	named := map[string]int{}
	for i, fr := range fields {
		if fr == nil {
			r.errs = append(r.errs, fmt.Sprintf("fields[%d] is nil", i))
			continue
		}
		rule := fr.fieldRule()
		if rule.p == 0 {
			r.errs = append(r.errs, fmt.Sprintf("fields[%d] points to nothing: it is Field(nil), or the zero FieldOf", i))
			continue
		}
		f, msg := locate(r.typ, written, base, rule)
		if msg != "" {
			r.errs = append(r.errs, fmt.Sprintf("fields[%d], a Field of type %s, %s", i, rule.typ, msg))
			continue
		}
		key := indexKey(f.index)
		if j, ok := named[key]; ok {
			r.errs = append(r.errs, fmt.Sprintf("fields[%d] names the field %s, as fields[%d] does", i, f.name, j))
			continue
		}
		named[key] = i
		r.fields = append(r.fields, fieldRules{pos: i, f: f, cs: rule.cs})
	}
	return r
}

// Schema gives T's schema whole, in place of the one CRD would reflect,
// wherever T appears. It is for a type that encodes itself, with a
// MarshalJSON or MarshalText method, whose schema can't be reflected from
// its fields.
func Schema[T any](s apiextensionsv1.JSONSchemaProps) TypeRules {
	return TypeRules{typ: reflect.TypeFor[T](), schema: s.DeepCopy()}
}

// Description describes the type itself, wherever it appears. A field's
// own Description replaces it there.
func (r TypeRules) Description(s string) TypeRules {
	r.description = s
	return r
}

// Validation constrains the type itself with a CEL rule, which the API
// server checks wherever the type appears, such as
// "self.minReplicas <= self.maxReplicas". The API server answers an object
// that breaks the rule with message.
func (r TypeRules) Validation(rule, message string) TypeRules {
	r.validations = append(slices.Clip(r.validations), apiextensionsv1.ValidationRule{Rule: rule, Message: message})
	return r
}

// FieldRule is a [FieldOf] of any type, as [Rules] takes it.
type FieldRule interface {
	fieldRule() fieldRule
}

// fieldRule is a FieldOf without its type parameter.
type fieldRule struct {
	p   uintptr // the address the Field points to
	typ reflect.Type
	cs  []constraint
}

// FieldOf is a field of type T, and its constraints. Each method returns a
// copy with one more constraint, so a FieldOf can be shared. A constraint
// that doesn't fit the field's type, such as Pattern on a number, is an
// error from [Resource.CRD]. Of two constraints of one kind, the later
// wins.
type FieldOf[T any] struct {
	p  *T
	cs []constraint
}

// Field names the field p points to, for [Rules].
func Field[T any](p *T) FieldOf[T] {
	return FieldOf[T]{p: p}
}

func (f FieldOf[T]) fieldRule() fieldRule {
	var p uintptr
	if f.p != nil {
		p = reflect.ValueOf(f.p).Pointer()
	}
	return fieldRule{p: p, typ: reflect.TypeFor[T](), cs: f.cs}
}

func (f FieldOf[T]) with(c constraint) FieldOf[T] {
	f.cs = append(slices.Clip(f.cs), c)
	return f
}

// Description describes the field, as kubectl explain shows it.
func (f FieldOf[T]) Description(s string) FieldOf[T] {
	return f.with(constraint{kind: "Description", s: s})
}

// Default is the value the API server fills the field with when an object
// leaves it out. A field with a Default is optional.
func (f FieldOf[T]) Default(v T) FieldOf[T] {
	return f.with(constraint{kind: "Default", values: []any{v}})
}

// Enum limits the field to values.
func (f FieldOf[T]) Enum(values ...T) FieldOf[T] {
	vs := make([]any, 0, len(values))
	for _, v := range values {
		vs = append(vs, v)
	}
	return f.with(constraint{kind: "Enum", values: vs})
}

// Minimum is the least a number may be.
func (f FieldOf[T]) Minimum(v T) FieldOf[T] {
	return f.with(constraint{kind: "Minimum", values: []any{v}})
}

// Maximum is the most a number may be.
func (f FieldOf[T]) Maximum(v T) FieldOf[T] {
	return f.with(constraint{kind: "Maximum", values: []any{v}})
}

// MinLength is the fewest characters a string may have.
func (f FieldOf[T]) MinLength(n int) FieldOf[T] {
	return f.with(constraint{kind: "MinLength", n: n})
}

// MaxLength is the most characters a string may have.
func (f FieldOf[T]) MaxLength(n int) FieldOf[T] {
	return f.with(constraint{kind: "MaxLength", n: n})
}

// Pattern is a regular expression a string must match. The API server
// matches it as an ECMA 262 regular expression, so it is best kept to the
// syntax the two share.
func (f FieldOf[T]) Pattern(re *regexp.Regexp) FieldOf[T] {
	return f.with(constraint{kind: "Pattern", re: re})
}

// MinItems is the fewest items a list may have.
func (f FieldOf[T]) MinItems(n int) FieldOf[T] {
	return f.with(constraint{kind: "MinItems", n: n})
}

// MaxItems is the most items a list may have.
func (f FieldOf[T]) MaxItems(n int) FieldOf[T] {
	return f.with(constraint{kind: "MaxItems", n: n})
}

// ListSet makes a list a set: its items are scalars, each at most once, and
// server-side apply merges two of its lists item by item.
func (f FieldOf[T]) ListSet() FieldOf[T] {
	return f.with(constraint{kind: "ListSet"})
}

// ListMap makes a list a map: its items are objects, each named by the
// values of keys, which are fields of the items. Server-side apply merges
// two of its lists by those names, as it merges a Deployment's containers.
func (f FieldOf[T]) ListMap(keys ...string) FieldOf[T] {
	return f.with(constraint{kind: "ListMap", keys: slices.Clone(keys)})
}

// Validation constrains the field with a CEL rule, which the API server
// checks, such as "self.startsWith('memcached:')". The API server answers an
// object that breaks the rule with message.
func (f FieldOf[T]) Validation(rule, message string) FieldOf[T] {
	return f.with(constraint{kind: "Validation", s: rule, message: message})
}

// constraint is one constraint of a FieldOf: its kind, which is the
// method that made it, and the method's arguments.
type constraint struct {
	kind    string
	values  []any // Default's, Enum's, Minimum's and Maximum's
	n       int   // the lengths' and the counts'
	re      *regexp.Regexp
	s       string // Description's, and Validation's rule
	message string // Validation's
	keys    []string
}

// locate returns the field of t that p points to, when base is the
// address of a t. It returns why not when p names no field encoding/json
// writes of t, as one of its own or one promoted from an embedded struct.
func locate(t reflect.Type, written []field, base uintptr, rule fieldRule) (field, string) {
	const noField = "names no field of the value given to Rules"
	if rule.p < base || rule.p-base >= t.Size() {
		return field{}, noField
	}
	steps, ok := find(t, rule.p-base, rule.typ)
	if !ok {
		return field{}, noField
	}

	last := steps[len(steps)-1]
	if last.owner.Kind() != reflect.Struct {
		return field{}, "names an item of an array, not a field"
	}
	name := last.owner.Field(last.i).Name
	index := make([]int, 0, len(steps))
	for _, s := range steps[:len(steps)-1] {
		if s.owner.Kind() != reflect.Struct || !s.owner.Field(s.i).Anonymous {
			return field{}, fmt.Sprintf("names %s, a field of %s, which only Rules of %s constrain", name, last.owner, last.owner)
		}
		index = append(index, s.i)
	}
	index = append(index, last.i)

	i := slices.IndexFunc(written, func(f field) bool { return slices.Equal(f.index, index) })
	if i < 0 {
		return field{}, fmt.Sprintf("names %s, a field encoding/json doesn't write", name)
	}
	return written[i], ""
}

// step is one step from a value to a part of it: the field i of a struct,
// or the item i of an array.
type step struct {
	owner reflect.Type
	i     int
}

// find returns the steps from a t to the value of type want at offset off
// within it, through t's fields and items, and not through pointers, which
// lead out of it.
func find(t reflect.Type, off uintptr, want reflect.Type) ([]step, bool) {
	switch t.Kind() {
	case reflect.Struct:
		for i := range t.NumField() {
			sf := t.Field(i)
			inside := off >= sf.Offset && off < sf.Offset+sf.Type.Size()
			if !inside && (sf.Type.Size() != 0 || off != sf.Offset) {
				continue
			}
			if off == sf.Offset && sf.Type == want {
				return []step{{owner: t, i: i}}, true
			}
			if rest, ok := find(sf.Type, off-sf.Offset, want); ok {
				return append([]step{{owner: t, i: i}}, rest...), true
			}
		}
	case reflect.Array:
		size := t.Elem().Size()
		if size == 0 || t.Len() == 0 {
			return nil, false
		}
		i := off / size
		at := off - i*size
		if at == 0 && t.Elem() == want {
			return []step{{owner: t, i: int(i)}}, true
		}
		if rest, ok := find(t.Elem(), at, want); ok {
			return append([]step{{owner: t, i: int(i)}}, rest...), true
		}
	}
	return nil, false
}

// indexKey is a key of a field's index.
func indexKey(index []int) string {
	var b strings.Builder
	for i, n := range index {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}

// typeRules are a TypeRules as CRD applies them: each field's constraints
// already a schema of their own.
type typeRules struct {
	schema      *apiextensionsv1.JSONSchemaProps
	description string
	validations []apiextensionsv1.ValidationRule

	// fields holds each constrained field's constraints, by its index.
	fields map[string]fieldSchema
}

// fieldSchema is the constraints of one field, as schema properties.
type fieldSchema struct {
	props     apiextensionsv1.JSONSchemaProps
	defaulted bool
}

// decorate gives s the type's own description and validations.
func (r *typeRules) decorate(s *apiextensionsv1.JSONSchemaProps) {
	if r.description != "" {
		s.Description = r.description
	}
	if len(r.validations) > 0 {
		s.XValidations = append(slices.Clone(s.XValidations), r.validations...)
	}
}

// apply gives p, a field's schema, the field's constraints.
func (f fieldSchema) apply(p *apiextensionsv1.JSONSchemaProps) {
	c := f.props
	if c.Description != "" {
		p.Description = c.Description
	}
	if c.Default != nil {
		p.Default = c.Default
	}
	if c.Enum != nil {
		p.Enum = c.Enum
	}
	if c.Minimum != nil {
		p.Minimum = c.Minimum
	}
	if c.Maximum != nil {
		p.Maximum = c.Maximum
	}
	if c.MinLength != nil {
		p.MinLength = c.MinLength
	}
	if c.MaxLength != nil {
		p.MaxLength = c.MaxLength
	}
	if c.Pattern != "" {
		p.Pattern = c.Pattern
	}
	if c.MinItems != nil {
		p.MinItems = c.MinItems
	}
	if c.MaxItems != nil {
		p.MaxItems = c.MaxItems
	}
	if c.XListType != nil {
		p.XListType = c.XListType
		p.XListMapKeys = c.XListMapKeys
	}
	if len(c.XValidations) > 0 {
		p.XValidations = append(slices.Clone(p.XValidations), c.XValidations...)
	}
}

// compileRules returns rules by the type each constrains, and every
// mistake in them, each naming its place in Resource.Rules.
func compileRules(rules []TypeRules) (map[reflect.Type]*typeRules, []error) {
	var errs []error
	byType := map[reflect.Type]*typeRules{}
	first := map[reflect.Type]int{}
	for i, r := range rules {
		if r.typ == nil {
			errs = append(errs, fmt.Errorf("Resource.Rules[%d] is the zero TypeRules", i))
			continue
		}
		if j, ok := first[r.typ]; ok {
			errs = append(errs, fmt.Errorf("Resource.Rules[%d] constrains %s, as Resource.Rules[%d] does", i, r.typ, j))
			continue
		}
		first[r.typ] = i
		byType[r.typ] = &typeRules{
			schema:      r.schema,
			description: r.description,
			validations: r.validations,
		}
	}

	for i, r := range rules {
		if r.typ == nil || first[r.typ] != i {
			continue
		}
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("Resource.Rules[%d], of %s: %s", i, r.typ, fmt.Sprintf(format, args...)))
		}
		for _, msg := range r.errs {
			fail("%s", msg)
		}
		for _, v := range r.validations {
			if v.Rule == "" {
				fail("Validation has an empty rule")
			}
		}
		if len(r.fields) > 0 && (known[r.typ] != nil || marshals(r.typ)) {
			fail("%s encodes itself, so its fields aren't its schema: give it a Schema", r.typ)
			continue
		}

		tr := byType[r.typ]
		tr.fields = map[string]fieldSchema{}
		for _, fr := range r.fields {
			fs, msgs := compileField(fr.f, fr.cs, byType)
			for _, msg := range msgs {
				fail("the field %s: %s", fr.f.name, msg)
			}
			tr.fields[indexKey(fr.f.index)] = fs
		}
	}
	return byType, errs
}

// compileField returns the schema properties of f's constraints, and why
// any of them doesn't fit f.
func compileField(f field, cs []constraint, byType map[reflect.Type]*typeRules) (fieldSchema, []string) {
	var (
		fs   fieldSchema
		msgs []string
	)
	typ, typed := schemaType(f.typ, f.quoted, byType)
	fits := func(c constraint, want ...string) bool {
		if !typed || slices.Contains(want, typ) {
			return true
		}
		msgs = append(msgs, fmt.Sprintf("%s is for %s, but the field is %s", c.kind, describe(want[0]), describe(typ)))
		return false
	}
	encode := func(c constraint, v any) (apiextensionsv1.JSON, bool) {
		b, err := json.Marshal(v)
		if err == nil && f.quoted && string(b) != "null" {
			// The string option writes the value's encoding as a string.
			b, err = json.Marshal(string(b))
		}
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("%s's value doesn't encode: %s", c.kind, err))
			return apiextensionsv1.JSON{}, false
		}
		return apiextensionsv1.JSON{Raw: b}, true
	}
	number := func(c constraint) *float64 {
		if !fits(c, "number", "integer") {
			return nil
		}
		v, ok := encode(c, c.values[0])
		if !ok {
			return nil
		}
		var n float64
		if err := json.Unmarshal(v.Raw, &n); err != nil {
			msgs = append(msgs, fmt.Sprintf("%s's value encodes as %s, not a number", c.kind, v.Raw))
			return nil
		}
		return &n
	}
	count := func(c constraint, want string) *int64 {
		if !fits(c, want) {
			return nil
		}
		if c.n < 0 {
			msgs = append(msgs, fmt.Sprintf("%s is %d, which is negative", c.kind, c.n))
			return nil
		}
		n := int64(c.n)
		return &n
	}

	p := &fs.props
	for _, c := range cs {
		switch c.kind {
		case "Description":
			p.Description = c.s
		case "Default":
			if v, ok := encode(c, c.values[0]); ok {
				p.Default = &v
				fs.defaulted = true
			}
		case "Enum":
			if len(c.values) == 0 {
				msgs = append(msgs, "Enum has no value")
				continue
			}
			enum := make([]apiextensionsv1.JSON, 0, len(c.values))
			for _, v := range c.values {
				if j, ok := encode(c, v); ok {
					enum = append(enum, j)
				}
			}
			p.Enum = enum
		case "Minimum":
			if n := number(c); n != nil {
				p.Minimum = n
			}
		case "Maximum":
			if n := number(c); n != nil {
				p.Maximum = n
			}
		case "MinLength":
			if n := count(c, "string"); n != nil {
				p.MinLength = n
			}
		case "MaxLength":
			if n := count(c, "string"); n != nil {
				p.MaxLength = n
			}
		case "Pattern":
			if !fits(c, "string") {
				continue
			}
			if c.re == nil {
				msgs = append(msgs, "Pattern is given a nil regular expression")
				continue
			}
			p.Pattern = c.re.String()
		case "MinItems":
			if n := count(c, "array"); n != nil {
				p.MinItems = n
			}
		case "MaxItems":
			if n := count(c, "array"); n != nil {
				p.MaxItems = n
			}
		case "ListSet":
			if fits(c, "array") {
				set := "set"
				p.XListType, p.XListMapKeys = &set, nil
			}
		case "ListMap":
			if !fits(c, "array") {
				continue
			}
			if len(c.keys) == 0 {
				msgs = append(msgs, "ListMap has no key")
				continue
			}
			if names, ok := itemFields(f.typ, byType); ok {
				for _, k := range c.keys {
					if !slices.Contains(names, k) {
						msgs = append(msgs, fmt.Sprintf("ListMap's key %q is no field of the list's items", k))
					}
				}
			}
			m := "map"
			p.XListType, p.XListMapKeys = &m, c.keys
		case "Validation":
			if c.s == "" {
				msgs = append(msgs, "Validation has an empty rule")
				continue
			}
			p.XValidations = append(p.XValidations, apiextensionsv1.ValidationRule{Rule: c.s, Message: c.message})
		}
	}
	return fs, msgs
}

// schemaType returns the type of the schema CRD reflects for t, and false
// when it can't tell, since t's schema is itself a mistake CRD reports.
func schemaType(t reflect.Type, quoted bool, byType map[reflect.Type]*typeRules) (string, bool) {
	if quoted {
		return "string", true
	}
	for t.Kind() == reflect.Pointer {
		if r := byType[t]; r != nil && r.schema != nil {
			return r.schema.Type, true
		}
		t = t.Elem()
	}
	if r := byType[t]; r != nil && r.schema != nil {
		return r.schema.Type, true
	}
	if s, ok := known[t]; ok {
		if s := s(); s.XIntOrString {
			return "int-or-string", true
		}
		return s().Type, true
	}
	if marshals(t) {
		return "", false
	}
	switch t.Kind() {
	case reflect.Bool:
		return "boolean", true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return "integer", true
	case reflect.Float32, reflect.Float64:
		return "number", true
	case reflect.String:
		return "string", true
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 && !marshals(t.Elem()) {
			return "string", true
		}
		return "array", true
	case reflect.Array:
		return "array", true
	case reflect.Map, reflect.Struct:
		return "object", true
	}
	return "", false
}

// itemFields returns the names of the fields of the items of the list t,
// and false when they aren't a struct whose schema is reflected.
func itemFields(t reflect.Type, byType map[reflect.Type]*typeRules) ([]string, bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
		return nil, false
	}
	item := t.Elem()
	for item.Kind() == reflect.Pointer {
		item = item.Elem()
	}
	if r := byType[item]; r != nil && r.schema != nil {
		return slices.Collect(maps.Keys(r.schema.Properties)), true
	}
	if item.Kind() != reflect.Struct || known[item] != nil || marshals(item) {
		return nil, false
	}
	var names []string
	for _, f := range jsonFields(item) {
		names = append(names, f.name)
	}
	return names, true
}

// describe names a schema's type in an error, with its article.
func describe(typ string) string {
	switch typ {
	case "integer", "array", "object":
		return "an " + typ
	case "int-or-string":
		return "an int or a string"
	case "":
		return "of no one type"
	}
	return "a " + typ
}
