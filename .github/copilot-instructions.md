## Repository Overview

Humus is a modular Go framework for building production-ready REST APIs, gRPC services, jobs, and queue processors on top of [Bedrock](https://github.com/z5labs/bedrock). All applications include automatic OpenTelemetry instrumentation, health monitoring, and graceful shutdown.

**Core Architecture: Builder + Runner Pattern**
- Every service type has a `Builder[T Configer]` that returns `bedrock.AppBuilder[T]`
- Builders wrap apps with: `appbuilder.LifecycleContext(appbuilder.OTel(appbuilder.Recover(...)))`
- `humus.Runner` orchestrates: config read → app build → app run with pluggable `ErrorHandler`
- Package-level `Run()` functions (e.g., `rest.Run`, `queue.Run`) combine builder+runner+defaults for simplicity

**Package Structure:**
```
rest/         - REST/HTTP APIs with OpenAPI 3.0 generation
  rpc/        - Type-safe request/response handlers (ProduceJson, ConsumeJson, HandleJson)
grpc/         - gRPC services with auto-instrumentation
job/          - One-off job executors
queue/        - Message queue processors (at-most-once / at-least-once semantics)
  kafka/      - Kafka implementation with goroutine-per-partition concurrency
health/       - Health monitoring abstractions (Binary, AndMonitor, OrMonitor)
config/       - OpenTelemetry configuration schemas
internal/     - Framework implementation (otel, httpserver, grpcserver, run)
```

## Essential Files

**Framework Core:**
- `humus.go` - `Logger`, `Runner`, `OnError`, `ErrorHandler` patterns
- `default_config.yaml` - OTel defaults with Go template functions (`env`, `default`)
- `config/otel.go` - Complete OTel config structure (Resource, Trace, Metric, Log)
- `.github/instructions/go.instructions.md` - Strict Go coding standards (read before editing)

**Service Implementations:**
- `rest/rest.go` - REST builder, `ListenerProvider`, `HttpServerProvider`, `Config` embedding
- `rest/handle.go` - `Handle()` registration, `OperationOption`, middleware pattern
- `rest/path.go` - Path building (`BasePath("/x").Segment("y").Param("id")`)
- `rest/rpc/json.go` - `ReturnJson`, `ConsumeJson`, OpenAPI schema generation
- `grpc/grpc.go` - gRPC builder with auto-registration of health service
- `queue/queue.go` - `ProcessAtMostOnce`, `ProcessAtLeastOnce`, `ErrEndOfQueue`
- `queue/kafka/kafka.go` - Kafka runtime with partition-level concurrency

**Examples (authoritative):**
- `example/rest/petstore/` - Full REST API with routes, handlers, config
- `example/grpc/petstore/` - gRPC service with health monitoring
- `example/queue/kafka-at-most-once/`, `example/queue/kafka-at-least-once/`

## Developer Workflows

**Build & Test:**
```bash
go build ./...                      # Build all packages (CI requirement)
go test -race -cover ./...          # All tests with race detection
go test -race -cover ./rest/rpc     # Targeted package testing
golangci-lint run                   # Lint (uses .golangci.yaml: timeout=5m, tests=false)
```

**CI Pipelines:** `.github/workflows/` - `build.yaml` (lint + test), `codeql.yaml`, `docs.yaml`, `coverage.yaml`

**Run Examples:**
```bash
cd example/rest/petstore && go run .
cd example/queue/kafka-at-most-once && go run .
```

## Critical Code Patterns

### Configuration
```go
// Custom config embeds humus.Config and implements provider interfaces
type Config struct {
    humus.Config `config:",squash"`
    HTTP struct { Port uint } `config:"http"`
}

func (c Config) Listener(ctx context.Context) (net.Listener, error) {
    return net.Listen("tcp", fmt.Sprintf(":%d", c.HTTP.Port))
}

// YAML supports Go templates: {{env "VAR" | default "value"}}
// Always use bedrockcfg.MultiSource for composition
```

### REST Entry Point
```go
func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)  // Or bytes.NewReader(embedBytes)
}

func Init(ctx context.Context, cfg rest.Config) (*rest.Api, error) {
    api := rest.NewApi(cfg.OpenApi.Title, cfg.OpenApi.Version)
    
    // Register handlers using rest.Handle + rpc helpers
    rest.Handle(http.MethodGet, rest.BasePath("/users").Param("id"), 
        rpc.ProduceJson(getUserHandler),  // GET - only response
        rest.QueryParam("format", rest.Required()),
    )
    
    rest.Handle(http.MethodPost, rest.BasePath("/users"),
        rpc.HandleJson(createUserHandler),  // POST - request + response
    )
    
    rest.Handle(http.MethodPost, rest.BasePath("/webhook"),
        rpc.ConsumeOnlyJson(webhookHandler),  // POST - only request
    )
    
    return api, nil
}
```

### RPC Handler Types
```go
// Producer (GET endpoints - no request body)
rpc.ProducerFunc[Response](func(ctx context.Context) (*Response, error) { ... })
// Wrap with: rpc.ProduceJson(producer)

// Consumer (POST webhooks - no response body)
rpc.ConsumerFunc[Request](func(ctx context.Context, req *Request) error { ... })
// Wrap with: rpc.ConsumeOnlyJson(consumer)

// Handler (full request/response)
rpc.HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) { ... })
// Wrap with: rpc.HandleJson(handler) -- shorthand for ConsumeJson(ReturnJson(handler))
```

### Queue Processing
```go
// At-most-once: Consume → Acknowledge → Process (fast, may lose messages)
processor := queue.ProcessAtMostOnce(consumer, processor, acknowledger)

// At-least-once: Consume → Process → Acknowledge (reliable, may duplicate)
processor := queue.ProcessAtLeastOnce(consumer, processor, acknowledger)

// Signal graceful shutdown by returning ErrEndOfQueue from Consumer
func (c *MyConsumer) Consume(ctx context.Context) (*Message, error) {
    if noMoreMessages { return nil, queue.ErrEndOfQueue }
    // ...
}
```

### Error Handling
```go
// Runner-level (custom behavior on build/run errors)
runner := humus.NewRunner(builder, humus.OnError(humus.ErrorHandlerFunc(func(err error) {
    log.Fatal(err)  // Or custom reporting
})))

// Operation-level (REST handlers)
rest.Handle(method, path, handler, 
    rest.OnError(rest.ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
        // Custom error response
    })),
)
```

### Logging & Observability
```go
log := humus.Logger("service-name")  // Auto-integrated with OTel traces
log.Info("message", slog.String("key", "value"))

// Logs automatically correlate with traces when using OTel-instrumented code
// All builders automatically initialize OTel SDK from config (no manual setup)
```

## Strict Requirements

**Testing:**
- Use `testify/require` (NOT `assert`) - see `rest/rest_test.go` for patterns
- All tests must pass with `-race` flag
- Table-driven tests preferred for multiple scenarios

**Naming:**
- Error variables: `var ErrFooBar = errors.New("...")` (enforced by staticcheck ST1012)
- Package names: lowercase, singular, no underscores (e.g., `rest` not `rest_api`)

**Go Conventions:**
- Follow `.github/instructions/go.instructions.md` STRICTLY
- NEVER duplicate `package` declarations in files
- Return early to reduce nesting (happy path left-aligned)
- Make zero values useful

**Configuration:**
- All YAML configs use Go templates with `env` and `default` functions
- Default configs are embedded via `//go:embed default_config.yaml`
- Custom configs MUST embed `humus.Config` with `` `config:",squash"` `` tag

## Common Pitfalls

❌ **Don't bypass lifecycle wrappers** - manually starting servers breaks OTel init and graceful shutdown  
❌ **Don't ignore delivery semantics** - at-least-once processors MUST be idempotent  
❌ **Don't change handler signatures** without updating OpenAPI generation in `rest/rpc`  
❌ **Don't use `assert`** in tests - use `require` to fail fast  
❌ **Don't hardcode config values** - use YAML templates: `{{env "VAR" | default "value"}}`

## Quick Reference

**Path Building:** `rest.BasePath("/api").Segment("v1").Segment("users").Param("id")`  
**Parameters:** `rest.QueryParam("name", rest.Required())`, `rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt"))`  
**Health Monitors:** `health.And(monitor1, monitor2)`, `health.Or(monitor1, monitor2)`, `new(health.Binary).MarkHealthy()`  
**Kafka Runtime:** `kafka.NewAtMostOnceRuntime(brokers, topic, groupID, processor)` (goroutine-per-partition model)

For detailed examples of any pattern, check `example/` or ask for specific clarification on: REST handlers, gRPC services, queue runtimes, OTel config, or CI workflows.
