---
title: Handler Helpers
description: Type-safe handler creation with built-in serialization
weight: 30
type: docs
---


Humus provides handler helpers that simplify common REST API patterns by combining type-safe request/response handling with automatic serialization and OpenAPI schema generation.

## Overview

The REST package provides three categories of handler helpers:

1. **JSON Handlers** - For endpoints that consume and/or produce JSON
2. **Producer/Consumer Patterns** - For endpoints with only request or only response
3. **Low-level Wrappers** - For building custom serialization formats

## Core Interfaces

All handler helpers build on these core interfaces:

```go
// Handler processes a request and returns a response
type Handler[Req, Resp any] interface {
    Handle(context.Context, *Req) (*Resp, error)
}

// Producer returns a response without consuming a request
type Producer[T any] interface {
    Produce(context.Context) (*T, error)
}

// Consumer consumes a request without returning a response
type Consumer[T any] interface {
    Consume(context.Context, *T) error
}
```

## JSON Handlers

The most common handlers work with JSON payloads. These provide automatic serialization, content-type validation, and OpenAPI schema generation.

### HandleJson - Full Request and Response

Use `HandleJson` when your endpoint consumes and produces JSON:

```go
// Define your handler logic
handler := rest.HandlerFunc[CreateUserRequest, User](
    func(ctx context.Context, req *CreateUserRequest) (*User, error) {
        user := &User{
            ID:    generateID(),
            Name:  req.Name,
            Email: req.Email,
        }
        return user, nil
    },
)

// Wrap with JSON serialization
rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(handler),
)
```

**What happens automatically:**
- Request body parsed as JSON
- Content-Type validation (requires `application/json`)
- Response serialized as JSON with `Content-Type: application/json`
- OpenAPI request/response schemas generated from types

**OpenAPI Output:**
```json
{
  "requestBody": {
    "required": true,
    "content": {
      "application/json": {
        "schema": {
          "$ref": "#/components/schemas/CreateUserRequest"
        }
      }
    }
  },
  "responses": {
    "200": {
      "content": {
        "application/json": {
          "schema": {
            "$ref": "#/components/schemas/User"
          }
        }
      }
    }
  }
}
```

### ProduceJson - GET Endpoints

Use `ProduceJson` for GET endpoints that return data without consuming a request body:

```go
// Define your producer
producer := rest.ProducerFunc[[]User](
    func(ctx context.Context) (*[]User, error) {
        users := getUsersFromDB(ctx)
        return &users, nil
    },
)

// Wrap with JSON serialization
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users"),
    rest.ProduceJson(producer),
)
```

**What happens automatically:**
- No request body parsing
- Response serialized as JSON
- OpenAPI response schema generated

**Accessing Path/Query Parameters:**
```go
producer := rest.ProducerFunc[User](
    func(ctx context.Context) (*User, error) {
        // Extract parameters from context
        userID := rest.PathParamValue(ctx, "id")
        include := rest.QueryParamValue(ctx, "include")

        user := getUserByID(ctx, userID, include)
        return user, nil
    },
)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    rest.ProduceJson(producer),
    rest.QueryParam("include"),
)
```

### ConsumeOnlyJson - Webhook Endpoints

Use `ConsumeOnlyJson` for POST/PUT webhooks that process data but don't return content:

```go
// Define your consumer
consumer := rest.ConsumerFunc[WebhookPayload](
    func(ctx context.Context, payload *WebhookPayload) error {
        processWebhook(ctx, payload)
        return nil
    },
)

// Wrap with JSON deserialization
rest.Handle(
    http.MethodPost,
    rest.BasePath("/webhooks/github"),
    rest.ConsumeOnlyJson(consumer),
)
```

**What happens automatically:**
- Request body parsed as JSON
- Content-Type validation
- Returns `200 OK` with empty body on success
- Returns appropriate error status on failure

**Response behavior:**
```http
POST /webhooks/github HTTP/1.1
Content-Type: application/json

{"event": "push", "repository": "myrepo"}

HTTP/1.1 200 OK
Content-Length: 0
```

## Function Adapters

Handler helpers provide function adapters for inline handler definitions:

### HandlerFunc

Convert a function to a `Handler`:

```go
handler := rest.HandlerFunc[Request, Response](
    func(ctx context.Context, req *Request) (*Response, error) {
        // Process request and return response
        return &Response{}, nil
    },
)
```

### ProducerFunc

Convert a function to a `Producer`:

```go
producer := rest.ProducerFunc[Response](
    func(ctx context.Context) (*Response, error) {
        // Generate and return response
        return &Response{}, nil
    },
)
```

### ConsumerFunc

Convert a function to a `Consumer`:

```go
consumer := rest.ConsumerFunc[Request](
    func(ctx context.Context, req *Request) error {
        // Process request
        return nil
    },
)
```

## Composition Patterns

Handler helpers are designed to compose together, allowing you to build complex handlers from simple pieces.

### Adding Custom Middleware

Wrap handlers with additional behavior:

```go
// Base handler
baseHandler := rest.HandlerFunc[Request, Response](businessLogic)

// Add validation layer
validatingHandler := rest.HandlerFunc[Request, Response](
    func(ctx context.Context, req *Request) (*Response, error) {
        if err := validateRequest(req); err != nil {
            return nil, err
        }
        return baseHandler.Handle(ctx, req)
    },
)

// Wrap with JSON serialization
rest.Handle(
    http.MethodPost,
    rest.BasePath("/api"),
    rest.HandleJson(validatingHandler),
)
```

### Transforming Responses

Chain transformations before serialization:

```go
// Handler returns internal type
handler := rest.HandlerFunc[Request, InternalResponse](getInternalData)

// Transform to API response
transformer := rest.HandlerFunc[Request, ApiResponse](
    func(ctx context.Context, req *Request) (*ApiResponse, error) {
        internal, err := handler.Handle(ctx, req)
        if err != nil {
            return nil, err
        }
        return toApiResponse(internal), nil
    },
)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api"),
    rest.HandleJson(transformer),
)
```

## Low-Level Building Blocks

For custom serialization formats, use the underlying wrappers directly.

### ConsumeJson - Custom Request Deserialization

Wrap any handler to consume JSON requests:

```go
handler := rest.HandlerFunc[MyRequest, MyResponse](businessLogic)

// Add JSON request deserialization
jsonHandler := rest.ConsumeJson(handler)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api"),
    jsonHandler,
)
```

### ReturnJson - Custom Response Serialization

Wrap any handler to return JSON responses:

```go
handler := rest.HandlerFunc[MyRequest, MyResponse](businessLogic)

// Add JSON response serialization
jsonHandler := rest.ReturnJson(handler)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api"),
    jsonHandler,
)
```

### ConsumeNothing and ProduceNothing

Build handlers without request or response bodies:

```go
// Producer - generates response without request body
producer := rest.ProducerFunc[Response](generateData)
handler := rest.ConsumeNothing(producer)

// Consumer - processes request without response body
consumer := rest.ConsumerFunc[Request](processData)
handler := rest.ProduceNothing(consumer)
```

## Advanced Patterns

### Conditional Response Types

Return different response types based on business logic:

```go
handler := rest.HandlerFunc[Request, Response](
    func(ctx context.Context, req *Request) (*Response, error) {
        // Return different status codes via custom error types
        if !isAuthorized(ctx) {
            return nil, rest.UnauthorizedError{
                Cause: errors.New("invalid credentials"),
            }
        }

        if !exists(req.ID) {
            return nil, rest.NotFoundError{
                Cause: errors.New("resource not found"),
            }
        }

        return &Response{Data: getData(req.ID)}, nil
    },
)
```

See [Error Handling]({{< ref "error-handling" >}}) for complete error handling patterns.

### Streaming Responses

For streaming responses, implement custom `TypedResponse`:

```go
type StreamingResponse struct {
    data chan []byte
}

func (sr *StreamingResponse) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
    w.Header().Set("Content-Type", "application/x-ndjson")
    w.WriteHeader(http.StatusOK)

    for data := range sr.data {
        if _, err := w.Write(data); err != nil {
            return err
        }
        if f, ok := w.(http.Flusher); ok {
            f.Flush()
        }
    }
    return nil
}

func (sr *StreamingResponse) Spec() (int, openapi3.ResponseOrRef, error) {
    // Define OpenAPI spec for streaming response
    return http.StatusOK, openapi3.ResponseOrRef{}, nil
}
```

### Custom Content Types

Implement handlers for other content types:

```go
// XML request type
type XMLRequest[T any] struct {
    inner T
}

func (xr *XMLRequest[T]) ReadRequest(ctx context.Context, r *http.Request) error {
    contentType := r.Header.Get("Content-Type")
    if contentType != "application/xml" {
        return rest.BadRequestError{
            Cause: rest.InvalidContentTypeError{
                ContentType: contentType,
            },
        }
    }

    dec := xml.NewDecoder(r.Body)
    return dec.Decode(&xr.inner)
}

func (xr *XMLRequest[T]) Spec() (openapi3.RequestBodyOrRef, error) {
    // Define OpenAPI spec for XML request
    return openapi3.RequestBodyOrRef{}, nil
}
```

## Complete Example

Putting it all together in a CRUD API:

```go
type UserStore interface {
    Create(ctx context.Context, user User) error
    Get(ctx context.Context, id string) (*User, error)
    List(ctx context.Context) ([]User, error)
    Update(ctx context.Context, user User) error
    Delete(ctx context.Context, id string) error
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    store := NewUserStore()

    // POST /users - Create user
    createHandler := rest.HandlerFunc[CreateUserRequest, User](
        func(ctx context.Context, req *CreateUserRequest) (*User, error) {
            user := User{
                ID:    generateID(),
                Name:  req.Name,
                Email: req.Email,
            }
            if err := store.Create(ctx, user); err != nil {
                return nil, err
            }
            return &user, nil
        },
    )

    // GET /users - List all users
    listProducer := rest.ProducerFunc[[]User](
        func(ctx context.Context) (*[]User, error) {
            users, err := store.List(ctx)
            return &users, err
        },
    )

    // GET /users/{id} - Get single user
    getProducer := rest.ProducerFunc[User](
        func(ctx context.Context) (*User, error) {
            id := rest.PathParamValue(ctx, "id")
            return store.Get(ctx, id)
        },
    )

    // PUT /users/{id} - Update user
    updateHandler := rest.HandlerFunc[UpdateUserRequest, User](
        func(ctx context.Context, req *UpdateUserRequest) (*User, error) {
            id := rest.PathParamValue(ctx, "id")
            user := User{
                ID:    id,
                Name:  req.Name,
                Email: req.Email,
            }
            if err := store.Update(ctx, user); err != nil {
                return nil, err
            }
            return &user, nil
        },
    )

    // DELETE /users/{id} - Delete user
    deleteConsumer := rest.ConsumerFunc[struct{}](
        func(ctx context.Context, _ *struct{}) error {
            id := rest.PathParamValue(ctx, "id")
            return store.Delete(ctx, id)
        },
    )

    api := rest.NewApi(
        "User API",
        "1.0.0",
        rest.Handle(http.MethodPost, rest.BasePath("/users"), rest.HandleJson(createHandler)),
        rest.Handle(http.MethodGet, rest.BasePath("/users"), rest.ProduceJson(listProducer)),
        rest.Handle(http.MethodGet, rest.BasePath("/users").Param("id"), rest.ProduceJson(getProducer)),
        rest.Handle(http.MethodPut, rest.BasePath("/users").Param("id"), rest.HandleJson(updateHandler)),
        rest.Handle(http.MethodDelete, rest.BasePath("/users").Param("id"), rest.ConsumeOnlyJson(deleteConsumer)),
    )

    return api, nil
}
```

## Best Practices

### 1. Choose the Right Helper

Match the helper to your endpoint's behavior:

```go
// GET endpoints - ProduceJson
rest.ProduceJson(producer)

// Webhooks - ConsumeOnlyJson
rest.ConsumeOnlyJson(consumer)

// Full CRUD operations - HandleJson
rest.HandleJson(handler)
```

### 2. Keep Handlers Focused

Each handler should have a single responsibility:

```go
// Good - focused handler
handler := rest.HandlerFunc[CreateUserRequest, User](createUser)

// Avoid - handler doing too much
handler := rest.HandlerFunc[Request, Response](
    func(ctx context.Context, req *Request) (*Response, error) {
        validate(req)      // Should be middleware
        log(req)           // Should be middleware
        transform(req)     // Should be separate transformer
        return process(req), nil
    },
)
```

### 3. Use Type Parameters Effectively

Define clear request/response types:

```go
// Good - explicit types
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type UserResponse struct {
    ID        string    `json:"id"`
    Name      string    `json:"name"`
    Email     string    `json:"email"`
    CreatedAt time.Time `json:"created_at"`
}

// Avoid - using map[string]interface{}
handler := rest.HandlerFunc[map[string]interface{}, map[string]interface{}](...)
```

### 4. Leverage Function Adapters

Use function adapters for inline handlers:

```go
// Good - inline with adapter
rest.ProduceJson(rest.ProducerFunc[Response](
    func(ctx context.Context) (*Response, error) {
        return &Response{}, nil
    },
))

// Verbose - defining separate type
type MyProducer struct{}
func (p *MyProducer) Produce(ctx context.Context) (*Response, error) {
    return &Response{}, nil
}
rest.ProduceJson(&MyProducer{})
```

### 5. Document with JSON Tags

Use JSON tags to control serialization and OpenAPI schema generation:

```go
type User struct {
    ID        string    `json:"id"`                    // Required field
    Name      string    `json:"name"`                  // Required field
    Email     string    `json:"email,omitempty"`       // Optional field
    Internal  string    `json:"-"`                     // Excluded from JSON
    CreatedAt time.Time `json:"created_at"`            // Snake case in JSON
}
```

## Performance Considerations

### Request Body Size

JSON handlers buffer the entire request body in memory. For large uploads, consider:

```go
// Set max request size at the HTTP server level
config := rest.Config{
    HTTP: rest.HTTPConfig{
        MaxRequestBodySize: 10 * 1024 * 1024, // 10 MB
    },
}
```

### Response Streaming

For large responses, use custom streaming responses instead of buffering:

```go
// Avoid - buffers entire response
type LargeResponse struct {
    Data []LargeItem `json:"data"` // Could be GBs
}

// Prefer - streams data
type StreamingHandler struct{}

func (h *StreamingHandler) Handle(ctx context.Context, req *Request) (*StreamingResponse, error) {
    return &StreamingResponse{
        data: generateDataStream(ctx),
    }, nil
}
```

## Troubleshooting

### Content-Type Errors

If you see `400 Bad Request` with "invalid content type":

```json
{
  "error": "invalid content type: text/plain"
}
```

Ensure your client sends `Content-Type: application/json`:

```bash
curl -X POST http://localhost:8080/api \
  -H "Content-Type: application/json" \
  -d '{"key": "value"}'
```

### JSON Parsing Errors

If you see JSON unmarshal errors, verify your request structure matches the type:

```go
// Handler expects
type Request struct {
    Name string `json:"name"`
}

// Client must send
{"name": "value"}

// Not
{"Name": "value"}  // Wrong - Go field name instead of JSON tag
```

### Empty Response Body

`ConsumeOnlyJson` returns an empty response body by design:

```go
// This is correct behavior
rest.ConsumeOnlyJson(consumer)
// Returns: 200 OK with empty body

// To return data, use HandleJson instead
rest.HandleJson(handler)
// Returns: 200 OK with JSON response
```

## Next Steps

- Learn about [Error Handling]({{< ref "error-handling" >}}) for custom error responses
- Explore [Routing]({{< ref "routing" >}}) for path parameters and query strings
- See [OpenAPI]({{< ref "openapi" >}}) for customizing generated schemas
- Read [Authentication]({{< ref "authentication" >}}) for securing endpoints
