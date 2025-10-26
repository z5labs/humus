---
title: Routing
description: Paths and parameters
weight: 40
type: docs
---

# Routing

Humus REST routing provides flexible path building and comprehensive parameter validation with automatic OpenAPI documentation generation.

## Path Building

Paths are constructed using `rest.BasePath()` and chained parameter methods.

### Static Paths

Simple static routes:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users"),
    listUsersHandler,
)
// Matches: GET /users
```

### Path Parameters

Add dynamic segments using `.Param()`:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    getUserHandler,
)
// Matches: GET /users/{id}
// Examples: /users/123, /users/abc
```

Access path parameters in your handler:

```go
handler := rpc.ProducerFunc[User](func(ctx context.Context) (*User, error) {
    userID := rest.PathParamValue(ctx, "id")
    return getUserByID(ctx, userID)
})
```

### Nested Paths

Chain multiple path segments:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id").Path("posts").Param("postId"),
    getPostHandler,
)
// Matches: GET /users/{id}/posts/{postId}
// Example: /users/123/posts/456
```

### Path Options

Path parameters support validation options:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id", rest.Regex(regexp.MustCompile(`^\d+$`))),
    getUserHandler,
)
// Only matches numeric IDs: /users/123
// Rejects: /users/abc (returns 400 Bad Request)
```

## Parameter Validation

Parameters can be validated using various options that apply to headers, query parameters, cookies, and path parameters.

### Required Parameters

Ensure parameters are present:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/search"),
    searchHandler,
    rest.QueryParam("q", rest.Required()),
)
```

Missing required parameters return `400 Bad Request`:

```json
{
  "error": "missing required request parameter in query: q"
}
```

### Regular Expression Validation

Validate parameter format:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users"),
    handler,
    rest.QueryParam("page", rest.Regex(regexp.MustCompile(`^\d+$`))),
    rest.QueryParam("email", rest.Regex(regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`))),
)
```

Invalid formats return `400 Bad Request`:

```json
{
  "error": "invalid parameter value in query: page"
}
```

### Combining Validators

Chain multiple validators:

```go
rest.QueryParam(
    "api_key",
    rest.Required(),
    rest.Regex(regexp.MustCompile(`^[a-f0-9]{32}$`)),
)
```

Validators run in order. The first failure stops validation and returns an error.

## Query Parameters

Query parameters are extracted from the URL query string.

### Basic Usage

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/search"),
    handler,
    rest.QueryParam("q"),
    rest.QueryParam("limit"),
    rest.QueryParam("offset"),
)
```

Access in handler:

```go
handler := rpc.ProducerFunc[Results](func(ctx context.Context) (*Results, error) {
    query := rest.QueryParamValue(ctx, "q")[0]
    limit := rest.QueryParamValue(ctx, "limit")[0]

    return search(ctx, query, limit)
})
```

### Multiple Values

Query parameters can have multiple values:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/filter"),
    handler,
    rest.QueryParam("tag"), // Allows multiple values
)
```

Request: `GET /filter?tag=go&tag=rest&tag=api`

```go
handler := rpc.ProducerFunc[Results](func(ctx context.Context) (*Results, error) {
    tags := rest.QueryParamValue(ctx, "tag")
    // tags = []string{"go", "rest", "api"}

    return filterByTags(ctx, tags)
})
```

### With Validation

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/data"),
    handler,
    rest.QueryParam("page", rest.Required(), rest.Regex(regexp.MustCompile(`^\d+$`))),
    rest.QueryParam("limit", rest.Regex(regexp.MustCompile(`^\d+$`))),
)
```

## Headers

Headers are used for metadata, authentication, and content negotiation.

### Basic Usage

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/data"),
    handler,
    rest.Header("Accept-Language"),
    rest.Header("X-Request-ID", rest.Required()),
)
```

Access in handler:

```go
handler := rpc.ProducerFunc[Data](func(ctx context.Context) (*Data, error) {
    language := rest.HeaderValue(ctx, "Accept-Language")[0]
    requestID := rest.HeaderValue(ctx, "X-Request-ID")[0]

    return getData(ctx, language, requestID)
})
```

### Authentication Headers

For authentication, use dedicated auth options instead of manual validation:

```go
rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    handler,
    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", verifier)),
)
```

See [Authentication]({{< ref "authentication" >}}) for complete authentication examples.

### Custom Validation

```go
rest.Handle(
    http.MethodPost,
    rest.BasePath("/webhooks"),
    handler,
    rest.Header("X-Signature", rest.Required(), rest.Regex(regexp.MustCompile(`^sha256=[a-f0-9]{64}$`))),
)
```

## Cookies

Cookies are extracted from the Cookie header.

### Basic Usage

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/dashboard"),
    handler,
    rest.Cookie("session", rest.Required()),
)
```

Access in handler:

```go
handler := rpc.ProducerFunc[Dashboard](func(ctx context.Context) (*Dashboard, error) {
    cookies := rest.CookieValue(ctx, "session")
    sessionID := cookies[0].Value

    return getDashboard(ctx, sessionID)
})
```

### With Validation

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/app"),
    handler,
    rest.Cookie("session_id", rest.Required(), rest.Regex(regexp.MustCompile(`^[a-f0-9]{64}$`))),
)
```

### Cookie Authentication

Cookies can be used for API key authentication:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api"),
    handler,
    rest.Cookie("api_key", rest.Required(), rest.APIKey("cookie-auth")),
)
```

## OpenAPI Integration

All parameters are automatically documented in the OpenAPI specification:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    handler,
    rest.QueryParam("include", rest.Regex(regexp.MustCompile(`^(profile|posts|comments)$`))),
    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", verifier)),
)
```

Generates OpenAPI spec:

```json
{
  "paths": {
    "/users/{id}": {
      "get": {
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true
          },
          {
            "name": "include",
            "in": "query",
            "schema": {
              "type": "string",
              "pattern": "^(profile|posts|comments)$"
            }
          },
          {
            "name": "Authorization",
            "in": "header",
            "required": true
          }
        ],
        "security": [
          {"jwt": []}
        ]
      }
    }
  }
}
```

## Best Practices

### 1. Use Path Parameters for Resources

```go
// Good - resource identifiers in path
rest.BasePath("/users").Param("id")

// Avoid - identifiers in query string
rest.BasePath("/users") + rest.QueryParam("id")
```

### 2. Use Query Parameters for Filtering and Options

```go
// Good - filtering/pagination via query
rest.QueryParam("page")
rest.QueryParam("filter")
rest.QueryParam("sort")
```

### 3. Validate Early

Apply validation at the parameter level, not in business logic:

```go
// Good
rest.QueryParam("limit", rest.Required(), rest.Regex(regexp.MustCompile(`^\d+$`)))

// Avoid - validation in handler
handler := func(ctx context.Context) (*Results, error) {
    limit := rest.QueryParamValue(ctx, "limit")[0]
    if !isNumeric(limit) {
        return nil, errors.New("invalid limit")
    }
    // ...
}
```

### 4. Use Typed Context Keys

Avoid string collisions when storing values in context:

```go
type contextKey string

const userIDKey contextKey = "user_id"

// Set
ctx = context.WithValue(ctx, userIDKey, "123")

// Get with type safety
userID, ok := ctx.Value(userIDKey).(string)
```

### 5. Document with Examples

Use descriptive parameter names and add OpenAPI descriptions:

```go
// Clear parameter names
rest.QueryParam("items_per_page")  // Not just "limit"
rest.QueryParam("sort_order")      // Not just "order"
```

### 6. Use Authentication Options

For authentication, always use the built-in auth options:

```go
// Good
rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", verifier))

// Avoid - manual token parsing
rest.Header("Authorization", rest.Required())
// ... then parse JWT in handler
```

See [Authentication]({{< ref "authentication" >}}) for proper authentication patterns.

## Common Patterns

### Pagination

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/items"),
    handler,
    rest.QueryParam("page", rest.Regex(regexp.MustCompile(`^\d+$`))),
    rest.QueryParam("limit", rest.Regex(regexp.MustCompile(`^\d+$`))),
)
```

### Filtering

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/products"),
    handler,
    rest.QueryParam("category"),
    rest.QueryParam("min_price", rest.Regex(regexp.MustCompile(`^\d+(\.\d{2})?$`))),
    rest.QueryParam("max_price", rest.Regex(regexp.MustCompile(`^\d+(\.\d{2})?$`))),
)
```

### Sorting

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users"),
    handler,
    rest.QueryParam("sort", rest.Regex(regexp.MustCompile(`^(name|email|created_at)$`))),
    rest.QueryParam("order", rest.Regex(regexp.MustCompile(`^(asc|desc)$`))),
)
```

### Nested Resources

```go
// GET /organizations/{orgId}/teams/{teamId}/members
rest.Handle(
    http.MethodGet,
    rest.BasePath("/organizations").
        Param("orgId").
        Path("teams").
        Param("teamId").
        Path("members"),
    handler,
)
```

### Versioned APIs

```go
// URL versioning
rest.BasePath("/api/v1/users")
rest.BasePath("/api/v2/users")

// Or header versioning
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/users"),
    handler,
    rest.Header("API-Version", rest.Required(), rest.Regex(regexp.MustCompile(`^v\d+$`))),
)
```

## Next Steps

- Learn about [Authentication]({{< ref "authentication" >}}) for securing your routes
- Explore [RPC Pattern]({{< ref "rpc-pattern" >}}) for type-safe handlers
- See [OpenAPI]({{< ref "openapi" >}}) for customizing generated documentation
- Read [Error Handling]({{< ref "error-handling" >}}) for parameter validation errors
