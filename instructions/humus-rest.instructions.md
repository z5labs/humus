---
description: 'Patterns and best practices for REST API applications using Humus'
applyTo: '**/*.go'
---

# Humus Framework - REST Service Instructions

This file provides patterns and best practices specific to REST API applications using Humus. Use this file alongside `humus-common.instructions.md` for complete guidance.

## Important: Package Merge (2025)

**The `rest/rpc` package has been merged into the `rest` package.** All handler types, interfaces, and functions are now directly in the `rest` package:

- Import `github.com/z5labs/humus/rest` (not `rest/rpc`)
- Use `rest.Operation()` instead of `rest.Handle()`
- Use `rest.ProduceJson()`, `rest.HandleJson()`, etc. (not `rpc.*`)
- Use `rest.Producer`, `rest.Consumer`, `rest.Handler` interfaces

## Project Structure

Use this structure for production services. This matches the examples in the Humus repository:

```
my-rest-service/
├── main.go              # Minimal entry point (just calls app.Init)
├── config.yaml          # Configuration
├── app/
│   └── app.go          # Init function and Config type
├── endpoint/           # REST endpoint handlers
│   ├── create_user.go
│   ├── get_user.go
│   └── list_users.go
├── service/            # Backend service clients
│   ├── user.go         # User service client
│   └── order.go        # Order service client
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
        endpoint.CreateUser(ctx),
        endpoint.GetUser(ctx),
        endpoint.ListUsers(ctx),
    )
    return api, nil
}
```

## Configuration

**Custom Config with provider interface:**

If you need to customize the HTTP server listener (e.g., custom port), implement the `ListenerProvider` interface:

```go
type Config struct {
    rest.Config `config:",squash"`
    
    HTTP struct {
        Port uint `config:"port"`
    } `config:"http"`
}

func (c Config) Listener(ctx context.Context) (net.Listener, error) {
    return net.Listen("tcp", fmt.Sprintf(":%d", c.HTTP.Port))
}
```

See `humus-common.instructions.md` for general configuration patterns like using Go templates in YAML and the backend service client pattern.

## REST Service Patterns

### Entry Point

The entry point should use embedded config bytes (see main.go in Project Structure above):

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

### Handler Types

Handlers should be implemented as struct types that implement the specific interface (`rest.Producer`, `rest.Consumer`, or `rest.Handler`).

#### 1. Producer (GET endpoints - no request body)

Implement the `rest.Producer` interface with a `Produce` method:

```go
// endpoint/list_users.go
package endpoint

import (
    "context"
    "database/sql"
    "log/slog"
    "net/http"

    "github.com/z5labs/bedrock/lifecycle"
    "github.com/z5labs/humus"
    "github.com/z5labs/humus/rest"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/trace"
)

type listUsersHandler struct {
    tracer        trace.Tracer
    log           *slog.Logger
    listUsersStmt *sql.Stmt
}

type ListUsersResponse []*User

func ListUsers(ctx context.Context, db *sql.DB) rest.ApiOption {
    stmt, err := db.Prepare("SELECT id, name FROM users LIMIT ?")
    if err != nil {
        panic(err)
    }

    lc, ok := lifecycle.FromContext(ctx)
    if !ok {
        panic("lifecycle must be present in context")
    }
    lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
        return stmt.Close()
    }))

    h := &listUsersHandler{
        tracer:        otel.Tracer("my-service/endpoint"),
        log:           humus.Logger("my-service/endpoint"),
        listUsersStmt: stmt,
    }

    return rest.Operation(
        http.MethodGet,
        rest.BasePath("/users"),
        rest.ProduceJson(h),
    )
}

func (h *listUsersHandler) Produce(ctx context.Context) (*ListUsersResponse, error) {
    // Implement query logic
    return nil, nil
}
```

#### 2. Consumer (POST webhooks - no response body)

Implement the `rest.Consumer` interface with a `Consume` method:

```go
// endpoint/webhook.go
package endpoint

type webhookHandler struct {
    tracer trace.Tracer
    log    *slog.Logger
}

type WebhookRequest struct {
    Event string `json:"event"`
    Data  any    `json:"data"`
}

func Webhook(ctx context.Context) rest.ApiOption {
    h := &webhookHandler{
        tracer: otel.Tracer("my-service/endpoint"),
        log:    humus.Logger("my-service/endpoint"),
    }

    return rest.Operation(
        http.MethodPost,
        rest.BasePath("/webhook"),
        rest.ConsumeOnlyJson(h),
    )
}

func (h *webhookHandler) Consume(ctx context.Context, req *WebhookRequest) error {
    // Process webhook
    return nil
}
```

#### 3. Handler (full request/response)

Implement the `rest.Handler` interface with a `Handle` method:

```go
// endpoint/create_user.go
package endpoint

type createUserHandler struct {
    tracer         trace.Tracer
    log            *slog.Logger
    createUserStmt *sql.Stmt
}

type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type CreateUserResponse struct {
    ID string `json:"id"`
}

func CreateUser(ctx context.Context, db *sql.DB) rest.ApiOption {
    stmt, err := db.Prepare("INSERT INTO users (name, email) VALUES (?, ?)")
    if err != nil {
        panic(err)
    }

    lc, ok := lifecycle.FromContext(ctx)
    if !ok {
        panic("lifecycle must be present in context")
    }
    lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
        return stmt.Close()
    }))

    h := &createUserHandler{
        tracer:         otel.Tracer("my-service/endpoint"),
        log:            humus.Logger("my-service/endpoint"),
        createUserStmt: stmt,
    }

    return rest.Operation(
        http.MethodPost,
        rest.BasePath("/users"),
        rest.HandleJson(h),
    )
}

func (h *createUserHandler) Handle(ctx context.Context, req *CreateUserRequest) (*CreateUserResponse, error) {
    // Create user
    return &CreateUserResponse{ID: "123"}, nil
}
```

### Path Building

```go
// Simple path
rest.BasePath("/users")

// Path with segments
rest.BasePath("/api").Segment("v1").Segment("users")

// Path with parameters
rest.BasePath("/users").Param("id")  // /users/{id}
```

### Parameter Options

```go
rest.Operation(method, path, handler,
    rest.QueryParam("format", rest.Required()),
    rest.PathParam("id", rest.Required()),
    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt")),
)
```

### Operation-Level Error Handling

```go
rest.Operation(method, path, handler,
    rest.OnError(rest.ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
        w.WriteHeader(http.StatusInternalServerError)
        json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
    })),
)
```

## OpenAPI Generation

REST handlers automatically generate OpenAPI 3.0 specifications:

- **Available at**: `/openapi.json`
- **Uses Go struct tags**: `json:"field_name"` tags define schema
- **Supports validation**: Use parameter options for required fields, regex patterns
- **Authentication**: Use `rest.JWTAuth()`, `rest.APIKeyAuth()`, `rest.BasicAuth()`

## Health Endpoints

All REST services automatically include health endpoints:

- **Liveness**: `/health/liveness` - Always returns 200 when server is running
- **Readiness**: `/health/readiness` - Returns 200 when service is ready (checks monitors)

## REST-Specific Best Practices

### DO ✅

1. **Organize handlers in endpoint/ package** - one file per endpoint/operation
2. **Use rest.HandleJson for full request/response** - it's shorthand for ConsumeJson(ReturnJson(handler))
3. **Use proper handler types** - Producer for GET, Consumer for webhooks, Handler for full request/response
4. **Leverage OpenAPI generation** - your handlers automatically generate documentation
5. **Use path building helpers** - `BasePath().Segment().Param()` for clarity

### DON'T ❌

1. **Don't change handler signatures** without understanding OpenAPI generation
2. **Don't mix raw http.Handler with rest.Operation** - use the typed handler wrappers
3. **Don't bypass the handler wrappers** - they provide type safety and OpenAPI generation
4. **Don't hardcode paths** - use the path building helpers
5. **Don't ignore parameter validation** - use Required(), regex patterns, etc.

## Common REST Pitfalls

### Wrong Handler Pattern

❌ **Wrong (mixing raw http.Handler):**
```go
api.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
    // Manual JSON marshaling, no OpenAPI generation
})
```

✅ **Correct:**
```go
func CreateUser() rest.ApiOption {
    handler := rest.HandlerFunc[CreateUserRequest, UserResponse](
        func(ctx context.Context, req *CreateUserRequest) (*UserResponse, error) {
            return &UserResponse{}, nil
        },
    )
    return rest.Operation(http.MethodPost, rest.BasePath("/users"), rest.HandleJson(handler))
}
```

## Quick Reference

| Pattern | Code |
|---------|------|
| REST operation | `rest.Operation(method, path, handler, ...options)` |
| REST path | `rest.BasePath("/api").Segment("v1").Param("id")` |
| Query param | `rest.QueryParam("name", rest.Required())` |
| Path param | `rest.PathParam("id", rest.Required())` |
| Header | `rest.Header("Authorization", rest.JWTAuth("jwt"))` |
| Producer handler | `rest.ProduceJson(rest.ProducerFunc[Response](...))` |
| Consumer handler | `rest.ConsumeOnlyJson(rest.ConsumerFunc[Request](...))` |
| Full handler | `rest.HandleJson(rest.HandlerFunc[Req, Resp](...))` |

## Example Project

Study this production-ready example in the Humus repository:

- **REST API**: [orders-walkthrough](https://github.com/z5labs/humus/tree/main/example/rest/orders-walkthrough) - Production-ready REST service structure with comprehensive patterns

## Additional Resources

- **REST Documentation**: https://z5labs.dev/humus/features/rest/
- **Authentication Guide**: https://z5labs.dev/humus/features/rest/authentication/
- **Common patterns**: See `humus-common.instructions.md`
