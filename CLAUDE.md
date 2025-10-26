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
# - Tests: false (configured in .golangci.yaml)
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
- `internal/grpchealth/` - gRPC health service implementation

### Key Abstractions

#### REST Services

**rest.Api** - OpenAPI-compliant HTTP handler
- Provides `/openapi.json` endpoint with dynamic schema
- Built-in `/health/liveness` and `/health/readiness` probes
- Routes operations using `rest.Handle()` with path patterns

**rest/rpc Pattern:**
```go
rpc.Handler[Req, Resp]              // Business logic
  → TypedRequest[Req]                // Request deserialization + schema
  → TypedResponse[Resp]              // Response serialization + schema
  → openapi3.Operation               // OpenAPI spec generation
```

Pre-built serializers available:
- `ConsumeJson[Req, Resp](handler)` - Wraps handler to consume JSON requests
- `ReturnJson[Req, Resp](handler)` - Wraps handler to return JSON responses
- Chain them: `rpc.NewOperation(ConsumeJson(ReturnJson(handlerFunc)))`

**Path Building:**
```go
rest.BasePath("/users")              // Static path: /users
rest.BasePath("/users").Param("id")  // Path parameter: /users/{id}
```

**Parameter Validation:**
```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users"),
    handler,
    rest.QueryParam("format", rest.Required()),
    rest.Header("Authorization", rest.Required()),
)
```

**JWT Authentication:**
```go
// Implement a JWT verifier
type MyJWTVerifier struct {
    publicKey *rsa.PublicKey
}

func (v *MyJWTVerifier) Verify(ctx context.Context, token string) (context.Context, error) {
    // Verify JWT signature and claims
    claims, err := jwt.Parse(token, v.publicKey)
    if err != nil {
        return nil, err
    }
    // Inject claims into context for downstream handlers
    return context.WithValue(ctx, "claims", claims), nil
}

// Use in handler registration
rest.Handle(
    http.MethodGet,
    rest.BasePath("/protected"),
    handler,
    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", &MyJWTVerifier{})),
)
```

#### gRPC Services

**grpc.Api** - gRPC service registrar
- Implements `grpc.ServiceRegistrar`
- Automatic OTel instrumentation via interceptors
- Auto-registration of gRPC Health service
- Health monitoring for registered services (if service implements `health.Monitor`)

#### Job Services

**job.Handler** - Simple interface with single method:
```go
Handle(ctx context.Context) error
```

No HTTP/gRPC server, just executes once with OTel and lifecycle management.

#### Health Monitoring

**health.Monitor** - Interface for health checks:
```go
Healthy(ctx context.Context) (bool, error)
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
rest.Handle(http.MethodPost, rest.BasePath("/users"), operation)
```

**gRPC:**
```go
api := grpc.NewApi()
yourpb.RegisterYourServiceServer(api, implementation)
// Health service auto-registered
```

**Job:**
```go
app := job.NewApp(handlerFunc)
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
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Initialize your service
}
```

## Important Conventions

1. **Error Handling** - Use `rpc.ErrorHandler` interface for custom error responses in REST operations; set via `OnError()` operation option
2. **JWT Authentication** - Implement `rest.JWTVerifier` interface to verify tokens and inject claims into context; framework handles token extraction from "Bearer " header and returns 401 on failure
3. **OpenAPI Schemas** - Generated automatically via reflection from Go types (uses `github.com/swaggest/jsonschema-go`)
4. **Health Checks** - REST apps should implement custom health handlers via `rest.Readiness()` and `rest.Liveness()` options; gRPC health is automatic
5. **Graceful Shutdown** - Handled automatically by Bedrock lifecycle; no explicit cleanup needed in most cases
6. **OTel Instrumentation** - Automatic for HTTP (via otelhttp) and gRPC (via interceptors); use `otel.Tracer/Meter` directly in business logic
7. **Logging** - Use `humus.Logger(name)` to get an OpenTelemetry-integrated logger; returns `*slog.Logger`
8. **Option Pattern** - Used throughout for extensibility (e.g., `rest.ApiOption`, `grpc.RunOption`, `rpc.OperationOption`)

## Testing Patterns

Tests use standard Go testing with:
- `github.com/stretchr/testify` for assertions and mocking
- Race detection enabled in CI (`-race` flag)
- Example tests in `*_example_test.go` files demonstrate usage patterns

When writing tests:
- Mock `context.Context` for handler tests
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
- `github.com/swaggest/jsonschema-go` - JSON schema reflection

**gRPC Specific:**
- `google.golang.org/grpc` - gRPC framework

## Examples

The `example/` directory contains reference implementations:
- `example/grpc/petstore/` - gRPC service example with health monitoring

Refer to these for real-world usage patterns of the framework.
