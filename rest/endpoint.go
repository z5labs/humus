// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"net/http"

	"github.com/z5labs/bedrock/rest"
	"github.com/z5labs/bedrock/rest/endpoint"
	"google.golang.org/protobuf/proto"
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
			e.method,
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

// NewEndpoint
func NewEndpoint[I, O any, Req ProtoMessage[I], Resp ProtoMessage[O]](method string, path string, h Handler[I, O, Req, Resp], opts ...EndpointOption) Endpoint {
	eo := &endpointOptions{}
	for _, opt := range opts {
		opt(eo)
	}
	eo.eopts = append(eo.eopts, endpoint.OnError(&errHandler{marshal: proto.Marshal}))

	op := endpoint.NewOperation(h, eo.eopts...)

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
