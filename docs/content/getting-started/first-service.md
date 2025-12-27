---
title: Your First Service
description: Build a Hello World REST service with Humus
weight: 20
type: docs
---


In this guide, you'll build a simple REST service that responds with "Hello, World!". This will introduce you to the core concepts of Humus.

## Project Setup

Create a new directory and initialize a Go module:

```bash
mkdir hello-humus
cd hello-humus
go mod init hello-humus
go get github.com/z5labs/humus
```

## Application Code

Create a `main.go` file:

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

func main() {
    // Configure HTTP listener with default port :8080
    listener := httpserver.NewTCPListener(
        httpserver.Addr(config.Default(":8080", config.Env("HTTP_ADDR"))),
    )

    // Configure HTTP server
    srv := httpserver.NewServer(listener)

    // Create a simple handler that returns "Hello, World!"
    handler := rest.ProducerFunc[string](func(ctx context.Context) (*string, error) {
        msg := "Hello, World!"
        return &msg, nil
    })

    // Create API with GET /hello endpoint
    api := rest.NewApi(
        "Hello Service",
        "1.0.0",
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/hello"),
            rest.ProduceJson(handler),
        ),
    )

    // Build REST application
    restBuilder := rest.Build(srv, api)

    // Configure OpenTelemetry SDK (disabled for this simple example)
    sdk := otel.SDK{
        TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
            return config.Value[trace.TracerProvider]{}, nil
        }),
        MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
            return config.Value[metric.MeterProvider]{}, nil
        }),
    }

    // Wrap with OpenTelemetry
    otelBuilder := otel.Build(sdk, restBuilder)

    // Run the application
    _ = app.Run(context.Background(), otelBuilder)
}
```

## Running the Service

Run your service:

```bash
go run main.go
```

You should see output indicating the server has started. The service is now running on http://localhost:8080.

## Testing the Endpoint

In another terminal, test your endpoint:

```bash
curl http://localhost:8080/hello
```

You should see:

```
"Hello, World!"
```

## Exploring Built-in Endpoints

Humus automatically provides several endpoints:

### OpenAPI Specification

```bash
curl http://localhost:8080/openapi.json
```

This returns the OpenAPI 3.0 specification for your API, automatically generated from your code.

### Health Checks

```bash
# Liveness probe
curl http://localhost:8080/health/liveness

# Readiness probe
curl http://localhost:8080/health/readiness
```

Both should return `200 OK` with `{"healthy":true}`.

## Understanding the Code

Let's break down what's happening:

1. **Configuration**: Configuration is handled via `config.Reader[T]` types. In this example, `config.ReaderOf(":8080")` provides a static value for the HTTP address.

2. **Builder Pattern**: Applications are built using the builder pattern:
   - `rest.Build()` creates an HTTP application builder
   - `otel.Build()` wraps it with OpenTelemetry instrumentation
   - `app.Run()` executes the builder and manages the lifecycle

3. **API Creation**: The `rest.NewApi()` function creates your API:
   - Define API metadata (name and version)
   - Register handlers with HTTP methods and paths
   - Handlers are registered inline using `rest.Handle()`

4. **Handler Pattern**: The `rest.ProducerFunc` creates a type-safe handler that produces a `string` response with no request body.

5. **Lifecycle Management**: `app.Run()` handles:
   - Building the application from the builder
   - Starting the HTTP server
   - Graceful shutdown on OS signals (SIGINT, SIGTERM)

## Next Steps

Now that you have a working service, you can:

- Learn about [Configuration]({{< ref "configuration" >}}) to use environment variables and dynamic config
- Explore [Project Structure]({{< ref "project-structure" >}}) for organizing larger applications
- Read about [REST Services]({{< ref "/features/rest" >}}) for more advanced HTTP patterns
- Understand [Core Concepts]({{< ref "/concepts" >}}) for a deeper dive into Humus architecture

## Complete Example

The complete code for this example is available in the [Humus repository](https://github.com/z5labs/humus/tree/main/example).
