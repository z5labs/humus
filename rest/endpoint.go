// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/bedrock/pkg/ptr"
	"github.com/z5labs/bedrock/rest"
	"github.com/z5labs/bedrock/rest/endpoint"
	"github.com/z5labs/bedrock/rest/mux"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Handler
type Handler[I, O any] endpoint.Handler[I, O]

// OperationHandler
type OperationHandler[I, O any, Req endpoint.Request[I], Resp endpoint.Response[O]] Handler[I, O]

// ProtobufContentType
var ProtobufContentType = "application/x-protobuf"

// ProtoMessage embeds [proto.Message] but adds *T support.
type ProtoMessage[T any] interface {
	*T
	proto.Message
}

// ContentTyper
type ContentTyper[T any] interface {
	*T
	endpoint.ContentTyper
}

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

// NewEndpoint
func NewEndpoint[I, O any, Req endpoint.Request[I], Resp endpoint.Response[O]](method, path string, h OperationHandler[I, O, Req, Resp], opts ...EndpointOption) Endpoint {
	eo := &endpointOptions{}
	for _, opt := range opts {
		opt(eo)
	}
	eo.eopts = append(eo.eopts, endpoint.OnError(&errHandler{marshal: proto.Marshal}))

	op := endpoint.NewOperation[I, O, Req, Resp](h, eo.eopts...)

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

type ConsumeProtoHandler[I, O any, Req ProtoMessage[I]] struct {
	inner Handler[I, O]
}

// ConsumesProto
func ConsumesProto[I, O any, Req ProtoMessage[I]](h Handler[I, O]) ConsumeProtoHandler[I, O, Req] {
	return ConsumeProtoHandler[I, O, Req]{inner: h}
}

// ProtoRequest
type ProtoRequest[I any, T ProtoMessage[I]] struct {
	msg I
}

// Handle implements the [Handler] interface.
func (h ConsumeProtoHandler[I, O, Req]) Handle(ctx context.Context, req *ProtoRequest[I, Req]) (*O, error) {
	return h.inner.Handle(ctx, &req.msg)
}

func (req ProtoRequest[I, T]) ContentType() string {
	var msg T = &req.msg
	desc := msg.ProtoReflect().Descriptor()
	if desc.Fields().Len() == 0 {
		return ""
	}
	return ProtobufContentType
}

func (req ProtoRequest[I, T]) Validate() error {
	var msg T = &req.msg
	vt, ok := any(msg).(endpoint.Validator)
	if !ok {
		return nil
	}
	return vt.Validate()
}

func (req ProtoRequest[I, T]) OpenApiV3Schema() (*openapi3.Schema, error) {
	var msg T = &req.msg
	jsonSchema, err := reflectJsonSchema(msg.ProtoReflect().Descriptor())
	if err != nil {
		return nil, err
	}
	var schemaOrRef openapi3.SchemaOrRef
	schemaOrRef.FromJSONSchema(jsonSchema.ToSchemaOrBool())
	return schemaOrRef.Schema, nil
}

func (req *ProtoRequest[I, T]) ReadRequest(r *http.Request) (err error) {
	defer closeError(&err, r.Body)

	var b []byte
	b, err = io.ReadAll(r.Body)
	if err != nil {
		return
	}

	var msg T = &req.msg
	err = proto.Unmarshal(b, msg)
	return
}

func closeError(err *error, c io.Closer) {
	closeErr := c.Close()
	if closeErr == nil {
		return
	}
	if *err == nil {
		*err = closeErr
		return
	}
	*err = errors.Join(*err, closeErr)
}

// ProduceProtoHandler
type ProduceProtoHandler[I, O any, Resp ProtoMessage[O]] struct {
	inner Handler[I, O]
}

// ProducesProto
func ProducesProto[I, O any, Resp ProtoMessage[O]](h Handler[I, O]) ProduceProtoHandler[I, O, Resp] {
	return ProduceProtoHandler[I, O, Resp]{inner: h}
}

// ProtoResponse
type ProtoResponse[I any, T ProtoMessage[I]] struct {
	msg T
}

// Handle implements the [Handler] interface.
func (h ProduceProtoHandler[I, O, Resp]) Handle(ctx context.Context, req *I) (*ProtoResponse[O, Resp], error) {
	resp, err := h.inner.Handle(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	return &ProtoResponse[O, Resp]{msg: resp}, nil
}

func (resp ProtoResponse[I, T]) ContentType() string {
	var i I
	var msg T = &i
	desc := msg.ProtoReflect().Descriptor()
	if desc.Fields().Len() == 0 {
		return ""
	}
	return ProtobufContentType
}

func (resp ProtoResponse[I, T]) OpenApiV3Schema() (*openapi3.Schema, error) {
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

func (resp *ProtoResponse[I, T]) WriteTo(w io.Writer) (int64, error) {
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

// ConsumeMultipartFormDataHandler
type ConsumeMultipartFormDataHandler[I endpoint.OpenApiV3Schemaer, O any] struct {
	inner Handler[multipart.Reader, O]
}

// ConsumesMultipartFormData
func ConsumesMultipartFormData[I endpoint.OpenApiV3Schemaer, O any](h Handler[multipart.Reader, O]) ConsumeMultipartFormDataHandler[I, O] {
	return ConsumeMultipartFormDataHandler[I, O]{inner: h}
}

// MultipartFormDataRequest
type MultipartFormDataRequest[I endpoint.OpenApiV3Schemaer] struct {
	r *multipart.Reader
	c io.Closer
}

// Handle implements the [Handler] interface.
func (h ConsumeMultipartFormDataHandler[I, O]) Handle(ctx context.Context, req *MultipartFormDataRequest[I]) (resp *O, err error) {
	defer closeError(&err, req.c)

	resp, err = h.inner.Handle(ctx, req.r)
	return
}

// ContentType implements the [endpoint.ContentTyper] interface.
func (req MultipartFormDataRequest[I]) ContentType() string {
	return "multipart/form-data"
}

// Validate implements the [endpoint.Validator] interface.
func (req MultipartFormDataRequest[I]) Validate() error {
	return nil
}

// OpenApiV3Schema implements the [endpoint.OpenApiV3Schemaer] interface.
func (MultipartFormDataRequest[I]) OpenApiV3Schema() (*openapi3.Schema, error) {
	var i I
	return i.OpenApiV3Schema()
}

// ReadRequest implements the [endpoint.RequestReader] interface.
func (req *MultipartFormDataRequest[I]) ReadRequest(r *http.Request) error {
	mr, err := r.MultipartReader()
	if err != nil {
		return err
	}

	req.r = mr
	req.c = r.Body
	return nil
}

type MultipartWriter interface {
	CreatePart(header textproto.MIMEHeader) (io.Writer, error)
}

type MultipartResponseWriter[T any] interface {
	*T

	endpoint.OpenApiV3Schemaer

	WriteParts(w MultipartWriter) error
}

type ProduceMultipartFormDataHandler[I, O any, Resp MultipartResponseWriter[O]] struct {
	inner Handler[I, O]
}

func ProducesMultipartFormData[I, O any, Resp MultipartResponseWriter[O]](h Handler[I, O]) ProduceMultipartFormDataHandler[I, O, Resp] {
	return ProduceMultipartFormDataHandler[I, O, Resp]{inner: h}
}

type MultipartFormDataResponse[I any, T MultipartResponseWriter[I]] struct {
	inner T
}

func (MultipartFormDataResponse[I, T]) ContentType() string {
	return "multipart/form-data"
}

func (MultipartFormDataResponse[I, T]) OpenApiV3Schema() (*openapi3.Schema, error) {
	var i I
	var t T = &i
	return t.OpenApiV3Schema()
}

func (resp *MultipartFormDataResponse[I, T]) WriteTo(w io.Writer) (int64, error) {
	mw := multipart.NewWriter(w)
	err := resp.inner.WriteParts(mw)
	return 0, err
}

func (h ProduceMultipartFormDataHandler[I, O, Resp]) Handle(ctx context.Context, req *I) (*MultipartFormDataResponse[O, Resp], error) {
	resp, err := h.inner.Handle(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	return &MultipartFormDataResponse[O, Resp]{inner: resp}, nil
}
