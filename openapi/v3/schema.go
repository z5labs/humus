// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/swaggest/jsonschema-go"
)

// SchemaOf returns T's JSON Schema, reflected from its fields, with each
// property named after the struct tag tag, such as "json" or "xml". A codec
// whose bodies are T's fields calls it from its Schema method, and adds
// what its format needs, such as OpenAPI's xml object.
//
// The schema is reflected with swaggest/jsonschema-go:
//   - an exported field without tag is a property named after the field,
//     as encoding/json and encoding/xml name it, and a field whose tag
//     names it "-" isn't a property;
//   - refs are inlined, so the schema stands alone, as the document lists
//     it under the body's media type;
//   - int64 values have the format int64, and float64 values the format
//     double.
//
// It returns an error for an empty tag, for a T that holds itself, by way
// of its fields, at any depth, since an inlined schema can't, and for a T
// that has no JSON Schema, such as a channel or a function.
func SchemaOf[T any](tag string) (Schema, error) {
	t := reflect.TypeFor[T]()
	if tag == "" {
		return nil, fmt.Errorf("openapi: SchemaOf[%s]: the tag is empty", t)
	}
	if rec := recursiveType(t, tag, make(map[reflect.Type]bool)); rec != nil {
		return nil, fmt.Errorf("openapi: SchemaOf[%s]: %s holds itself, so its schema can't be inlined", t, rec)
	}
	// The reflector reads T from a value: T's zero value, or, for an
	// interface, which has none but nil, a pointer to one.
	var v any = *new(T)
	if t.Kind() == reflect.Interface {
		v = new(T)
	}
	var r jsonschema.Reflector
	s, err := r.Reflect(v,
		jsonschema.PropertyNameTag(tag),
		jsonschema.InlineRefs,
		jsonschema.ProcessWithoutTags,
		jsonschema.InterceptSchema(formatNumbers),
	)
	if err != nil {
		return nil, fmt.Errorf("openapi: SchemaOf[%s]: %w", t, err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("openapi: SchemaOf[%s]: %w", t, err)
	}
	return Schema(b), nil
}

// formatNumbers gives an int64's integer schema the format int64, and a
// float64's number schema the format double, as OpenAPI's data types name
// them.
func formatNumbers(p jsonschema.InterceptSchemaParams) (bool, error) {
	if !p.Processed || !p.Value.IsValid() || p.Schema.Format != nil {
		return false, nil
	}
	t := p.Value.Type()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t.Kind() == reflect.Int64 && p.Schema.HasType(jsonschema.Integer):
		p.Schema.WithFormat("int64")
	case t.Kind() == reflect.Float64 && p.Schema.HasType(jsonschema.Number):
		p.Schema.WithFormat("double")
	}
	return false, nil
}

// recursiveType returns a struct type that t holds within itself, by way of
// the fields SchemaOf reflects, or nil. onPath holds the struct types that
// hold t.
func recursiveType(t reflect.Type, tag string, onPath map[reflect.Type]bool) reflect.Type {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return recursiveType(t.Elem(), tag, onPath)
	case reflect.Struct:
		if onPath[t] {
			return t
		}
		onPath[t] = true
		defer delete(onPath, t)
		for f := range t.Fields() {
			if !f.IsExported() && !f.Anonymous {
				continue
			}
			if name, _, _ := strings.Cut(f.Tag.Get(tag), ","); name == "-" {
				continue
			}
			if rec := recursiveType(f.Type, tag, onPath); rec != nil {
				return rec
			}
		}
	}
	return nil
}
