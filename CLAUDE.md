# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

Humus is a modular Go framework built on top of Bedrock (Z5Labs' base framework) that provides standardized patterns for building four types of applications:

1. **REST/HTTP Services** - OpenAPI-compliant web applications
2. **gRPC Services** - gRPC-based microservices
3. **Job Services** - One-off job executors
4. **Queue Services** - Message queue processors with at-most-once or at-least-once delivery semantics

All applications include standardized observability (OpenTelemetry), health checks, and graceful shutdown out of the box.

## Requirements

- Go 1.24.0 or later (see `go.mod`)

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

### CI/CD Workflows

Located in `.github/workflows/`:
- `build.yaml` - Lint and test pipeline
- `coverage.yaml` - Test coverage reporting
- `docs.yaml` - Documentation site deployment
- `codeql.yaml` - Security analysis

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
- `queue/` - Message queue processing framework with delivery semantics
  - `queue/kafka/` - Kafka-specific runtime with goroutine-per-partition concurrency
- `health/` - Health monitoring abstractions
- `config/` - Configuration schemas (mainly OTel)
- `concurrent/` - Thread-safe utilities

**Internal Packages** (framework implementation):
- `internal/httpserver/` - HTTP server lifecycle wrapper
- `internal/grpcserver/` - gRPC server lifecycle wrapper
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

**Producer/Consumer Shortcuts:**

For GET endpoints (no request body, only response):
- `rpc.ProduceJson[Resp](producer)` - Wraps a Producer to return JSON
- `rpc.ProducerFunc[Resp]` - Function adapter for Producer interface

For POST/PUT webhooks (request body, no response):
- `rpc.ConsumeOnlyJson[Req](consumer)` - Wraps a Consumer to accept JSON
- `rpc.ConsumerFunc[Req]` - Function adapter for Consumer interface

Full request/response:
- `rpc.HandleJson[Req, Resp](handler)` - Combines ConsumeJson and ReturnJson

Example:
```go
// GET endpoint - only produces a response
handler := rpc.ProducerFunc[HelloResponse](func(ctx context.Context) (*HelloResponse, error) {
    return &HelloResponse{Message: "Hello, World!"}, nil
})
rest.Handle(http.MethodGet, rest.BasePath("/hello"), rpc.ProduceJson(handler))

// POST webhook - only consumes a request
consumer := rpc.ConsumerFunc[WebhookPayload](func(ctx context.Context, req *WebhookPayload) error {
    // Process webhook, return status code
    return nil
})
rest.Handle(http.MethodPost, rest.BasePath("/webhook"), rpc.ConsumeOnlyJson(consumer))

// Full request/response - shorthand for ConsumeJson(ReturnJson(handler))
rest.Handle(http.MethodPost, rest.BasePath("/users"), rpc.HandleJson(handlerFunc))
```

**HTML Template Helpers:**

For server-rendered HTML pages (use with HTMX or traditional forms):
- `rest.ProduceHTML[Resp](producer, template)` - Wraps a Producer to return HTML responses
- `rest.ReturnHTML[Req, Resp](handler, template)` - Wraps handler to return HTML responses
- `rest.ConsumeForm[Req, Resp](handler)` - Wraps handler to consume form data
- `rest.ConsumeOnlyForm[Req](consumer)` - Consumes form data without response body
- `rest.HandleForm[Req, Resp](handler)` - Consumes form data and returns JSON

Form data binding uses struct tags:
```go
type MyFormRequest struct {
    Name  string `form:"name"`
    Email string `form:"email"`
    Age   int    `form:"age"`
}
```

Example:
```go
// GET endpoint returning HTML
tmpl := template.Must(template.New("page").Parse("<h1>{{.Message}}</h1>"))
producer := rest.ProducerFunc[PageData](func(ctx context.Context) (*PageData, error) {
    return &PageData{Message: "Hello"}, nil
})
rest.Handle(http.MethodGet, rest.BasePath("/"), rest.ProduceHTML(producer, tmpl))

// POST endpoint consuming form data and returning HTML fragment
itemTmpl := template.Must(template.New("item").Parse("<li>{{.Text}}</li>"))
handler := rest.HandlerFunc[FormRequest, ItemResponse](func(ctx context.Context, req *FormRequest) (*ItemResponse, error) {
    return &ItemResponse{Text: req.Text}, nil
})
rest.Handle(
    http.MethodPost,
    rest.BasePath("/add"),
    rest.ConsumeForm(rest.ReturnHTML(handler, itemTmpl)),
)
```

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

**RFC 7807 Problem Details:**

Humus supports [RFC 7807 Problem Details](https://tools.ietf.org/html/rfc7807) for standardized HTTP API error responses. This is an opt-in feature configured per operation using `rest.OnError()`.

```go
// Basic Problem Details error handler
handler := rest.NewProblemDetailsErrorHandler(
    rest.WithDefaultType("https://example.com/errors"),
)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rpc.HandleJson(createUserHandler),
    rest.OnError(handler),
)
```

**Custom errors with extension fields:**

Embed `rest.ProblemDetail` to create type-safe custom errors with additional fields:

```go
type ValidationError struct {
    rest.ProblemDetail
    ValidationErrors []FieldError `json:"validation_errors"`
}

type FieldError struct {
    Field   string `json:"field"`
    Message string `json:"message"`
}

func (e ValidationError) Error() string {
    return e.Detail
}

// Return from handler
func createUser(ctx context.Context, req *CreateUserRequest) (*User, error) {
    if req.Email == "" {
        return nil, ValidationError{
            ProblemDetail: rest.ProblemDetail{
                Type:     "https://example.com/errors/validation",
                Title:    "Validation Failed",
                Status:   http.StatusBadRequest,
                Detail:   "Request validation failed",
                Instance: "/users",
            },
            ValidationErrors: []FieldError{
                {Field: "email", Message: "Email is required"},
            },
        }
    }
    // ...
}
```

**Error detection hierarchy:**

The `ProblemDetailsErrorHandler` detects errors in this order:

1. **Custom errors embedding ProblemDetail** - Serialized directly with all fields (including extensions and explicit detail message)
2. **Framework errors** (`rest.BadRequestError`, `rest.UnauthorizedError`, etc.) - Converted to standard Problem Details with hardcoded detail message
3. **Generic errors** - Wrapped as 500 Internal Server Error with hardcoded detail message

**Security: Error Detail Protection**

For security, only errors embedding `ProblemDetail` include the actual error details. All other errors (framework errors and generic errors) use the hardcoded message "An internal server error occurred." to prevent leaking sensitive information like database connection strings, internal paths, or stack traces.

```go
// Custom errors with ProblemDetail include your explicit details
return nil, ValidationError{
    ProblemDetail: rest.ProblemDetail{
        Detail: "Request validation failed", // This detail IS included
        ...
    },
}

// Generic errors are automatically secured
return nil, errors.New("database failed: password=secret123")
// Response detail: "An internal server error occurred."
// The password is NOT leaked to the client
```

**Configuration options:**

```go
handler := rest.NewProblemDetailsErrorHandler(
    rest.WithDefaultType("https://api.example.com/errors"),
)
```

Available options:
- `rest.WithDefaultType(uri)` - Base URI for error types (defaults to "about:blank")

The logger is always set to `humus.Logger("rest")` and cannot be customized.

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

#### Queue Services

**Core Interfaces:**
- **Consumer[T]** - Retrieves messages from a queue (return `ErrEndOfQueue` when exhausted)
- **Processor[T]** - Executes business logic on messages
- **Acknowledger[T]** - Confirms successful processing back to the queue

**Delivery Semantics (implemented by Runtime):**

At-most-once: Consume → Acknowledge → Process
- Messages acknowledged before processing
- Processing failures result in message loss
- Suitable for non-critical data (metrics, logging, caching)

At-least-once: Consume → Process → Acknowledge
- Messages acknowledged only after successful processing
- Processing failures result in redelivery and retry
- Requires idempotent processors to handle duplicates
- Suitable for critical operations (financial transactions, database updates)

**Runtime Interface:**
```go
type Runtime interface {
    ProcessQueue(ctx context.Context) error
}
```

Implementations coordinate Consumer, Processor, and Acknowledger. Return `queue.ErrEndOfQueue` from Consumer when the queue is exhausted to trigger graceful shutdown.

**Kafka Runtime (queue/kafka):**
```go
// Basic Kafka runtime with goroutine-per-partition concurrency
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce(topic, processor),
)

// With mTLS authentication for secure broker connections
cert, err := tls.LoadX509KeyPair("client-cert.pem", "client-key.pem")
if err != nil {
    return err
}
caCert, err := os.ReadFile("ca-cert.pem")
if err != nil {
    return err
}
caCertPool := x509.NewCertPool()
if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
    return fmt.Errorf("failed to parse CA certificate")
}

tlsConfig := &tls.Config{
    Certificates: []tls.Certificate{cert},
    RootCAs:      caCertPool,
    MinVersion:   tls.VersionTLS12,
}

runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.WithTLS(tlsConfig),
    kafka.AtLeastOnce(topic, processor),
)

// Multiple topics with different processors
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", ordersProcessor),
    kafka.AtMostOnce("events", eventsProcessor),
)

// Advanced configuration with TLS, timeouts, and fetch settings
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.WithTLS(&tls.Config{
        Certificates: []tls.Certificate{cert},
        RootCAs:      caCertPool,
        ServerName:   "kafka.example.com",
        MinVersion:   tls.VersionTLS12,
    }),
    kafka.SessionTimeout(45 * time.Second),
    kafka.RebalanceTimeout(30 * time.Second),
    kafka.FetchMaxBytes(50 * 1024 * 1024),  // 50 MB
    kafka.MaxConcurrentFetches(10),
    kafka.AtLeastOnce(topic, processor),
)
```

**TLS Configuration:**
- Use standard `*tls.Config` from Go's `crypto/tls` package
- Load certificates with `tls.LoadX509KeyPair()` or `tls.X509KeyPair()` for in-memory data
- Configure CA pool with `x509.CertPool` for broker verification
- Supports both mTLS (client cert + CA) and TLS-only (CA only)

**Custom Runtime Example:**
```go
type MyRuntime struct {
    consumer     queue.Consumer[Message]
    processor    queue.Processor[Message]
    acknowledger queue.Acknowledger[Message]
}

func (r *MyRuntime) ProcessQueue(ctx context.Context) error {
    for {
        msg, err := r.consumer.Consume(ctx)
        if errors.Is(err, queue.ErrEndOfQueue) {
            return nil // Graceful shutdown
        }
        if err != nil {
            return err
        }

        // At-least-once: Process before Acknowledge
        if err := r.processor.Process(ctx, msg); err != nil {
            return err
        }

        if err := r.acknowledger.Acknowledge(ctx, msg); err != nil {
            return err
        }
    }
}
```

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

**REST:** (see detailed examples in REST Services section above)
```go
api := rest.NewApi("My Service", "v1.0.0")
rest.Handle(http.MethodPost, rest.BasePath("/users"), rpc.HandleJson(handlerFunc))
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

**Queue:**
```go
// Using Kafka runtime (recommended for Kafka)
runtime := kafka.NewAtLeastOnceRuntime(brokers, topic, groupID, processor)
app := queue.NewApp(runtime)

// Or custom runtime implementation (see Queue Services section above)
runtime := &MyRuntime{...}
app := queue.NewApp(runtime)
```

### Entry Point Pattern

All service types follow this pattern:
```go
func main() {
    rest.Run(configReader, app.Init)
    // or grpc.Run(configReader, app.Init)
    // or job.Run(configReader, app.Init)
    // or queue.Run(configReader, app.Init)
}
```

The initializer function receives config and returns the API/App:
```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Initialize your service
}

// For queue services:
func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    runtime := &MyRuntime{...}
    return queue.NewApp(runtime), nil
}
```

## Important Conventions

**CRITICAL: Read [.github/instructions/go.instructions.md](.github/instructions/go.instructions.md) before editing any Go code.** This file contains strict Go coding standards that must be followed, including:
- Package declaration rules (NEVER duplicate `package` declarations)
- Naming conventions for variables, functions, and interfaces
- Error handling patterns and propagation
- Concurrency guidelines

1. **Error Handling** - Use `rpc.ErrorHandler` interface for custom error responses in REST operations; set via `OnError()` operation option. For RFC 7807 compliant errors, use `rest.ProblemDetailsErrorHandler` with custom errors embedding `rest.ProblemDetail`
2. **Error Naming (ENFORCED)** - All error variables must follow the `ErrFoo` naming pattern (enforced by golangci-lint staticcheck ST1012):
   ```go
   // Correct
   var ErrNotFound = errors.New("not found")
   var ErrInvalidInput = errors.New("invalid input")

   // Wrong - will fail linting
   var NotFoundError = errors.New("not found")
   ```
3. **JWT Authentication** - Implement `rest.JWTVerifier` interface to verify tokens and inject claims into context; framework handles token extraction from "Bearer " header and returns 401 on failure
4. **OpenAPI Schemas** - Generated automatically via reflection from Go types (uses `github.com/swaggest/jsonschema-go`)
5. **Health Checks** - REST apps should implement custom health handlers via `rest.Readiness()` and `rest.Liveness()` options; gRPC health is automatic
6. **Graceful Shutdown** - Handled automatically by Bedrock lifecycle; no explicit cleanup needed in most cases
7. **OTel Instrumentation** - Automatic for HTTP (via otelhttp) and gRPC (via interceptors); use `otel.Tracer/Meter` directly in business logic
8. **Logging** - Use `humus.Logger(name)` to get an OpenTelemetry-integrated logger; returns `*slog.Logger`
9. **Option Pattern** - Used throughout for extensibility (e.g., `rest.ApiOption`, `grpc.RunOption`, `rpc.OperationOption`)
10. **Queue End-of-Queue Signal** - Queue consumers should return `queue.ErrEndOfQueue` when the queue is exhausted to trigger graceful shutdown; particularly useful for finite queues or batch processing

## Testing Patterns

Tests use standard Go testing with:
- `github.com/stretchr/testify` for assertions and mocking
- Race detection enabled in CI (`-race` flag)
- Example tests in `*_example_test.go` files demonstrate usage patterns

When writing tests:
- Use `testify/require` for assertions (fail-fast) rather than `testify/assert`
- Mock `context.Context` for handler tests
- Use `httptest.NewRecorder()` for HTTP handler tests
- Mock `grpc.ServiceRegistrar` for gRPC registration tests
- Test health monitors in isolation before composing
- For queue processors, use call order verification:
  - Create a `callRecorder` to track method invocations
  - Verify the correct sequence (e.g., Consume → Acknowledge → Process for at-most-once)
  - Test error handling at each phase independently

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
- `example/rest/petstore/` - REST API example with OpenAPI generation
- `example/rest/problem-details/` - RFC 7807 Problem Details error handling
- `example/rest/htmx/` - HTMX todo list with HTML templates and form handling
- `example/grpc/petstore/` - gRPC service example with health monitoring
- `example/queue/kafka-at-most-once/` - Kafka queue with at-most-once semantics
- `example/queue/kafka-at-least-once/` - Kafka queue with at-least-once semantics
- `example/queue/kafka-mtls-at-least-once/` - Kafka queue with mTLS authentication

Refer to these for real-world usage patterns of the framework.

## Common Pitfalls

- **Duplicate package declarations** - Each Go file must have exactly ONE `package` line (see [go.instructions.md](.github/instructions/go.instructions.md))
- **Using `assert` in tests** - Use `require` for fail-fast behavior, not `assert`
- **Hardcoding config values** - Use YAML templates: `{{env "VAR" | default "value"}}`
- **Bypassing lifecycle wrappers** - Manual server starts break OTel init and graceful shutdown
- **Non-idempotent at-least-once processors** - Must handle duplicate message delivery
- **Ignoring ErrEndOfQueue** - Queue consumers must return this for graceful shutdown
- **Wrong error variable names** - Use `ErrFoo` pattern, not `FooError` (enforced by linter)
