// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"context"
	"net/http"

	"github.com/swaggest/openapi-go/openapi3"
)

// Handler represents a RPC style implementation of the core
// logic for your [http.Handler].
type Handler[Req, Resp any] interface {
	Handle(context.Context, *Req) (*Resp, error)
}

// HandlerFunc is an adapter to allow the use of ordinary functions
// as [Handler]s.
type HandlerFunc[Req, Resp any] func(context.Context, *Req) (*Resp, error)

// Handle implements the [Handler] interface.
func (f HandlerFunc[Req, Resp]) Handle(ctx context.Context, req *Req) (*Resp, error) {
	return f(ctx, req)
}


// RequestReader is meant to be implemented by any type which knows how
// unmarshal itself from a [http.Request].
type RequestReader[T any] interface {
	*T

	ReadRequest(context.Context, *http.Request) error
}

// TypedRequest is a [RequestReader] which also provides a OpenAPI 3.0
// spec for itself.
type TypedRequest[T any] interface {
	RequestReader[T]

	Spec() (*openapi3.RequestBody, error)
}

// ResponseWriter is meant to be implemented by any type which knows how
// to marshal itself into a HTTP response.
type ResponseWriter[T any] interface {
	*T

	WriteResponse(context.Context, http.ResponseWriter) error
}

// TypedResponse is a [ResponseWriter] which also provides a OpenAPI 3.0
// spec for itself.
type TypedResponse[T any] interface {
	ResponseWriter[T]

	Spec() (int, *openapi3.Response, error)
}

