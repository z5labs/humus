// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/z5labs/humus/internal/ptr"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go/openapi3"
	"go.opentelemetry.io/otel"
)

// JsonConsumerHandler
type JsonConsumerHandler[Req, Resp any] struct {
	inner Handler[Req, Resp]
}

// ConsumesJson
func ConsumesJson[Req, Resp any](h Handler[Req, Resp]) *JsonConsumerHandler[Req, Resp] {
	return &JsonConsumerHandler[Req, Resp]{
		inner: h,
	}
}

// JsonRequest
type JsonRequest[T any] struct {
	t T
}

// Type implements the [TypedRequest] interface.
func (jr *JsonRequest[T]) Type() (*openapi3.RequestBody, error) {
	var reflector jsonschema.Reflector
	var t T

	jsonSchema, err := reflector.Reflect(t)
	if err != nil {
		return nil, err
	}

	var schemaOrRef openapi3.SchemaOrRef
	schemaOrRef.FromJSONSchema(jsonSchema.ToSchemaOrBool())

	return &openapi3.RequestBody{
		Required: ptr.Ref(true),
		Content: map[string]openapi3.MediaType{
			"application/json": {
				Schema: &schemaOrRef,
			},
		},
	}, nil
}

// ReadRequest implements the [RequestReader] interface.
func (jr *JsonRequest[T]) ReadRequest(ctx context.Context, r *http.Request) (err error) {
	_, span := otel.Tracer("endpoint").Start(ctx, "JsonRequest.ReadRequest")
	defer span.End()
	defer joinClose(&err, r.Body)

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		return InvalidContentTypeError{
			ContentType: contentType,
		}
	}

	dec := json.NewDecoder(r.Body)
	err = dec.Decode(&jr.t)
	return
}

// Handle implements the [Handler] interface.
func (h *JsonConsumerHandler[Req, Resp]) Handle(ctx context.Context, req *JsonRequest[Req]) (*Resp, error) {
	return h.inner.Handle(ctx, &req.t)
}

// JsonProducerHandler
type JsonProducerHandler[Req, Resp any] struct {
	inner Handler[Req, Resp]
}

// ProducesJson
func ProducesJson[Req, Resp any](h Handler[Req, Resp]) *JsonProducerHandler[Req, Resp] {
	return &JsonProducerHandler[Req, Resp]{
		inner: h,
	}
}

// JsonResponse
type JsonResponse[T any] struct {
	t *T
}

// Type implements the [TypedResponse] interface.
func (jr *JsonResponse[T]) Type() (int, *openapi3.Response, error) {
	var reflector jsonschema.Reflector
	var t T

	jsonSchema, err := reflector.Reflect(t)
	if err != nil {
		return 0, nil, err
	}

	var schemaOrRef openapi3.SchemaOrRef
	schemaOrRef.FromJSONSchema(jsonSchema.ToSchemaOrBool())

	resp := &openapi3.Response{
		Content: map[string]openapi3.MediaType{
			"application/json": {
				Schema: &schemaOrRef,
			},
		},
	}
	return http.StatusOK, resp, nil
}

// WriteResponse implements the [ResponseWriter] interface.
func (jr *JsonResponse[T]) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	_, span := otel.Tracer("endpoint").Start(ctx, "JsonResponse.WriteResponse")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	return enc.Encode(jr.t)
}

// Handle implements the [Handler] interface.
func (h *JsonProducerHandler[Req, Resp]) Handle(ctx context.Context, req *Req) (*JsonResponse[Resp], error) {
	resp, err := h.inner.Handle(ctx, req)
	if err != nil {
		return nil, err
	}
	jr := &JsonResponse[Resp]{
		t: resp,
	}
	return jr, nil
}
