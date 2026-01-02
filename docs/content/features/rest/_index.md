---
title: REST Services
description: Building OpenAPI-compliant HTTP APIs
weight: 10
type: docs
---


Humus REST services provide a complete framework for building OpenAPI-compliant HTTP APIs with automatic schema generation, type-safe handlers, and built-in observability.

## Overview

REST services in Humus are built on:

- **[chi](https://github.com/go-chi/chi)** - Fast, lightweight HTTP router
- **[OpenAPI 3.0](https://swagger.io/specification/)** - Automatic API documentation
- **Type Safety** - Compile-time type checking for requests and responses
- **OpenTelemetry** - Automatic tracing and metrics

## Quick Start

```go
package main

import (
    "context"
    "net/http"

    "github.com/z5labs/humus/app"
    "github.com/z5labs/humus/config"
    httpserver "github.com/z5labs/humus/http"
    "github.com/z5labs/humus/otel"
    "github.com/z5labs/humus/rest"

    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/trace"
)

type HelloResponse struct {
    Message string `json:"message"`
}

func main() {
    // Configure HTTP server
    listener := httpserver.NewTCPListener(
        httpserver.Addr(config.Default(":8080", config.Env("HTTP_ADDR"))),
    )
    srv := httpserver.NewServer(listener)

    // Create handler
    handler := rest.ProducerFunc[HelloResponse](func(ctx context.Context) (*HelloResponse, error) {
        return &HelloResponse{Message: "Hello, World!"}, nil
    })

    // Build API
    api := rest.NewApi(
        "Hello Service",
        "1.0.0",
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/hello"),
            rest.ProduceJson(handler),
        ),
    )

    restBuilder := rest.Build(srv, api)

    // Configure OpenTelemetry (disabled for simplicity)
    sdk := otel.SDK{
        TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
            return config.Value[trace.TracerProvider]{}, nil
        }),
        MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
            return config.Value[metric.MeterProvider]{}, nil
        }),
    }

    otelBuilder := otel.Build(sdk, restBuilder)

    _ = app.Run(context.Background(), otelBuilder)
}
```

## Core Components

### rest.Api

The main API object that combines:
- HTTP router (chi)
- OpenAPI spec generator
- Health check endpoints
- Middleware management

### RPC Pattern

Type-safe handler abstraction in the `rest` package:
- `rest.Handler[Req, Resp]` - Business logic interface for full request/response
- `rest.Producer[Resp]` - Response-only handlers (GET endpoints)
- `rest.Consumer[Req]` - Request-only handlers (POST webhooks)
- Request deserialization (JSON, XML, etc.)
- Response serialization
- OpenAPI schema generation

### Path Building

Flexible path definition:
- Static paths: `/users`
- Path parameters: `/users/{id}`
- Nested paths: `/users/{id}/posts/{postId}`

## Built-in Endpoints

Every REST service automatically includes:

- **`GET /openapi.json`** - OpenAPI 3.0 specification
- **`GET /health/liveness`** - Liveness probe
- **`GET /health/readiness`** - Readiness probe

## What You'll Learn

This section covers:

- [Quick Start]({{< ref "quick-start" >}}) - Build your first REST API
- [Handler Helpers]({{< ref "handler-helpers" >}}) - Type-safe handlers and serialization
- [Routing]({{< ref "routing" >}}) - Paths and parameters
- [Authentication]({{< ref "authentication" >}}) - JWT, API keys, and security
- [Interceptors]({{< ref "interceptors" >}}) - Operation-level request/response processing
- [Error Handling]({{< ref "error-handling" >}}) - Custom error responses
- [OpenAPI]({{< ref "openapi" >}}) - Working with generated specs
- [Health Checks]({{< ref "health-checks" >}}) - Monitoring service health

## Next Steps

Start with the [Quick Start Guide]({{< ref "quick-start" >}}) to build your first REST service.