# Humus

[![Go Reference](https://pkg.go.dev/badge/github.com/z5labs/humus.svg)](https://pkg.go.dev/github.com/z5labs/humus)
[![Go Report Card](https://goreportcard.com/badge/github.com/z5labs/humus)](https://goreportcard.com/report/github.com/z5labs/humus)
![Coverage](https://img.shields.io/badge/Coverage-33.1%25-yellow)
[![build](https://github.com/z5labs/humus/actions/workflows/build.yaml/badge.svg)](https://github.com/z5labs/humus/actions/workflows/build.yaml)

A modular Go framework for building production-ready REST APIs, gRPC services, and batch jobs with built-in observability, health checks, and graceful shutdown.

Built on top of [Bedrock](https://github.com/z5labs/bedrock), Humus provides standardized patterns and automatic instrumentation to help you focus on business logic while maintaining best practices.

## Quick Start

```go
package main

import (
    "context"
    "net/http"

    "github.com/z5labs/humus/rest"
    "github.com/z5labs/humus/rest/rpc"
)

type HelloResponse struct {
    Message string `json:"message"`
}

func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)
}

func Init(ctx context.Context, cfg rest.Config) (*rest.Api, error) {
    api := rest.NewApi("Hello API", "1.0.0")

    handler := rpc.ProducerFunc[HelloResponse](func(ctx context.Context) (*HelloResponse, error) {
        return &HelloResponse{Message: "Hello, World!"}, nil
    })

    rest.Handle(http.MethodGet, rest.BasePath("/hello"), rpc.ProduceJson(handler))
    return api, nil
}
```

Your API is now running with:
- Automatic OpenAPI documentation at `/openapi.json`
- Health endpoints at `/health/liveness` and `/health/readiness`
- OpenTelemetry tracing and metrics
- Graceful shutdown handling

## Features

### 🌐 REST Services
- **OpenAPI 3.0** - Automatic spec generation from Go types
- **Type-safe handlers** - Compile-time request/response validation
- **Built-in authentication** - JWT, API keys, Basic auth, OAuth2
- **Parameter validation** - Query params, headers, path params with regex support

### 🔌 gRPC Services
- **Auto-instrumentation** - OpenTelemetry interceptors built-in
- **Health service** - Automatic gRPC health checks
- **Service registration** - Simple, familiar gRPC patterns

### ⚙️ Job/Batch Services
- **One-off execution** - Run jobs with observability and lifecycle management
- **Simple interface** - Just implement `Handle(ctx context.Context) error`

### 📊 Observability
- **OpenTelemetry SDK** - Traces, metrics, and logs out of the box
- **Structured logging** - slog integration with context propagation
- **Health monitoring** - Composable health check patterns

## Installation

```bash
go get github.com/z5labs/humus
```

## Documentation

📚 **[Full Documentation](https://z5labs.dev/humus/)** - Comprehensive guides, examples, and API reference

- [Getting Started](https://z5labs.dev/humus/getting-started/) - Installation and first service
- [REST Services](https://z5labs.dev/humus/features/rest/) - Building HTTP APIs
- [gRPC Services](https://z5labs.dev/humus/features/grpc/) - Building gRPC services
- [Job Services](https://z5labs.dev/humus/features/job/) - Batch processing
- [Authentication](https://z5labs.dev/humus/features/rest/authentication/) - Securing your APIs

## Examples

Check out the [examples directory](./example) for complete, runnable examples:
- [REST Petstore](./example/rest/petstore) - Full REST API example
- [gRPC Petstore](./example/grpc/petstore) - gRPC service with health monitoring

## AI Coding Agent Instructions

If you're using an AI coding agent (like GitHub Copilot, Cursor, etc.), copy the relevant instruction files from the [instructions/](./instructions/) directory to your project repository (e.g., `.github/`). These files provide your AI assistant with Humus framework best practices, project structure patterns, and common pitfalls to avoid.

**Available instruction files:**
- [humus-common.instructions.md](./instructions/humus-common.instructions.md) - Common patterns for all service types (required)
- [humus-rest.instructions.md](./instructions/humus-rest.instructions.md) - REST API specific patterns
- [humus-grpc.instructions.md](./instructions/humus-grpc.instructions.md) - gRPC service specific patterns
- [humus-kafka.instructions.md](./instructions/humus-kafka.instructions.md) - Kafka queue processor specific patterns
- [humus-job.instructions.md](./instructions/humus-job.instructions.md) - Job executor specific patterns

Copy `humus-common.instructions.md` along with the file(s) specific to your application type.

## Requirements

- Go 1.24 or later

## License

Released under the [MIT License](LICENSE).
