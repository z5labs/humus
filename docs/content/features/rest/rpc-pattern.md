---
title: RPC Pattern
description: Type-safe handler abstraction
weight: 30
type: docs
---

# RPC Pattern

The `rest/rpc` package provides a type-safe abstraction for HTTP handlers with automatic OpenAPI schema generation.

## Overview

Traditional HTTP handlers in Go use `http.HandlerFunc`:

```go
func(w http.ResponseWriter, r *http.Request)
```

Humus provides a type-safe alternative:

```go
rpc.Handler[Request, Response]
```

This provides:
- **Type Safety** - Compile-time type checking
- **Automatic Serialization** - JSON/XML/etc. handled automatically
- **OpenAPI Generation** - Schemas generated from Go types
- **Simplified Logic** - Focus on business logic, not HTTP plumbing

## Basic Usage

### Simple Handler

```go
handler := rpc.Handle(func(ctx context.Context, req string) (string, error) {
    return "Hello, " + req, nil
})

operation := rpc.NewOperation(handler)
rest.Handle(http.MethodGet, rest.BasePath("/greet"), operation)
```

### With JSON Serialization

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

handler := rpc.NewOperation(
    rpc.ConsumeJson(
        rpc.ReturnJson(
            rpc.Handle(func(ctx context.Context, req CreateUserRequest) (User, error) {
                user := User{
                    ID:    generateID(),
                    Name:  req.Name,
                    Email: req.Email,
                }
                return user, nil
            }),
        ),
    ),
)

rest.Handle(http.MethodPost, rest.BasePath("/users"), handler)
```

## The Handler Interface

```go
type Handler[Req, Resp any] interface {
    Handle(context.Context, Req) (Resp, error)
}
```

You can implement this interface directly:

```go
type UserService struct {
    db *Database
}

func (s *UserService) Handle(ctx context.Context, req CreateUserRequest) (User, error) {
    // Business logic here
    user, err := s.db.CreateUser(ctx, req.Name, req.Email)
    if err != nil {
        return User{}, err
    }
    return user, nil
}

// Register
service := &UserService{db: database}
operation := rpc.NewOperation(
    rpc.ConsumeJson(rpc.ReturnJson(service)),
)
rest.Handle(http.MethodPost, rest.BasePath("/users"), operation)
```

## Request Types

### Typed Requests

Define request structure with struct tags:

```go
type GetUserRequest struct {
    ID string `path:"id"`  // From URL path parameter
}

handler := rpc.Handle(func(ctx context.Context, req GetUserRequest) (User, error) {
    return getUserByID(ctx, req.ID)
})
```

### Query Parameters

```go
type ListUsersRequest struct {
    Limit  int    `query:"limit"`
    Offset int    `query:"offset"`
    Sort   string `query:"sort"`
}

handler := rpc.Handle(func(ctx context.Context, req ListUsersRequest) ([]User, error) {
    return listUsers(ctx, req.Limit, req.Offset, req.Sort)
})
```

### Headers

```go
type AuthenticatedRequest struct {
    Token   string `header:"Authorization"`
    UserID  string `path:"userId"`
}

handler := rpc.Handle(func(ctx context.Context, req AuthenticatedRequest) (Response, error) {
    if !validateToken(req.Token) {
        return Response{}, fmt.Errorf("unauthorized")
    }
    return processRequest(ctx, req.UserID)
})
```

### Mixed Sources

Combine path, query, headers, and body:

```go
type ComplexRequest struct {
    UserID string `path:"userId"`          // From /users/{userId}
    APIKey string `header:"X-API-Key"`     // From header
    Limit  int    `query:"limit"`          // From ?limit=10
    Filter Filter `json:"filter"`          // From JSON body
}
```

### Empty Request

For handlers that don't need input:

```go
handler := rpc.Handle(func(ctx context.Context, _ any) (Response, error) {
    return getDefaultResponse(), nil
})
```

## Response Types

### Typed Responses

Return any Go type:

```go
type UserResponse struct {
    ID        string    `json:"id"`
    Name      string    `json:"name"`
    CreatedAt time.Time `json:"created_at"`
}

handler := rpc.Handle(func(ctx context.Context, req GetUserRequest) (UserResponse, error) {
    return UserResponse{
        ID:        "user-123",
        Name:      "John Doe",
        CreatedAt: time.Now(),
    }, nil
})
```

### Collections

Return slices for lists:

```go
handler := rpc.Handle(func(ctx context.Context, _ any) ([]User, error) {
    return []User{
        {ID: "1", Name: "Alice"},
        {ID: "2", Name: "Bob"},
    }, nil
})
```

### Empty Response

Return empty type for operations with no response body:

```go
handler := rpc.Handle(func(ctx context.Context, req DeleteRequest) (any, error) {
    if err := deleteUser(ctx, req.ID); err != nil {
        return nil, err
    }
    return nil, nil  // 200 OK with empty body
})
```

Or return a simple string:

```go
handler := rpc.Handle(func(ctx context.Context, req DeleteRequest) (string, error) {
    if err := deleteUser(ctx, req.ID); err != nil {
        return "", err
    }
    return "deleted", nil
})
```

## Serialization

### ConsumeJson

Parse JSON request body:

```go
rpc.ConsumeJson(handler)
```

This wraps your handler to:
1. Read request body
2. Parse as JSON
3. Pass to your handler

### ReturnJson

Serialize response as JSON:

```go
rpc.ReturnJson(handler)
```

This wraps your handler to:
1. Take your response
2. Serialize to JSON
3. Write to response with `Content-Type: application/json`

### Chaining

Combine for full JSON I/O:

```go
operation := rpc.NewOperation(
    rpc.ConsumeJson(
        rpc.ReturnJson(
            rpc.Handle(yourHandler),
        ),
    ),
)
```

## Error Handling

Errors returned from handlers are automatically handled:

```go
handler := rpc.Handle(func(ctx context.Context, req Request) (Response, error) {
    if req.Invalid {
        return Response{}, fmt.Errorf("invalid request")
    }
    return Response{}, nil
})
```

Default behavior:
- Returns HTTP 500
- Body: `{"error": "invalid request"}`

See [Error Handling]({{< ref "error-handling" >}}) for customization.

## Operation Options

Customize operations with options:

```go
operation := rpc.NewOperation(
    handler,
    rpc.WithOperationID("createUser"),
    rpc.WithDescription("Creates a new user"),
    rpc.WithTags("users"),
)
```

Common options:
- `rpc.WithOperationID(id)` - Sets OpenAPI operation ID
- `rpc.WithDescription(desc)` - Adds description to OpenAPI
- `rpc.WithTags(tags...)` - Organizes operations in OpenAPI
- `rpc.OnError(errorHandler)` - Custom error handling

## Best Practices

### 1. Use Strong Types

```go
// Good
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Avoid
type Request map[string]interface{}
```

### 2. Implement Handler Interface

For complex logic, implement the interface:

```go
type UserService struct {
    db     *Database
    logger *slog.Logger
}

func (s *UserService) Handle(ctx context.Context, req CreateUserRequest) (User, error) {
    s.logger.InfoContext(ctx, "creating user", "email", req.Email)

    user, err := s.db.CreateUser(ctx, req)
    if err != nil {
        s.logger.ErrorContext(ctx, "failed to create user", "error", err)
        return User{}, err
    }

    return user, nil
}
```

### 3. Use Descriptive Names

```go
// Good
type CreateUserRequest struct { ... }
type UpdateUserRequest struct { ... }

// Less clear
type UserRequest1 struct { ... }
type UserRequest2 struct { ... }
```

### 4. Document with Tags

```go
type User struct {
    ID    string `json:"id" description:"Unique user identifier"`
    Name  string `json:"name" description:"User's full name"`
    Email string `json:"email" format:"email" description:"User's email address"`
}
```

These tags appear in the OpenAPI schema.

## Next Steps

- Learn about [Routing]({{< ref "routing" >}}) for path building
- Explore [Request/Response]({{< ref "request-response" >}}) for serialization details
- See [Error Handling]({{< ref "error-handling" >}}) for custom errors
- Read [OpenAPI]({{< ref "openapi" >}}) for schema generation
