// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/humus/rest"
	"github.com/z5labs/sdk-go/ptr"
	"github.com/z5labs/sdk-go/try"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// ReturnJsonHandler
type ReturnJsonHandler[Req, Resp any] struct {
	inner  Handler[Req, Resp]
	tracer trace.Tracer
}

// ReturnJson initializes a [ReturnJsonHandler].
func ReturnJson[Req, Resp any](h Handler[Req, Resp]) *ReturnJsonHandler[Req, Resp] {
	return &ReturnJsonHandler[Req, Resp]{
		inner:  h,
		tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
	}
}

// JsonResponse
type JsonResponse[T any] struct {
	inner  *T
	tracer trace.Tracer
}

// Spec implements the [TypedResponse] interface.
func (*JsonResponse[T]) Spec() (int, *openapi3.Response, error) {
	var t T
	var reflector jsonschema.Reflector

	jsonSchema, err := reflector.Reflect(t, jsonschema.InlineRefs)
	if err != nil {
		return 0, nil, err
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
	return http.StatusOK, spec, nil
}

// WriteResponse implements the [ResponseWriter] interface.
func (jr *JsonResponse[T]) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	_, span := jr.tracer.Start(ctx, "JsonResponse.WriteResponse")
	defer span.End()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	return enc.Encode(jr.inner)
}

// Handle implements the [Handler] interface.
func (h *ReturnJsonHandler[Req, Resp]) Handle(ctx context.Context, req *Req) (*JsonResponse[Resp], error) {
	spanCtx, span := h.tracer.Start(ctx, "ReturnJsonHandler.Handle")
	defer span.End()

	resp, err := h.inner.Handle(spanCtx, req)
	if err != nil {
		return nil, err
	}
	return &JsonResponse[Resp]{
		inner:  resp,
		tracer: h.tracer,
	}, nil
}

// RequestBody implements the rest.Handler interface.
func (h *ReturnJsonHandler[Req, Resp]) RequestBody() openapi3.RequestBodyOrRef {
	return openapi3.RequestBodyOrRef{}
}

// Responses implements the rest.Handler interface.
func (h *ReturnJsonHandler[Req, Resp]) Responses() openapi3.Responses {
	var resp JsonResponse[Resp]
	statusCode, responseDef, err := resp.Spec()
	if err != nil {
		// Return empty responses if spec generation fails
		return openapi3.Responses{}
	}

	return openapi3.Responses{
		MapOfResponseOrRefValues: map[string]openapi3.ResponseOrRef{
			strconv.Itoa(statusCode): {
				Response: responseDef,
			},
		},
	}
}

// ServeHTTP implements the [http.Handler] interface.
func (h *ReturnJsonHandler[Req, Resp]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spanCtx, span := h.tracer.Start(r.Context(), "ReturnJsonHandler.ServeHTTP")
	defer span.End()

	var req Req
	resp, err := h.Handle(spanCtx, &req)
	if err != nil {
		span.RecordError(err)
		panic(err)
	}

	err = resp.WriteResponse(spanCtx, w)
	if err != nil {
		span.RecordError(err)
		panic(err)
	}
}

// ConsumeJsonHandler
type ConsumeJsonHandler[Req, Resp any] struct {
	inner  Handler[Req, Resp]
	tracer trace.Tracer
}

// ConsumeJson initializes a [ConsumeJsonHandler].
func ConsumeJson[Req, Resp any](h Handler[Req, Resp]) *ConsumeJsonHandler[Req, Resp] {
	return &ConsumeJsonHandler[Req, Resp]{
		inner:  h,
		tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
	}
}

// JsonRequest
type JsonRequest[T any] struct {
	inner  T
	tracer trace.Tracer
}

// Spec implements the [TypedRequest] interface.
func (*JsonRequest[T]) Spec() (*openapi3.RequestBody, error) {
	var t T
	var reflector jsonschema.Reflector

	jsonSchema, err := reflector.Reflect(t, jsonschema.InlineRefs)
	if err != nil {
		return nil, err
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
	return spec, nil
}

// ReadRequest implements the [RequestReader] interface.
func (jr *JsonRequest[T]) ReadRequest(ctx context.Context, r *http.Request) (err error) {
	_, span := jr.tracer.Start(ctx, "JsonRequest.ReadRequest")
	defer span.End()
	defer try.Close(&err, r.Body)

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		return rest.BadRequestError{
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
	spanCtx, span := h.tracer.Start(ctx, "ConsumeJsonHandler.Handle")
	defer span.End()

	return h.inner.Handle(spanCtx, &req.inner)
}

// RequestBody implements the rest.Handler interface.
func (h *ConsumeJsonHandler[Req, Resp]) RequestBody() openapi3.RequestBodyOrRef {
	var req JsonRequest[Req]
	reqBody, err := req.Spec()
	if err != nil {
		// Return empty request body if spec generation fails
		return openapi3.RequestBodyOrRef{}
	}

	return openapi3.RequestBodyOrRef{
		RequestBody: reqBody,
	}
}

// Responses implements the rest.Handler interface.
func (h *ConsumeJsonHandler[Req, Resp]) Responses() openapi3.Responses {
	var resp Resp
	// Check if the response type implements TypedResponse interface
	if typedResp, ok := any(&resp).(interface {
		Spec() (int, *openapi3.Response, error)
	}); ok {
		statusCode, respSpec, err := typedResp.Spec()
		if err != nil {
			return openapi3.Responses{}
		}
		return openapi3.Responses{
			MapOfResponseOrRefValues: map[string]openapi3.ResponseOrRef{
				strconv.Itoa(statusCode): {Response: respSpec},
			},
		}
	}
	return openapi3.Responses{}
}

// ServeHTTP implements the [http.Handler] interface.
func (h *ConsumeJsonHandler[Req, Resp]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spanCtx, span := h.tracer.Start(r.Context(), "ConsumeJsonHandler.ServeHTTP")
	defer span.End()

	var req JsonRequest[Req]
	req.tracer = h.tracer
	err := req.ReadRequest(spanCtx, r)
	if err != nil {
		span.RecordError(err)
		panic(err)
	}

	resp, err := h.Handle(spanCtx, &req)
	if err != nil {
		span.RecordError(err)
		panic(err)
	}

	// Check if the response type implements ResponseWriter interface
	if writer, ok := any(resp).(interface {
		WriteResponse(context.Context, http.ResponseWriter) error
	}); ok {
		err = writer.WriteResponse(spanCtx, w)
		if err != nil {
			span.RecordError(err)
			panic(err)
		}
	}
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
func ProduceJson[T any](p Producer[T]) *ReturnJsonHandler[emptyRequest, T] {
	inner := &producerHandler[T]{
		p:      p,
		tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
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
func ConsumeOnlyJson[T any](c Consumer[T]) *ConsumeJsonHandler[T, emptyResponse] {
	inner := &consumerHandler[T]{
		c:      c,
		tracer: otel.Tracer("github.com/z5labs/humus/rest/rpc"),
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
