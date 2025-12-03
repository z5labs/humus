// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/sdk-go/ptr"
)

// ConsumeFormHandler wraps a handler to consume form-encoded request data.
type ConsumeFormHandler[Req, Resp any] struct {
	inner Handler[Req, Resp]
}

// ConsumeForm initializes a [ConsumeFormHandler].
// It wraps the provided handler to automatically parse form data into the request type.
//
// The request type should have struct tags indicating form field names:
//
//	type MyRequest struct {
//	    Name  string `form:"name"`
//	    Email string `form:"email"`
//	    Age   int    `form:"age"`
//	}
func ConsumeForm[Req, Resp any](h Handler[Req, Resp]) *ConsumeFormHandler[Req, Resp] {
	return &ConsumeFormHandler[Req, Resp]{
		inner: h,
	}
}

// FormRequest wraps form-encoded request data.
type FormRequest[T any] struct {
	inner T
}

// Spec implements the [TypedRequest] interface.
func (*FormRequest[T]) Spec() (openapi3.RequestBodyOrRef, error) {
	var t T
	var reflector jsonschema.Reflector

	jsonSchema, err := reflector.Reflect(t, jsonschema.InlineRefs)
	if err != nil {
		return openapi3.RequestBodyOrRef{}, err
	}

	var schemaOrRef openapi3.SchemaOrRef
	schemaOrRef.FromJSONSchema(jsonSchema.ToSchemaOrBool())

	spec := &openapi3.RequestBody{
		Required: ptr.Ref(true),
		Content: map[string]openapi3.MediaType{
			"application/x-www-form-urlencoded": {
				Schema: &schemaOrRef,
			},
		},
	}

	return openapi3.RequestBodyOrRef{
		RequestBody: spec,
	}, nil
}

// ReadRequest implements the [RequestReader] interface.
func (fr *FormRequest[T]) ReadRequest(ctx context.Context, r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return BadRequestError{
			Cause: err,
		}
	}

	return decodeForm(r.Form, &fr.inner)
}

// Handle implements the [Handler] interface.
func (h *ConsumeFormHandler[Req, Resp]) Handle(ctx context.Context, req *FormRequest[Req]) (*Resp, error) {
	return h.inner.Handle(ctx, &req.inner)
}

// ConsumeOnlyForm creates a handler that consumes form requests without returning a response body.
// Use this for webhook-style POST/PUT endpoints that process data but don't return content.
//
// Example:
//
//	c := rest.ConsumerFunc[Request](func(ctx context.Context, req *Request) error {
//	    // process request
//	    return nil
//	})
//	handler := rest.ConsumeOnlyForm(c)
func ConsumeOnlyForm[T any](c Consumer[T]) *ConsumeFormHandler[T, EmptyResponse] {
	inner := &ConsumerHandler[T]{
		c: c,
	}
	return ConsumeForm(inner)
}

// HandleForm creates a handler that consumes form data and produces JSON responses.
// Use this for POST/PUT form endpoints with JSON response bodies.
//
// Example:
//
//	h := rest.HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) {
//	    return &Response{Message: req.Message}, nil
//	})
//	handler := rest.HandleForm(h)
func HandleForm[Req, Resp any](h Handler[Req, Resp]) *ConsumeFormHandler[Req, JsonResponse[Resp]] {
	return ConsumeForm(ReturnJson(h))
}

// decodeForm decodes form values into a struct using reflection.
// It looks for "form" struct tags to map form fields to struct fields.
func decodeForm(form map[string][]string, dst any) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr {
		return errors.New("dst must be a pointer")
	}

	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return errors.New("dst must be a pointer to a struct")
	}

	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		fieldType := t.Field(i)

		// Skip unexported fields
		if !field.CanSet() {
			continue
		}

		// Get form field name from tag or use field name
		formTag := fieldType.Tag.Get("form")
		if formTag == "" {
			formTag = strings.ToLower(fieldType.Name)
		}

		// Check if form value exists
		values, ok := form[formTag]
		if !ok || len(values) == 0 {
			continue
		}

		// Set the field value based on its type
		value := values[0]
		if err := setField(field, value); err != nil {
			return fmt.Errorf("failed to set field %s: %w", fieldType.Name, err)
		}
	}

	return nil
}

// setField sets a reflect.Value to a string value, converting types as needed.
// It supports:
// - encoding.TextUnmarshaler interface (highest priority)
// - time.Time and time.Duration
// - Primitive types (string, int, uint, bool, float)
// - Type aliases of primitives
func setField(field reflect.Value, value string) error {
	// First, check if the field implements encoding.TextUnmarshaler
	// We need to check both the field itself and a pointer to it
	if field.CanAddr() {
		ptrToField := field.Addr()
		if unmarshaler, ok := ptrToField.Interface().(encoding.TextUnmarshaler); ok {
			return unmarshaler.UnmarshalText([]byte(value))
		}
	}

	// Handle time.Time specially (it implements TextUnmarshaler but we want RFC3339 parsing)
	if field.Type() == reflect.TypeOf(time.Time{}) {
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(t))
		return nil
	}

	// Handle time.Duration (it's an int64 alias, but we want to parse duration strings)
	if field.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(d))
		return nil
	}

	// Fall back to kind-based type handling for primitives
	switch field.Kind() {
	case reflect.String:
		field.SetString(value)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		intVal, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		field.SetInt(intVal)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		uintVal, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return err
		}
		field.SetUint(uintVal)
	case reflect.Bool:
		boolVal, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		field.SetBool(boolVal)
	case reflect.Float32, reflect.Float64:
		floatVal, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		field.SetFloat(floatVal)
	default:
		return fmt.Errorf("unsupported field type: %s", field.Kind())
	}

	return nil
}
