---
title: Humus
type: docs
---


**A modular Go framework for building production-ready services with standardized observability, health checks, and graceful shutdown.**

Humus is built on top of [Bedrock](https://github.com/z5labs/bedrock) and provides opinionated patterns for three types of applications:

## Service Types

### REST/HTTP Services
Build OpenAPI-compliant web applications with automatic schema generation, built-in health endpoints, and flexible request/response handling.

[Get Started with REST →]({{< ref "/features/rest" >}})

### gRPC Services
Create gRPC microservices with automatic OpenTelemetry instrumentation, health service registration, and seamless integration with the gRPC ecosystem.

[Get Started with gRPC →]({{< ref "/features/grpc" >}})

### Job Services
Build one-off job executors for batch processing, migrations, or scheduled tasks with the same observability and lifecycle management as long-running services.

[Get Started with Jobs →]({{< ref "/features/job" >}})

## Key Features

**Batteries Included Observability**
- Automatic OpenTelemetry SDK initialization for traces, metrics, and logs
- Integrated structured logging with `slog`
- Zero-configuration instrumentation for HTTP and gRPC

**Production Ready**
- Graceful shutdown with OS signal handling
- Standardized health check patterns
- Panic recovery middleware
- Type-safe configuration system with `config.Reader[T]`

**Developer Friendly**
- Minimal boilerplate with builder pattern
- Automatic OpenAPI schema generation from Go types
- Type-safe request/response handling
- Comprehensive examples and documentation

## Quick Example

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
    // Configure HTTP server
    listener := httpserver.NewTCPListener(
        httpserver.Addr(config.Default(":8080", config.Env("HTTP_ADDR"))),
    )
    srv := httpserver.NewServer(listener)

    // Create API
    handler := rest.ProducerFunc[string](func(ctx context.Context) (*string, error) {
        msg := "Hello, World!"
        return &msg, nil
    })

    api := rest.NewApi(
        "My Service",
        "v1.0.0",
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/hello"),
            rest.ProduceJson(handler),
        ),
    )

    // Build application
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

    // Run
    _ = app.Run(context.Background(), otelBuilder)
}
```

## Next Steps

- [Installation and Getting Started]({{< ref "/getting-started" >}})
- [Core Concepts]({{< ref "/concepts" >}})
- [API Reference](https://pkg.go.dev/github.com/z5labs/humus)

## Resources

- [GitHub Repository](https://github.com/z5labs/humus)
- [GitHub Discussions](https://github.com/z5labs/humus/discussions)
- [Go Package Documentation](https://pkg.go.dev/github.com/z5labs/humus)
