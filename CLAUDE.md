# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Humus is a modular Go framework built on top of Bedrock (Z5Labs' base framework) that provides standardized patterns for building three types of applications:

1. **REST/HTTP Services** - OpenAPI-compliant web applications
2. **gRPC Services** - gRPC-based microservices
3. **Job Services** - One-off job executors

All applications include standardized observability (OpenTelemetry), health checks, and graceful shutdown out of the box.

## Development Commands

### Building
```bash
go build ./...
```

### Testing
```bash
# Run all tests with race detection and coverage
go test -race -cover ./...

# Run tests for a specific package
go test -race -cover ./rest/rpc

# Run a specific test
go test -race -run TestName ./path/to/package
```

### Linting
```bash
# Using golangci-lint (version: latest)
golangci-lint run

# The CI uses the same linter with these settings:
# - Timeout: 5m
# - Skip cache: false
```

## Architecture

### Core Design Pattern

Humus follows a **Builder + Runner** pattern:

1. Each service type has a `Builder()` function returning `bedrock.AppBuilder`
2. Builders automatically wrap apps with:
   - Panic recovery
   - OpenTelemetry SDK initialization
   - Lifecycle management
   - OS signal handling (graceful shutdown)
3. `humus.Runner` orchestrates: config reading → app building → app execution

### Configuration System

All configuration uses YAML with Go templating support:

**Template Functions:**
- `env "VAR_NAME"` - Read environment variable
- `default "value"` - Provide fallback value
- Example: `{{env "OTEL_SERVICE_NAME" | default "my-service"}}`

**Configuration Composition:**
```go
type Config struct {
    humus.Config `config:",squash"`      // Base OTel config
    HTTP struct { Port uint } `config:"http"`  // App-specific
}
```

The `default_config.yaml` file contains framework defaults for OpenTelemetry configuration (traces, metrics, logs). Apps should compose this with their own config using `bedrockcfg.MultiSource`.

### Package Organization

**Framework Packages** (user-facing):
- `rest/` - REST/HTTP application framework
  - `rest/rpc/` - RPC-style HTTP handler abstraction with OpenAPI generation
- `grpc/` - gRPC application framework
- `job/` - Job/batch application framework
- `health/` - Health monitoring abstractions
- `config/` - Configuration schemas (mainly OTel)
- `concurrent/` - Thread-safe utilities

**Internal Packages** (framework implementation):
- `internal/otel/` - OpenTelemetry SDK initialization
- `internal/httpserver/` - HTTP server lifecycle wrapper
- `internal/grpcserver/` - gRPC server lifecycle wrapper
- `internal/detector/` - Resource detection utilities
- `internal/humuspb/` - Internal protobuf definitions

### Key Abstractions

#### REST Services

**rest.Api** - OpenAPI-compliant HTTP handler
- Provides `/openapi.json` endpoint with dynamic schema
- Built-in `/health/liveness` and `/health/readiness` probes
- Routes operations using `rest.Operation[I, O, Req, Resp]`

**rest.Operation Pattern:**
```go
rpc.Handler[Req, Resp]              // Business logic
  → TypedRequest[Req]                // Request deserialization + schema
  → TypedResponse[Resp]              // Response serialization + schema
  → openapi3.Operation               // OpenAPI spec generation
```

Pre-built serializers available:
- `ConsumeJson[T]()` - JSON request reader with schema reflection
- `ReturnJson[T]()` - JSON response writer with schema reflection

#### gRPC Services

**grpc.Api** - gRPC service registrar
- Implements `grpc.ServiceRegistrar`
- Automatic OTel instrumentation via interceptors
- Auto-registration of gRPC Health service
- Health monitoring for registered services

#### Job Services

**job.Handler** - Simple interface with single method:
```go
Handle(ctx bedrock.Context) error
```

No HTTP/gRPC server, just executes once with OTel and lifecycle management.

#### Health Monitoring

**health.Monitor** - Interface for health checks:
```go
Healthy(ctx bedrock.Context) (bool, error)
```

Implementations:
- `health.Binary` - Simple healthy/unhealthy state (thread-safe)
- `health.AndMonitor` - Logical AND composition (fail-fast)
- `health.OrMonitor` - Logical OR composition (check all)

### Service Registration Patterns

**REST:**
```go
api := rest.NewApi("My Service", "v1.0.0")
operation := rpc.NewOperation(
    ConsumeJson(ReturnJson(handlerFunc)),
)
api.Route(http.MethodPost, "/users", operation)
```

**gRPC:**
```go
api := grpc.NewApi()
yourpb.RegisterYourServiceServer(api, implementation)
// Health service auto-registered
```

**Job:**
```go
app := job.New(handlerFunc)
// That's it - just implement Handler interface
```

### Entry Point Pattern

All service types follow this pattern:
```go
func main() {
    rest.Run(configReader, app.Init)
    // or grpc.Run(configReader, app.Init)
    // or job.Run(configReader, app.Init)
}
```

The initializer function receives config and returns the API/App:
```go
func Init(ctx bedrock.Context, cfg Config) (*rest.Api, error) {
    // Initialize your service
}
```

## Important Conventions

1. **Error Handling** - Use `rpc.ErrorHandler` interface for custom error responses in REST operations
2. **OpenAPI Schemas** - Generated automatically via reflection from Go types (uses `github.com/swaggest/jsonschema-go`)
3. **Health Checks** - REST apps should implement `health.Monitor` for readiness probes; gRPC health is automatic
4. **Graceful Shutdown** - Handled automatically by Bedrock lifecycle; no explicit cleanup needed in most cases
5. **OTel Instrumentation** - Automatic for HTTP (via otelhttp) and gRPC (via interceptors); use `otel.Tracer/Meter/Logger` directly in business logic
6. **Option Pattern** - Used throughout for extensibility (e.g., `rest.ApiOption`, `grpc.RunOption`, `rpc.OperationOption`)

## Testing Patterns

Tests use standard Go testing with:
- `github.com/stretchr/testify` for assertions and mocking
- Race detection enabled in CI (`-race` flag)
- Example tests in `*_example_test.go` files demonstrate usage patterns

When writing tests:
- Mock `bedrock.Context` for handler tests
- Use `httptest.NewRecorder()` for HTTP handler tests
- Mock `grpc.ServiceRegistrar` for gRPC registration tests
- Test health monitors in isolation before composing

## Dependencies

**Core Framework:**
- `github.com/z5labs/bedrock` - Application lifecycle and config management
- `go.opentelemetry.io/*` - Observability (required)

**HTTP Specific:**
- `github.com/go-chi/chi/v5` - Router (embedded in rest.Api)
- `github.com/swaggest/openapi-go` - OpenAPI spec generation

**gRPC Specific:**
- `google.golang.org/grpc` - gRPC framework

## Examples

The `example/` directory contains reference implementations:
- `example/rest/` - REST service example
- `example/grpc/` - gRPC service example
- `example/internal/` - Shared internal utilities

Refer to these for real-world usage patterns of the framework.
