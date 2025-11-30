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

    "github.com/z5labs/humus/rest"
)

type Config struct {
    rest.Config `config:",squash"`
}

type HelloResponse struct {
    Message string `json:"message"`
}

func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    handler := rest.ProducerFunc[HelloResponse](func(ctx context.Context) (*HelloResponse, error) {
        return &HelloResponse{Message: "Hello, World!"}, nil
    })

    api := rest.NewApi(
        "Hello Service",
        "1.0.0",
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/hello"),
            rest.ProduceJson(handler),
        ),
    )
    return api, nil
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

Type-safe handler abstraction in the rest package:
- `rest.Handler[Req, Resp]` - Business logic interface
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
- [Error Handling]({{< ref "error-handling" >}}) - Custom error responses
- [OpenAPI]({{< ref "openapi" >}}) - Working with generated specs
- [Health Checks]({{< ref "health-checks" >}}) - Monitoring service health

## Next Steps

Start with the [Quick Start Guide]({{< ref "quick-start" >}}) to build your first REST service.