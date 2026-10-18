// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

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

// The test's own codecs: a JSON-like codec, in this test package, using
// only openapi's exported API, as a third party's codec would. openapi
// refers to no format itself.

// jsonCodec reads and writes T as JSON, in mediaType.
type jsonCodec[T any] struct {
	mediaType string
}

// jsonDecoder is T's decoder as application/json.
func jsonDecoder[T any]() openapi.Decoder[T] { return jsonCodec[T]{mediaType: "application/json"} }

// jsonEncoder is T's encoder as application/json.
func jsonEncoder[T any]() openapi.Encoder[T] { return jsonCodec[T]{mediaType: "application/json"} }

// problemEncoder is T's encoder as application/problem+json.
func problemEncoder[T any]() openapi.Encoder[T] {
	return jsonCodec[T]{mediaType: "application/problem+json"}
}

func (c jsonCodec[T]) MediaType() string { return c.mediaType }

// Schema rejects a T that JSON can't encode, as a real codec would, so the
// mistake is a NewAPI error.
func (jsonCodec[T]) Schema() (openapi.Schema, error) {
	t := reflect.TypeFor[T]()
	switch t.Kind() {
	case reflect.Chan, reflect.Func:
		return nil, fmt.Errorf("JSON can't encode a %s", t)
	}
	return openapi.Schema(`{}`), nil
}

// Decode reads one JSON value, and nothing after it.
func (jsonCodec[T]) Decode(r *http.Request) (T, error) {
	var v T
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	if dec.More() {
		return v, errors.New("data after the JSON value")
	}
	return v, nil
}

// Encode marshals v into a buffer, so a value that can't be encoded fails
// before the status is written.
func (jsonCodec[T]) Encode(v T) (io.WriterTo, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(b), nil
}

// nilEncoder is an encoder function that returns nil.
func nilEncoder[T any]() openapi.Encoder[T] { return nil }

// problem is the test's error body, a problem detail of its own.
type problem struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Param  string `json:"param,omitempty"`
}

func (p problem) Error() string { return p.Title }

// notFound is a problem the handlers return.
type notFound struct{ problem }

// gone is another.
type gone struct{ problem }

// rejected wraps a RequestError, for RequestErrors.
func rejected(e *openapi.RequestError) problem {
	return problem{Status: e.Status, Title: http.StatusText(e.Status), Detail: e.Err.Error(), Param: e.Param}
}

// rejectedByOperation is an operation's own RequestErrors wrap, which marks
// its answers.
func rejectedByOperation(e *openapi.RequestError) problem {
	p := rejected(e)
	p.Title = "rejected by the operation"
	return p
}

// internalError wraps an error the catch-all answers. It hides the error's
// text, as a catch-all should.
func internalError(error) problem {
	return problem{Status: http.StatusInternalServerError, Title: "Internal Server Error"}
}
