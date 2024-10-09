// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/bedrock/pkg/ptr"
	"github.com/z5labs/bedrock/rest"
	"github.com/z5labs/bedrock/rest/endpoint"
	"github.com/z5labs/bedrock/rest/mux"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ContentTyper
type ContentTyper endpoint.ContentTyper

// ProtobufContentType
var ProtobufContentType = "application/x-protobuf"

// ProtoMessage embeds [proto.Message] but adds *T support.
type ProtoMessage[T any] interface {
	*T
	proto.Message
}

// Handler
type Handler[I, O any, Req ProtoMessage[I], Resp ProtoMessage[O]] endpoint.Handler[I, O]

// Endpoint
type Endpoint struct {
	method    string
	path      string
	operation rest.Operation
}

// RegisterEndpoint
func RegisterEndpoint(e Endpoint) Option {
	return func(a *App) {
		a.restOpts = append(a.restOpts, rest.Endpoint(
			mux.Method(e.method),
			e.path,
			e.operation,
		))
	}
}

type endpointOptions struct {
	eopts []endpoint.Option
}

// EndpointOption
type EndpointOption func(*endpointOptions)

// Returns
func Returns(status int) EndpointOption {
	return func(eo *endpointOptions) {
		eo.eopts = append(eo.eopts, endpoint.Returns(status))
	}
}

// Header
type Header endpoint.Header

// Headers
func Headers(hs ...Header) EndpointOption {
	return func(eo *endpointOptions) {
		headers := make([]endpoint.Header, len(hs))
		for i, h := range hs {
			headers[i] = (endpoint.Header)(h)
		}
		eo.eopts = append(eo.eopts, endpoint.Headers(headers...))
	}
}

// HeaderValue
func HeaderValue(ctx context.Context, name string) string {
	return endpoint.HeaderValue(ctx, name)
}

// PathParam
type PathParam endpoint.PathParam

// PathParams
func PathParams(ps ...PathParam) EndpointOption {
	return func(eo *endpointOptions) {
		params := make([]endpoint.PathParam, len(ps))
		for i, p := range ps {
			params[i] = (endpoint.PathParam)(p)
		}
		eo.eopts = append(eo.eopts, endpoint.PathParams(params...))
	}
}

// PathValue
func PathValue(ctx context.Context, name string) string {
	return endpoint.PathValue(ctx, name)
}

// QueryParam
type QueryParam endpoint.QueryParam

// QueryParams
func QueryParams(qps ...QueryParam) EndpointOption {
	return func(eo *endpointOptions) {
		params := make([]endpoint.QueryParam, len(qps))
		for i, param := range qps {
			params[i] = (endpoint.QueryParam)(param)
		}

		eo.eopts = append(eo.eopts, endpoint.QueryParams(params...))
	}
}

// QueryValue
func QueryValue(ctx context.Context, name string) string {
	return endpoint.QueryValue(ctx, name)
}

// NewProtoEndpoint
func NewProtoEndpoint[I, O any, Req ProtoMessage[I], Resp ProtoMessage[O]](method string, path string, h Handler[I, O, Req, Resp], opts ...EndpointOption) Endpoint {
	eo := &endpointOptions{}
	for _, opt := range opts {
		opt(eo)
	}
	eo.eopts = append(eo.eopts, endpoint.OnError(&errHandler{marshal: proto.Marshal}))

	protoHandler := protoHandler[I, O, Req, Resp]{
		inner: h,
	}

	op := endpoint.NewOperation(protoHandler, eo.eopts...)

	return Endpoint{
		method:    method,
		path:      path,
		operation: op,
	}
}

// ServeHTTP implements the [http.Handler] interface.
func (e Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.operation.ServeHTTP(w, r)
}

type protoHandler[I, O any, Req ProtoMessage[I], Resp ProtoMessage[O]] struct {
	inner Handler[I, O, Req, Resp]
}

func (h protoHandler[I, O, Req, Resp]) Handle(ctx context.Context, req *protoRequest[I, Req]) (*protoResponse[O, Resp], error) {
	resp, err := h.inner.Handle(ctx, &req.msg)
	if err != nil {
		return nil, err
	}
	return &protoResponse[O, Resp]{msg: resp}, nil
}

type protoRequest[I any, T ProtoMessage[I]] struct {
	msg I
}

func (req protoRequest[I, T]) ContentType() string {
	var msg T = &req.msg
	desc := msg.ProtoReflect().Descriptor()
	if desc.Fields().Len() == 0 {
		return ""
	}
	return ProtobufContentType
}

func (req protoRequest[I, T]) Validate() error {
	var msg T = &req.msg
	vt, ok := any(msg).(endpoint.Validator)
	if !ok {
		return nil
	}
	return vt.Validate()
}

func (req protoRequest[I, T]) OpenApiV3Schema() (*openapi3.Schema, error) {
	var msg T = &req.msg
	jsonSchema, err := reflectJsonSchema(msg.ProtoReflect().Descriptor())
	if err != nil {
		return nil, err
	}
	var schemaOrRef openapi3.SchemaOrRef
	schemaOrRef.FromJSONSchema(jsonSchema.ToSchemaOrBool())
	return schemaOrRef.Schema, nil
}

func (req *protoRequest[I, T]) ReadFrom(r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}

	var msg T = &req.msg
	err = proto.Unmarshal(b, msg)
	return int64(len(b)), err
}

type protoResponse[I any, T ProtoMessage[I]] struct {
	msg T
}

func (resp protoResponse[I, T]) ContentType() string {
	var i I
	var msg T = &i
	desc := msg.ProtoReflect().Descriptor()
	if desc.Fields().Len() == 0 {
		return ""
	}
	return ProtobufContentType
}

func (resp protoResponse[I, T]) OpenApiV3Schema() (*openapi3.Schema, error) {
	var i I
	var msg T = &i
	jsonSchema, err := reflectJsonSchema(msg.ProtoReflect().Descriptor())
	if err != nil {
		return nil, err
	}
	var schemaOrRef openapi3.SchemaOrRef
	schemaOrRef.FromJSONSchema(jsonSchema.ToSchemaOrBool())
	return schemaOrRef.Schema, nil
}

func (resp *protoResponse[I, T]) WriteTo(w io.Writer) (int64, error) {
	b, err := proto.Marshal(resp.msg)
	if err != nil {
		return 0, err
	}
	return io.Copy(w, bytes.NewReader(b))
}

func reflectJsonSchema(desc protoreflect.MessageDescriptor) (schema jsonschema.Schema, err error) {
	fields := desc.Fields()
	if fields.Len() == 0 {
		return
	}

	schema.AddType(jsonschema.Object)
	for i := range fields.Len() {
		field := fields.Get(i)

		var subschema jsonschema.Schema

		switch field.Kind() {
		case protoreflect.BoolKind:
			subschema.AddType(jsonschema.Boolean)
		case protoreflect.EnumKind:
			subschema.AddType("enum")
		case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
			subschema.AddType(jsonschema.Integer)
			subschema.Format = ptr.Ref("int32")
		case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
			subschema.AddType(jsonschema.Integer)
			subschema.Format = ptr.Ref("uint32")
		case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
			subschema.AddType(jsonschema.Integer)
			subschema.Format = ptr.Ref("int64")
		case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			subschema.AddType(jsonschema.Integer)
			subschema.Format = ptr.Ref("uint64")
		case protoreflect.FloatKind:
			subschema.AddType(jsonschema.Number)
			subschema.Format = ptr.Ref("float32")
		case protoreflect.DoubleKind:
			subschema.AddType(jsonschema.Number)
			subschema.Format = ptr.Ref("float64")
		case protoreflect.StringKind:
			subschema.AddType(jsonschema.String)
		case protoreflect.BytesKind:
			subschema.AddType("binary")
		case protoreflect.MessageKind:
			msgSchema, err := reflectJsonSchema(field.Message())
			if err != nil {
				return subschema, err
			}
			if field.Cardinality() != protoreflect.Repeated {
				subschema = msgSchema
				break
			}

			subschema.AddType(jsonschema.Array)

			subschema.Items = &jsonschema.Items{}
			subschema.Items.WithSchemaOrBool(msgSchema.ToSchemaOrBool())
		}

		var val jsonschema.SchemaOrBool
		val.WithTypeObject(subschema)
		schema.WithPropertiesItem(field.JSONName(), val)
	}
	return
}
