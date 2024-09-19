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
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Empty is a helper type to be used when your API accepts
// or returns an empty request or response, respectively.
type Empty struct{}

// ProtoReflect implements the [protoreflect.ProtoMessage] interface.
func (Empty) ProtoReflect() protoreflect.Message {
	return nil
}

// ContentTyper
type ContentTyper endpoint.ContentTyper

// Handler
type Handler[Req, Resp proto.Message] endpoint.Handler[Req, Resp]

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

// NewEndpoint
func NewEndpoint[Req, Resp proto.Message](method string, path string, h Handler[Req, Resp], opts ...EndpointOption) Endpoint {
	eo := &endpointOptions{}
	for _, opt := range opts {
		opt(eo)
	}
	eo.eopts = append(eo.eopts, endpoint.OnError(errHandler{}))

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
