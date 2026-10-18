// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package jsoncodec reads and writes JSON bodies for openapi's operations.
//
// Decoder and Encoder are T's codecs as application/json. Problem is T's
// encoder as application/problem+json, for RFC 9457 problem details. Pass
// them uninstantiated, and the compiler infers T from the handler:
//
//	openapi.Post(items, c.addCartItem, jsoncodec.Decoder, jsoncodec.Encoder,
//		openapi.Status(http.StatusCreated),
//		openapi.Error[NotFound](http.StatusNotFound, jsoncodec.Problem),
//	)
//
// jsoncodec handles the details that are easy to get wrong:
//   - It decodes one JSON value, and an empty body, malformed JSON, a value
//     of the wrong type and data after the value are errors, which the API
//     answers with a 400.
//   - Unknown fields are ignored.
//   - An error from http.MaxBytesHandler's reader stays wrapped, so the API
//     answers it with a 413.
//   - Its Schema rejects a T that JSON can't decode into or encode, such as
//     a channel, a function or an io.Reader, so the mistake is a NewAPI
//     error rather than every request's.
//   - It encodes into a buffer, so a value that can't be encoded is
//     answered by the catch-all, and the response has a Content-Length.
//
// jsoncodec uses only openapi's exported API, as a third party's codec
// would, and openapi never uses it.
package jsoncodec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"

	openapi "github.com/z5labs/humus/openapi/v3"
)

// Decoder is T's decoder as application/json.
func Decoder[T any]() openapi.Decoder[T] { return codec[T]{mediaType: "application/json"} }

// Encoder is T's encoder as application/json.
func Encoder[T any]() openapi.Encoder[T] { return codec[T]{mediaType: "application/json"} }

// Problem is T's encoder as application/problem+json, for an error body
// that is an RFC 9457 problem detail.
func Problem[T any]() openapi.Encoder[T] { return codec[T]{mediaType: "application/problem+json"} }

// codec reads and writes T as JSON, in mediaType.
type codec[T any] struct {
	mediaType string
}

func (c codec[T]) MediaType() string { return c.mediaType }

// Schema returns an error naming T for a T that JSON can't decode into or
// encode: an interface, io.Reader included, a channel, a function, a
// complex number or an unsafe pointer, or a pointer to one. It returns
// T's schema, reflected by its json tags, for any other T.
func (codec[T]) Schema() (openapi.Schema, error) {
	t := reflect.TypeFor[T]()
	k := t
	for k.Kind() == reflect.Pointer {
		k = k.Elem()
	}
	switch k.Kind() {
	case reflect.Interface, reflect.Chan, reflect.Func, reflect.Complex64, reflect.Complex128, reflect.UnsafePointer:
		return nil, fmt.Errorf("jsoncodec: JSON can't decode into or encode the type %s", t)
	}
	return openapi.SchemaOf[T]("json")
}

// Decode reads one JSON value from r's body, and checks that nothing but
// whitespace follows it. Unknown fields are ignored.
func (codec[T]) Decode(r *http.Request) (T, error) {
	var v T
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&v); err != nil {
		if errors.Is(err, io.EOF) {
			return v, errors.New("the body is empty")
		}
		return v, err
	}
	_, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return v, nil
	}
	if _, ok := errors.AsType[*json.SyntaxError](err); err == nil || ok {
		return v, errors.New("data after the JSON value")
	}
	// The body failed to read, such as past http.MaxBytesHandler's limit.
	return v, err
}

// Encode marshals v into a buffer, before the status is written, and
// returns it as a *bytes.Reader, whose length is the Content-Length.
func (codec[T]) Encode(v T) (io.WriterTo, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("jsoncodec: %w", err)
	}
	return bytes.NewReader(b), nil
}
