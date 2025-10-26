// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package rpc provides RPC-style handler implementations that integrate with the
// [rest] package to build HTTP APIs.
//
// This package offers three primary functions for creating JSON handlers:
//   - [ProduceJson] - GET endpoints that return JSON (no request body)
//   - [ConsumeOnlyJson] - POST/PUT endpoints that accept JSON (no response body)
//   - [HandleJson] - POST/PUT endpoints with JSON request and response
//
// All handlers implement the [rest.Handler] interface, providing both HTTP handling
// and OpenAPI schema generation capabilities. Use them with [rest.Handle] to register
// operations on a [rest.Api].
//
// # Basic Usage
//
// Create a handler and register it with rest.Handle():
//
//	handler := rpc.ProduceJson(
//	    rpc.ProducerFunc[Response](func(ctx context.Context) (*Response, error) {
//	        return &Response{Message: "hello"}, nil
//	    }),
//	)
//
//	operation := rest.Handle(http.MethodGet, rest.BasePath("/hello"), handler)
//	api := rest.NewApi("My API", "v1.0.0", operation)
//
// # Error Handling
//
// Errors are handled by the rest package via [rest.ErrorHandler]. Custom error
// handlers can be configured per-operation using [rest.OnError].
//
// The [InvalidContentTypeError] type implements [rest.HttpResponseWriter] to
// return appropriate HTTP 400 responses when content types don't match.
package rpc
