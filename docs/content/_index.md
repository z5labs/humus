---
title: Humus
type: docs
---

# Humus

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
- YAML-based configuration with templating support

**Developer Friendly**
- Minimal boilerplate with Builder + Runner pattern
- Automatic OpenAPI schema generation from Go types
- Type-safe request/response handling
- Comprehensive examples and documentation

## Quick Example

```go
package main

import (
    "context"
    "net/http"

    "github.com/z5labs/humus/rest"
    "github.com/z5labs/humus/rest/rpc"
)

type Config struct {
    rest.Config `config:",squash"`
}

func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    api := rest.NewApi("My Service", "v1.0.0")

    handler := rpc.NewOperation(
        rpc.Handle(func(ctx context.Context, req any) (string, error) {
            return "Hello, World!", nil
        }),
    )

    rest.Handle(http.MethodGet, rest.BasePath("/hello"), handler)
    return api, nil
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
