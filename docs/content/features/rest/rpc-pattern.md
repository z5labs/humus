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

Humus provides type-safe alternatives through three specialized functions:

- **`ProduceJson[T]`** - GET endpoints that return JSON (no request body)
- **`ConsumeOnlyJson[T]`** - POST/PUT endpoints that consume JSON (no response body)
- **`HandleJson[Req, Resp]`** - POST/PUT endpoints with JSON request and response

This provides:
- **Type Safety** - Compile-time type checking
- **Automatic Serialization** - JSON handled automatically
- **OpenAPI Generation** - Schemas generated from Go types
- **Simplified Logic** - Focus on business logic, not HTTP plumbing

## The Three Patterns

### ProduceJson - GET Endpoints

Use `ProduceJson` for endpoints that return data without consuming a request body:

```go
type ListUsersResponse struct {
    Users []User `json:"users"`
}

handler := rpc.ProducerFunc[ListUsersResponse](func(ctx context.Context) (*ListUsersResponse, error) {
    users, err := getUsers(ctx)
    if err != nil {
        return nil, err
    }
    return &ListUsersResponse{Users: users}, nil
})

rest.Handle(http.MethodGet, rest.BasePath("/users"), rpc.ProduceJson(handler))
```

### ConsumeOnlyJson - Webhook Endpoints

Use `ConsumeOnlyJson` for endpoints that process data without returning a response body:

```go
type WebhookPayload struct {
    Event string `json:"event"`
    Data  string `json:"data"`
}

handler := rpc.ConsumerFunc[WebhookPayload](func(ctx context.Context, req *WebhookPayload) error {
    // Process webhook
    return processWebhook(ctx, req.Event, req.Data)
})

rest.Handle(http.MethodPost, rest.BasePath("/webhooks"), rpc.ConsumeOnlyJson(handler))
```

### HandleJson - Full CRUD Endpoints

Use `HandleJson` for endpoints with both request and response bodies:

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

handler := rpc.HandlerFunc[CreateUserRequest, User](func(ctx context.Context, req *CreateUserRequest) (*User, error) {
    user := &User{
        ID:    generateID(),
        Name:  req.Name,
        Email: req.Email,
    }
    return user, nil
})

rest.Handle(http.MethodPost, rest.BasePath("/users"), rpc.HandleJson(handler))
```

## Handler Interfaces

### Handler[Req, Resp]

The main interface for handlers with both request and response:

```go
type Handler[Req, Resp any] interface {
    Handle(context.Context, *Req) (*Resp, error)
}
```

You can implement this interface directly on your types:

```go
type UserService struct {
    db *Database
}

func (s *UserService) Handle(ctx context.Context, req *CreateUserRequest) (*User, error) {
    // Business logic here
    user, err := s.db.CreateUser(ctx, req.Name, req.Email)
    if err != nil {
        return nil, err
    }
    return user, nil
}

// Register
service := &UserService{db: database}
rest.Handle(http.MethodPost, rest.BasePath("/users"), rpc.HandleJson(service))
```

### Producer[T]

For read-only endpoints that don't consume a request body:

```go
type Producer[T any] interface {
    Produce(context.Context) (*T, error)
}
```

Example implementation:

```go
type ListUsersService struct {
    db *Database
}

func (s *ListUsersService) Produce(ctx context.Context) (*ListUsersResponse, error) {
    users, err := s.db.ListUsers(ctx)
    if err != nil {
        return nil, err
    }
    return &ListUsersResponse{Users: users}, nil
}

// Register
service := &ListUsersService{db: database}
rest.Handle(http.MethodGet, rest.BasePath("/users"), rpc.ProduceJson(service))
```

### Consumer[T]

For write-only endpoints that don't return a response body:

```go
type Consumer[T any] interface {
    Consume(context.Context, *T) error
}
```

Example implementation:

```go
type WebhookService struct {
    processor *EventProcessor
}

func (s *WebhookService) Consume(ctx context.Context, req *WebhookPayload) error {
    return s.processor.Process(ctx, req.Event, req.Data)
}

// Register
service := &WebhookService{processor: eventProcessor}
rest.Handle(http.MethodPost, rest.BasePath("/webhooks"), rpc.ConsumeOnlyJson(service))
```

### Adapter Functions

For simple cases, use adapter functions instead of implementing interfaces:

- `HandlerFunc[Req, Resp]` - Wraps a function to implement `Handler[Req, Resp]`
- `ProducerFunc[T]` - Wraps a function to implement `Producer[T]`
- `ConsumerFunc[T]` - Wraps a function to implement `Consumer[T]`

```go
// Handler adapter
h := rpc.HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) {
    return &Response{}, nil
})

// Producer adapter
p := rpc.ProducerFunc[Response](func(ctx context.Context) (*Response, error) {
    return &Response{}, nil
})

// Consumer adapter
c := rpc.ConsumerFunc[Request](func(ctx context.Context, req *Request) error {
    return nil
})
```

## Integration with rest.Handle

All RPC handlers are registered using `rest.Handle()`:

```go
operation := rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rpc.HandleJson(handler),
    rest.QueryParam("format"),
    rest.Header("Authorization", rest.Required()),
)

api := rest.NewApi("My Service", "v1.0.0", operation)
```

The RPC handler automatically implements the `rest.Handler` interface, providing both HTTP handling and OpenAPI schema generation.

## Request Types

### Typed Requests

Define request structure with JSON tags:

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

handler := rpc.HandlerFunc[CreateUserRequest, User](func(ctx context.Context, req *CreateUserRequest) (*User, error) {
    return createUser(ctx, req.Name, req.Email)
})
```

### Accessing Query Parameters

Query parameters are accessed using the `rest` package, not through struct tags:

```go
handler := rpc.ProducerFunc[ListUsersResponse](func(ctx context.Context) (*ListUsersResponse, error) {
    // Access query parameter
    limitValues := rest.QueryParamValue(ctx, "limit")
    limit, err := strconv.Atoi(limitValues[0])
    if err != nil {
        return nil, err
    }

    return listUsers(ctx, limit)
})

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users"),
    rpc.ProduceJson(handler),
    rest.QueryParam("limit", rest.Required(), rest.Regex(regexp.MustCompile(`^\d+$`))),
)
```

### Accessing Path Parameters

Path parameters are also accessed via the `rest` package:

```go
handler := rpc.ProducerFunc[User](func(ctx context.Context) (*User, error) {
    // Access path parameter
    id := rest.PathParamValue(ctx, "id")
    return getUserByID(ctx, id)
})

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    rpc.ProduceJson(handler),
)
```

### Accessing Headers

Headers are accessed using standard `http.Request` from context or the `rest` package:

```go
handler := rpc.ProducerFunc[Response](func(ctx context.Context) (*Response, error) {
    // Access header
    token := rest.HeaderValue(ctx, "Authorization")
    if !validateToken(token) {
        return nil, fmt.Errorf("unauthorized")
    }
    return processRequest(ctx)
})

rest.Handle(
    http.MethodGet,
    rest.BasePath("/data"),
    rpc.ProduceJson(handler),
    rest.Header("Authorization", rest.Required()),
)
```

## Response Types

### Typed Responses

Return any Go type with JSON tags:

```go
type UserResponse struct {
    ID        string    `json:"id"`
    Name      string    `json:"name"`
    CreatedAt time.Time `json:"created_at"`
}

handler := rpc.ProducerFunc[UserResponse](func(ctx context.Context) (*UserResponse, error) {
    return &UserResponse{
        ID:        "user-123",
        Name:      "John Doe",
        CreatedAt: time.Now(),
    }, nil
})
```

### Collections

Return slices for lists:

```go
type User struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

type ListUsersResponse []User

handler := rpc.ProducerFunc[ListUsersResponse](func(ctx context.Context) (*ListUsersResponse, error) {
    users := &ListUsersResponse{
        {ID: "1", Name: "Alice"},
        {ID: "2", Name: "Bob"},
    }
    return users, nil
})
```

### Empty Response

Use `ConsumeOnlyJson` for operations with no response body:

```go
type DeleteRequest struct {
    ID string `json:"id"`
}

handler := rpc.ConsumerFunc[DeleteRequest](func(ctx context.Context, req *DeleteRequest) error {
    return deleteUser(ctx, req.ID)
})

rest.Handle(http.MethodDelete, rest.BasePath("/users"), rpc.ConsumeOnlyJson(handler))
```

This returns HTTP 200 OK with an empty response body.

## OpenAPI Schema Generation

All handlers automatically generate OpenAPI schemas from Go types:

```go
type CreateUserRequest struct {
    Name  string `json:"name" description:"User's full name"`
    Email string `json:"email" format:"email" description:"User's email address"`
}

type User struct {
    ID    string `json:"id" description:"Unique user identifier"`
    Name  string `json:"name"`
    Email string `json:"email" format:"email"`
}
```

The RPC package uses reflection to generate JSON schemas that are included in the OpenAPI specification. Struct tags like `description` and `format` are automatically recognized.

## Error Handling

Errors returned from handlers are automatically handled by the `rest` package:

```go
handler := rpc.HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) {
    if req.Invalid {
        return nil, fmt.Errorf("invalid request")
    }
    return &Response{}, nil
})
```

Default behavior:
- Returns HTTP 500
- Body: `{"error": "invalid request"}`

### Custom Error Handling

Configure custom error handlers using `rest.OnError` when registering the operation:

```go
errorHandler := func(ctx context.Context, w http.ResponseWriter, err error) {
    if errors.Is(err, ErrNotFound) {
        w.WriteHeader(http.StatusNotFound)
        json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
        return
    }
    w.WriteHeader(http.StatusInternalServerError)
    json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rpc.HandleJson(handler),
    rest.OnError(errorHandler),
)
```

## Operation Options

Customize operations using `rest.Handle` options:

```go
operation := rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rpc.HandleJson(handler),
    rest.WithOperationID("createUser"),
    rest.WithDescription("Creates a new user"),
    rest.WithTags("users"),
    rest.QueryParam("format"),
    rest.Header("Authorization", rest.Required()),
)
```

Common options:
- `rest.WithOperationID(id)` - Sets OpenAPI operation ID
- `rest.WithDescription(desc)` - Adds description to OpenAPI
- `rest.WithTags(tags...)` - Organizes operations in OpenAPI
- `rest.OnError(errorHandler)` - Custom error handling
- `rest.QueryParam(name, opts...)` - Define query parameters
- `rest.Header(name, opts...)` - Define required headers

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

### 2. Implement Interfaces on Service Structs

For complex logic with dependencies, implement interfaces on structs:

```go
type UserService struct {
    db     *Database
    logger *slog.Logger
    tracer trace.Tracer
}

func (s *UserService) Handle(ctx context.Context, req *CreateUserRequest) (*User, error) {
    s.logger.InfoContext(ctx, "creating user", "email", req.Email)

    user, err := s.db.CreateUser(ctx, req)
    if err != nil {
        s.logger.ErrorContext(ctx, "failed to create user", "error", err)
        return nil, err
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

### 5. Choose the Right Pattern

- **GET endpoints** → Use `ProduceJson` with `Producer[T]`
- **Webhooks/Events** → Use `ConsumeOnlyJson` with `Consumer[T]`
- **CRUD operations** → Use `HandleJson` with `Handler[Req, Resp]`

### 6. Use Pointer Signatures

All handler methods must use pointer types:

```go
// Correct
func (h *Handler) Handle(ctx context.Context, req *Request) (*Response, error)

// Incorrect - will not compile
func (h *Handler) Handle(ctx context.Context, req Request) (Response, error)
```

## Next Steps

- Learn about [Routing]({{< ref "routing" >}}) for path building
- Explore [Request/Response]({{< ref "request-response" >}}) for serialization details
- See [Error Handling]({{< ref "error-handling" >}}) for custom errors
- Read [OpenAPI]({{< ref "openapi" >}}) for schema generation
