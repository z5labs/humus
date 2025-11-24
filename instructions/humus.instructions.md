# Humus Framework Instructions

This file provides best practices and patterns for building applications with the Humus framework. Copy this file to your repository's `.github/` or `instructions/` directory to provide context to your coding agent.

## Overview

Humus is a modular Go framework built on [Bedrock](https://github.com/z5labs/bedrock) for creating production-ready REST APIs, gRPC services, jobs, and queue processors. Every application automatically includes OpenTelemetry instrumentation, health monitoring, and graceful shutdown.

## Project Structure Patterns

### Simple Service (Single File)

Use for small services with minimal complexity:

```
my-service/
├── main.go              # Entry point with Run() call and Init function
├── config.yaml          # Configuration file
├── go.mod
├── go.sum
└── README.md
```

**main.go example:**
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
    api := rest.NewApi("My Service", "1.0.0")
    // Register handlers...
    return api, nil
}
```

### Organized Service (Recommended)

Use this structure for production services. This matches the examples in the Humus repository:

```
my-service/
├── main.go              # Minimal entry point (just calls app.Init)
├── config.yaml          # Configuration
├── app/
│   └── app.go          # Init function and Config type
├── endpoint/           # For REST services
│   ├── create_user.go
│   ├── get_user.go
│   └── list_users.go
├── go.mod
└── go.sum
```

**main.go:**
```go
package main

import (
    "bytes"
    _ "embed"
    "github.com/z5labs/humus/rest"
    "my-service/app"
)

//go:embed config.yaml
var configBytes []byte

func main() {
    rest.Run(bytes.NewReader(configBytes), app.Init)
}
```

**app/app.go:**
```go
package app

import (
    "context"
    "my-service/endpoint"
    "github.com/z5labs/humus/rest"
)

type Config struct {
    rest.Config `config:",squash"`
    // Add service-specific config here
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    api := rest.NewApi(
        cfg.OpenApi.Title,
        cfg.OpenApi.Version,
        endpoint.CreateUser(),
        endpoint.GetUser(),
        endpoint.ListUsers(),
    )
    return api, nil
}
```

### gRPC Service Structure

```
my-grpc-service/
├── main.go
├── config.yaml
├── app/
│   └── app.go          # Init function
├── pet/                # Domain package
│   └── registrar/
│       └── registrar.go  # Service registration
├── proto/              # Proto definitions
│   └── pet.proto
├── petpb/              # Generated protobuf code
│   ├── pet.pb.go
│   └── pet_grpc.pb.go
├── go.mod
└── go.sum
```

### Queue/Job Service Structure

```
my-queue-service/
├── main.go
├── config.yaml
├── app/
│   └── app.go          # Init function
├── processor/
│   └── processor.go    # Message processing logic
├── go.mod
└── go.sum
```

## Critical Patterns

### Configuration

**Always embed humus.Config:**
```go
type Config struct {
    humus.Config `config:",squash"`  // REQUIRED - provides OTel config
    
    // Service-specific configuration
    HTTP struct {
        Port uint `config:"port"`
    } `config:"http"`
}
```

**Implement provider interfaces when needed:**
```go
func (c Config) Listener(ctx context.Context) (net.Listener, error) {
    return net.Listen("tcp", fmt.Sprintf(":%d", c.HTTP.Port))
}
```

**Use Go templates in config.yaml:**
```yaml
openapi:
  title: {{env "SERVICE_NAME" | default "My Service"}}
  version: {{env "VERSION" | default "v1.0.0"}}

http:
  port: {{env "HTTP_PORT" | default "8080"}}
```

### REST Service Patterns

**Entry Point:**
```go
func main() {
    rest.Run(rest.YamlSource("config.yaml"), app.Init)
}
```

**Handler Registration (in app/app.go):**
```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    api := rest.NewApi(
        cfg.OpenApi.Title,
        cfg.OpenApi.Version,
        endpoint.CreateUser(),
        endpoint.GetUser(db),
        endpoint.ListUsers(db),
    )
    return api, nil
}
```

**Handler Types:**

1. **Producer (GET endpoints - no request body):**
```go
// endpoint/get_user.go
func GetUser(db *sql.DB) rest.Operation {
    handler := rpc.ProducerFunc[UserResponse](func(ctx context.Context) (*UserResponse, error) {
        // Fetch and return data
        return &UserResponse{ID: "123", Name: "John"}, nil
    })
    
    return rest.Handle(
        http.MethodGet,
        rest.BasePath("/users").Param("id"),
        rpc.ProduceJson(handler),
        rest.PathParam("id", rest.Required()),
    )
}
```

2. **Consumer (POST webhooks - no response body):**
```go
// endpoint/webhook.go
func Webhook() rest.Operation {
    handler := rpc.ConsumerFunc[WebhookRequest](func(ctx context.Context, req *WebhookRequest) error {
        // Process request
        return nil
    })
    
    return rest.Handle(
        http.MethodPost,
        rest.BasePath("/webhook"),
        rpc.ConsumeOnlyJson(handler),
    )
}
```

3. **Handler (full request/response):**
```go
// endpoint/create_user.go
func CreateUser(db *sql.DB) rest.Operation {
    handler := rpc.HandlerFunc[CreateUserRequest, UserResponse](
        func(ctx context.Context, req *CreateUserRequest) (*UserResponse, error) {
            // Create user
            return &UserResponse{ID: "123", Name: req.Name}, nil
        },
    )
    
    return rest.Handle(
        http.MethodPost,
        rest.BasePath("/users"),
        rpc.HandleJson(handler),  // Shorthand for ConsumeJson(ReturnJson(handler))
    )
}
```

**Path Building:**
```go
// Simple path
rest.BasePath("/users")

// Path with segments
rest.BasePath("/api").Segment("v1").Segment("users")

// Path with parameters
rest.BasePath("/users").Param("id")  // /users/{id}
```

**Parameter Options:**
```go
rest.Handle(method, path, handler,
    rest.QueryParam("format", rest.Required()),
    rest.PathParam("id", rest.Required()),
    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt")),
)
```

### gRPC Service Patterns

**Entry Point:**
```go
func main() {
    grpc.Run(grpc.YamlSource("config.yaml"), app.Init)
}
```

**Init Function:**
```go
func Init(ctx context.Context, cfg Config) (*grpc.Api, error) {
    api := grpc.NewApi()
    
    // Register your gRPC services
    registrar.Register(api, dependencies)
    
    return api, nil
}
```

**Service Registration:**
```go
// pet/registrar/registrar.go
func Register(api *grpc.Api, store Store) {
    svc := &service{store: store}
    petpb.RegisterPetServiceServer(api, svc)
}

type service struct {
    petpb.UnimplementedPetServiceServer
    store Store
}

func (s *service) CreatePet(ctx context.Context, req *petpb.CreatePetRequest) (*petpb.Pet, error) {
    // Implementation
    return &petpb.Pet{}, nil
}
```

### Queue Processing Patterns

**At-Most-Once Processing (fast, may lose messages):**
```go
// Process order: Consume → Acknowledge → Process
processor := queue.ProcessAtMostOnce(consumer, processor, acknowledger)
```

**At-Least-Once Processing (reliable, may duplicate):**
```go
// Process order: Consume → Process → Acknowledge
processor := queue.ProcessAtLeastOnce(consumer, processor, acknowledger)

// IMPORTANT: Your processor MUST be idempotent!
```

**Graceful Shutdown:**
```go
func (c *MyConsumer) Consume(ctx context.Context) (*Message, error) {
    select {
    case <-ctx.Done():
        return nil, queue.ErrEndOfQueue  // Signals graceful shutdown
    case msg := <-c.messages:
        return msg, nil
    }
}
```

**Kafka Runtime Example:**
```go
func Init(ctx context.Context, cfg Config) (queue.Runtime, error) {
    proc := processor.New()
    runtime := kafka.NewAtMostOnceRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.Topic,
        cfg.Kafka.GroupID,
        proc,
    )
    return runtime, nil
}
```

### Error Handling

**Operation-Level (REST handlers):**
```go
rest.Handle(method, path, handler,
    rest.OnError(rest.ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
        w.WriteHeader(http.StatusInternalServerError)
        json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
    })),
)
```

**Runner-Level:**
```go
runner := humus.NewRunner(builder, humus.OnError(humus.ErrorHandlerFunc(func(err error) {
    log.Fatal(err)
})))
```

### Logging & Observability

**Always use humus.Logger:**
```go
log := humus.Logger("service-name")
log.Info("user created", slog.String("user_id", userID))
log.Error("failed to process", slog.String("error", err.Error()))
```

**Logs automatically correlate with OpenTelemetry traces** - no manual instrumentation needed.

### Health Monitoring

**Binary Health Check:**
```go
monitor := new(health.Binary)
monitor.MarkHealthy()    // Service is healthy
monitor.MarkUnhealthy()  // Service is unhealthy
```

**Composite Monitors:**
```go
// Both must be healthy
health.And(dbMonitor, cacheMonitor)

// At least one must be healthy
health.Or(replica1Monitor, replica2Monitor)
```

## Best Practices

### DO ✅

1. **Keep main.go minimal** - just call `rest.Run()` or `grpc.Run()` with `app.Init`
2. **Embed configuration files** - use `//go:embed config.yaml` for portability
3. **Organize handlers in endpoint/ package** - one file per endpoint/operation
4. **Use the app/ package for Init function** - keeps business logic separate from main
5. **Embed humus.Config in custom Config** - required for OpenTelemetry
6. **Use Go templates in config.yaml** - `{{env "VAR" | default "value"}}`
7. **Return early to reduce nesting** - keep the happy path left-aligned
8. **Use rpc.HandleJson for full request/response** - it's shorthand for ConsumeJson(ReturnJson(handler))
9. **Test with `-race` flag** - catch concurrency issues early
10. **Make at-least-once processors idempotent** - they may process messages multiple times

### DON'T ❌

1. **Don't bypass lifecycle wrappers** - manually starting servers breaks OTel and graceful shutdown
2. **Don't put business logic in main.go** - use app/app.go and endpoint/ packages
3. **Don't ignore delivery semantics** - understand at-most-once vs at-least-once
4. **Don't hardcode configuration** - use environment variables with templates
5. **Don't change handler signatures** without understanding OpenAPI generation
6. **Don't duplicate package declarations** - each .go file has exactly ONE package line
7. **Don't use assert in tests** - use require.* to fail fast
8. **Don't forget to close resources** - use lifecycle hooks for cleanup
9. **Don't ignore errors** - always handle or propagate them
10. **Don't manually initialize OpenTelemetry** - Humus does this automatically

## Common Pitfalls

### Incorrect Config Embedding

❌ **Wrong:**
```go
type Config struct {
    Title string
    // Missing humus.Config embedding
}
```

✅ **Correct:**
```go
type Config struct {
    rest.Config `config:",squash"`  // or grpc.Config or humus.Config
    // Your config here
}
```

### Wrong Handler Pattern

❌ **Wrong (mixing raw http.Handler with rest.Handle):**
```go
api.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
    // Manual JSON marshaling, no OpenAPI generation
})
```

✅ **Correct:**
```go
func CreateUser() rest.Operation {
    handler := rpc.HandlerFunc[CreateUserRequest, UserResponse](
        func(ctx context.Context, req *CreateUserRequest) (*UserResponse, error) {
            return &UserResponse{}, nil
        },
    )
    return rest.Handle(http.MethodPost, rest.BasePath("/users"), rpc.HandleJson(handler))
}
```

### Not Using Lifecycle Hooks

❌ **Wrong (resource leak):**
```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db, _ := sql.Open("postgres", cfg.DB.URL)
    // db never gets closed!
    return api, nil
}
```

✅ **Correct:**
```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db, _ := sql.Open("postgres", cfg.DB.URL)
    
    lc, _ := lifecycle.FromContext(ctx)
    lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
        return db.Close()
    }))
    
    return api, nil
}
```

### Ignoring Queue Semantics

❌ **Wrong (non-idempotent at-least-once processor):**
```go
processor := queue.ProcessAtLeastOnce(consumer, func(ctx context.Context, msg *Message) error {
    balance += msg.Amount  // DANGER: Will double-count if message is reprocessed!
    return nil
}, acknowledger)
```

✅ **Correct (idempotent processor):**
```go
processor := queue.ProcessAtLeastOnce(consumer, func(ctx context.Context, msg *Message) error {
    // Check if already processed
    if alreadyProcessed(msg.ID) {
        return nil  // Skip duplicate
    }
    balance += msg.Amount
    markProcessed(msg.ID)
    return nil
}, acknowledger)
```

## Testing Patterns

### Table-Driven Tests

```go
func TestCreateUser(t *testing.T) {
    tests := []struct {
        name    string
        input   CreateUserRequest
        want    *UserResponse
        wantErr bool
    }{
        {
            name:  "valid user",
            input: CreateUserRequest{Name: "John"},
            want:  &UserResponse{ID: "123", Name: "John"},
        },
        {
            name:    "empty name",
            input:   CreateUserRequest{Name: ""},
            wantErr: true,
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got, err := handler(context.Background(), &tt.input)
            if tt.wantErr {
                require.Error(t, err)
                return
            }
            require.NoError(t, err)
            require.Equal(t, tt.want, got)
        })
    }
}
```

### Use testify/require (not assert)

❌ **Wrong:**
```go
assert.Equal(t, expected, actual)  // Test continues on failure
```

✅ **Correct:**
```go
require.Equal(t, expected, actual)  // Test stops immediately on failure
```

## OpenAPI Generation

REST handlers automatically generate OpenAPI 3.0 specifications:

- **Available at**: `/openapi.json`
- **Uses Go struct tags**: `json:"field_name"` tags define schema
- **Supports validation**: Use parameter options for required fields, regex patterns
- **Authentication**: Use `rest.JWTAuth()`, `rest.APIKeyAuth()`, `rest.BasicAuth()`

## Health Endpoints

All services automatically include health endpoints:

- **Liveness**: `/health/liveness` - Always returns 200 when server is running
- **Readiness**: `/health/readiness` - Returns 200 when service is ready (checks monitors)

## Quick Reference

| Pattern | Code |
|---------|------|
| REST path | `rest.BasePath("/api").Segment("v1").Param("id")` |
| Query param | `rest.QueryParam("name", rest.Required())` |
| Path param | `rest.PathParam("id", rest.Required())` |
| Header | `rest.Header("Authorization", rest.JWTAuth("jwt"))` |
| Producer handler | `rpc.ProduceJson(rpc.ProducerFunc[Response](...))` |
| Consumer handler | `rpc.ConsumeOnlyJson(rpc.ConsumerFunc[Request](...))` |
| Full handler | `rpc.HandleJson(rpc.HandlerFunc[Req, Resp](...))` |
| Health monitors | `health.And(m1, m2)`, `health.Or(m1, m2)` |
| Logger | `log := humus.Logger("service")` |

## Example Projects

Study these examples in the Humus repository:

- **REST API**: `example/rest/petstore/` - Complete REST service structure
- **gRPC Service**: `example/grpc/petstore/` - gRPC with health monitoring
- **Queue Processor**: `example/queue/kafka-at-most-once/` - Kafka message processing

## Additional Resources

- **Documentation**: https://z5labs.dev/humus/
- **Repository**: https://github.com/z5labs/humus
- **Bedrock Framework**: https://github.com/z5labs/bedrock
- **Examples**: https://github.com/z5labs/humus/tree/main/example

## Version Information

This instructions file is designed for Humus applications using:
- Go 1.24 or later
- Humus framework latest version

Keep this file updated as you add project-specific patterns and conventions.
