---
description: 'Patterns and best practices for REST API applications using Humus'
applyTo: '**/*.go'
---

# Humus Framework - REST Service Instructions

This file provides patterns and best practices specific to REST API applications using Humus. Use this file alongside `humus-common.instructions.md` for complete guidance.

## Project Structure

### Simple Service (Single File)

Use for small services with minimal complexity:

```
my-rest-service/
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
my-rest-service/
├── main.go              # Minimal entry point (just calls app.Init)
├── config.yaml          # Configuration
├── app/
│   └── app.go          # Init function and Config type
├── endpoint/           # Handler functions
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

## REST Service Patterns

### Entry Point

```go
func main() {
    rest.Run(rest.YamlSource("config.yaml"), app.Init)
}
```

### Handler Types

#### 1. Producer (GET endpoints - no request body)

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

#### 2. Consumer (POST webhooks - no response body)

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

#### 3. Handler (full request/response)

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
rest.Handle(method, path, handler,
    rest.QueryParam("format", rest.Required()),
    rest.PathParam("id", rest.Required()),
    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt")),
)
```

### Operation-Level Error Handling

```go
rest.Handle(method, path, handler,
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

## REST-Specific Best Practices

### DO ✅

1. **Organize handlers in endpoint/ package** - one file per endpoint/operation
2. **Use rpc.HandleJson for full request/response** - it's shorthand for ConsumeJson(ReturnJson(handler))
3. **Use proper handler types** - Producer for GET, Consumer for webhooks, Handler for full request/response
4. **Leverage OpenAPI generation** - your handlers automatically generate documentation
5. **Use path building helpers** - `BasePath().Segment().Param()` for clarity

### DON'T ❌

1. **Don't change handler signatures** without understanding OpenAPI generation
2. **Don't mix raw http.Handler with rest.Handle** - use the rpc wrappers
3. **Don't bypass the rpc helpers** - they provide type safety and OpenAPI generation
4. **Don't hardcode paths** - use the path building helpers
5. **Don't ignore parameter validation** - use Required(), regex patterns, etc.

## Common REST Pitfalls

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

## Example Project

Study this example in the Humus repository:

- **REST API**: `example/rest/petstore/` - Complete REST service structure

## Additional Resources

- **REST Documentation**: https://z5labs.dev/humus/features/rest/
- **Authentication Guide**: https://z5labs.dev/humus/features/rest/authentication/
- **Common patterns**: See `humus-common.instructions.md`
