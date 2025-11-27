// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/sdk-go/ptr"
	"github.com/z5labs/sdk-go/try"
)

// ReturnJsonHandler
type ReturnJsonHandler[Req, Resp any] struct {
	inner Handler[Req, Resp]
}

// ReturnJson initializes a [ReturnJsonHandler].
func ReturnJson[Req, Resp any](h Handler[Req, Resp]) *ReturnJsonHandler[Req, Resp] {
	return &ReturnJsonHandler[Req, Resp]{
		inner: h,
	}
}

// JsonResponse
type JsonResponse[T any] struct {
	inner *T
}

// Spec implements the [TypedResponse] interface.
func (*JsonResponse[T]) Spec() (int, openapi3.ResponseOrRef, error) {
	var t T
	var reflector jsonschema.Reflector

	jsonSchema, err := reflector.Reflect(t, jsonschema.InlineRefs)
	if err != nil {
		return 0, openapi3.ResponseOrRef{}, err
	}

	var schemaOrRef openapi3.SchemaOrRef
	schemaOrRef.FromJSONSchema(jsonSchema.ToSchemaOrBool())

	spec := &openapi3.Response{
		Content: map[string]openapi3.MediaType{
			"application/json": {
				Schema: &schemaOrRef,
			},
		},
	}

	return http.StatusOK, openapi3.ResponseOrRef{
		Response: spec,
	}, nil
}

// WriteResponse implements the [ResponseWriter] interface.
func (jr *JsonResponse[T]) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	return enc.Encode(jr.inner)
}

// Handle implements the [Handler] interface.
func (h *ReturnJsonHandler[Req, Resp]) Handle(ctx context.Context, req *Req) (*JsonResponse[Resp], error) {
	resp, err := h.inner.Handle(ctx, req)
	if err != nil {
		return nil, err
	}
	return &JsonResponse[Resp]{
		inner: resp,
	}, nil
}

// ConsumeJsonHandler
type ConsumeJsonHandler[Req, Resp any] struct {
	inner Handler[Req, Resp]
}

// ConsumeJson initializes a [ConsumeJsonHandler].
func ConsumeJson[Req, Resp any](h Handler[Req, Resp]) *ConsumeJsonHandler[Req, Resp] {
	return &ConsumeJsonHandler[Req, Resp]{
		inner: h,
	}
}

// JsonRequest
type JsonRequest[T any] struct {
	inner T
}

// Spec implements the [TypedRequest] interface.
func (*JsonRequest[T]) Spec() (openapi3.RequestBodyOrRef, error) {
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
			"application/json": {
				Schema: &schemaOrRef,
			},
		},
	}

	return openapi3.RequestBodyOrRef{
		RequestBody: spec,
	}, nil
}

// ReadRequest implements the [RequestReader] interface.
func (jr *JsonRequest[T]) ReadRequest(ctx context.Context, r *http.Request) (err error) {
	defer try.Close(&err, r.Body)

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		return BadRequestError{
			Cause: InvalidContentTypeError{
				ContentType: contentType,
			},
		}
	}

	dec := json.NewDecoder(r.Body)
	return dec.Decode(&jr.inner)
}

// Handle implements the [Handler] interface.
func (h *ConsumeJsonHandler[Req, Resp]) Handle(ctx context.Context, req *JsonRequest[Req]) (*Resp, error) {
	return h.inner.Handle(ctx, &req.inner)
}

// ProduceJson creates a handler that returns JSON responses without consuming a request body.
// Use this for GET endpoints that return data.
//
// Example:
//
//	p := rpc.ProducerFunc[Response](func(ctx context.Context) (*Response, error) {
//	    return &Response{Message: "hello"}, nil
//	})
//	handler := rpc.ProduceJson(p)
func ProduceJson[T any](p Producer[T]) *ReturnJsonHandler[EmptyRequest, T] {
	inner := &ProducerHandler[T]{
		p: p,
	}
	return ReturnJson(inner)
}

// ConsumeOnlyJson creates a handler that consumes JSON requests without returning a response body.
// Use this for webhook-style POST/PUT endpoints that process data but don't return content.
//
// Example:
//
//	c := rpc.ConsumerFunc[Request](func(ctx context.Context, req *Request) error {
//	    // process request
//	    return nil
//	})
//	handler := rpc.ConsumeOnlyJson(c)
func ConsumeOnlyJson[T any](c Consumer[T]) *ConsumeJsonHandler[T, EmptyResponse] {
	inner := &ConsumerHandler[T]{
		c: c,
	}
	return ConsumeJson(inner)
}

// HandleJson creates a handler that both consumes and produces JSON.
// Use this for POST/PUT endpoints with request and response bodies.
//
// Example:
//
//	h := rpc.HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) {
//	    return &Response{Message: req.Message}, nil
//	})
//	handler := rpc.HandleJson(h)
func HandleJson[Req, Resp any](h Handler[Req, Resp]) *ConsumeJsonHandler[Req, JsonResponse[Resp]] {
	return ConsumeJson(ReturnJson(h))
}
