// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package rest provides an opinionated HTTPS REST server built on top of
// bedrock's HTTP and OTel runtimes.
//
// The server is HTTPS-only. If no TLS certificate is provided, a self-signed
// certificate is generated automatically at startup (suitable for development).
//
// All framework-level configuration is read from environment variables so no
// config file is required. Options passed to [Run] override the env var defaults.
//
// # Environment Variables
//
//   - HUMUS_REST_PORT            - TCP port to listen on (default: 8443)
//   - HUMUS_REST_READ_TIMEOUT    - Maximum duration for reading a request (default: 5s)
//   - HUMUS_REST_READ_HEADER_TIMEOUT - Maximum duration for reading request headers (default: 2s)
//   - HUMUS_REST_WRITE_TIMEOUT   - Maximum duration for writing a response (default: 10s)
//   - HUMUS_REST_IDLE_TIMEOUT    - Maximum idle time between keep-alive requests (default: 120s)
//   - HUMUS_REST_MAX_HEADER_BYTES - Maximum size of request headers in bytes (default: 1048576)
//   - HUMUS_REST_TLS_PKCS12_FILE     - Path to a DER-encoded PKCS#12 file containing the certificate and private key
//   - HUMUS_REST_TLS_PKCS12_PASSWORD - Password for the PKCS#12 file (empty string if no password)
//
// # OpenTelemetry
//
// Real OTel SDK providers are always initialised. By default traces and metrics
// use noop exporters (discarded), and logs are written to stdout. Provide
// [OTLPExporter] with a gRPC target to route all three signals to an OTLP
// collector instead.
//
// Every HTTP request is automatically wrapped in an OTel span via otelhttp.
//
// # Basic Usage
//
//	package main
//
//	import (
//	    "context"
//	    "net/http"
//
//	    "github.com/z5labs/humus/rest"
//	    bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
//	)
//
//	type HelloResponse struct {
//	    Message string `json:"message"`
//	}
//
//	type APIError struct {
//	    Message string `json:"message"`
//	}
//
//	func (e APIError) Error() string { return e.Message }
//
//	func main() {
//	    ep := bedrockrest.GET("/hello", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) (HelloResponse, error) {
//	        return HelloResponse{Message: "Hello, World!"}, nil
//	    })
//	    ep = bedrockrest.WriteJSON[HelloResponse](http.StatusOK, ep)
//	    route := bedrockrest.CatchAll(http.StatusInternalServerError, func(err error) APIError {
//	        return APIError{Message: err.Error()}
//	    }, ep)
//
//	    if err := rest.Run(
//	        context.Background(),
//	        rest.Title("Hello API"),
//	        rest.Version("1.0.0"),
//	        rest.Handle(route),
//	    ); err != nil {
//	        panic(err)
//	    }
//	}
package rest
