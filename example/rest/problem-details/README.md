# Problem Details Example

This example demonstrates RFC 7807 Problem Details error handling in a Humus REST API.

## Overview

This is a simple user management API that showcases:

- **Custom error types** with extension fields (ValidationError, NotFoundError, ConflictError)
- **ProblemDetailsErrorHandler** configuration per endpoint
- **Type-safe error responses** using struct embedding
- **In-memory data store** for demonstration purposes

## Running the Example

```bash
# From this directory
go run .
```

The API will start on port 8080 (or the port configured in config.yaml).

## Endpoints

### Create User

```bash
POST /users
Content-Type: application/json

{
  "name": "John Doe",
  "email": "john@example.com"
}
```

**Success Response (201):**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "John Doe",
  "email": "john@example.com"
}
```

**Validation Error Response (400):**
```json
{
  "type": "https://api.example.com/errors/validation",
  "title": "Validation Failed",
  "status": 400,
  "detail": "One or more validation errors occurred",
  "errors": {
    "name": ["Name is required"],
    "email": ["Email format is invalid"]
  }
}
```

**Conflict Error Response (409):**
```json
{
  "type": "https://api.example.com/errors/conflict",
  "title": "Resource Conflict",
  "status": 409,
  "detail": "A resource with email=john@example.com already exists",
  "conflicting_field": "email",
  "existing_value": "john@example.com"
}
```

### Get User

```bash
GET /users/{id}
```

**Success Response (200):**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "John Doe",
  "email": "john@example.com"
}
```

**Not Found Error Response (404):**
```json
{
  "type": "https://api.example.com/errors/not-found",
  "title": "Resource Not Found",
  "status": 404,
  "detail": "User with ID 550e8400-e29b-41d4-a716-446655440000 not found",
  "resource_type": "User",
  "resource_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

### List Users

```bash
GET /users
```

**Response (200):**
```json
{
  "users": [
    {
      "id": "550e8400-e29b-41d4-a716-446655440000",
      "name": "John Doe",
      "email": "john@example.com"
    }
  ]
}
```

## Example Usage with curl

```bash
# Create a user
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{"name":"John Doe","email":"john@example.com"}'

# Create a user with validation errors
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{"name":"","email":"invalid-email"}'

# Create a duplicate user (conflict error)
curl -X POST http://localhost:8080/users \
  -H "Content-Type: application/json" \
  -d '{"name":"John Doe","email":"john@example.com"}'

# Get a user
curl http://localhost:8080/users/{id}

# Get a non-existent user (not found error)
curl http://localhost:8080/users/00000000-0000-0000-0000-000000000000

# List all users
curl http://localhost:8080/users
```

## OpenAPI Documentation

View the OpenAPI specification at:
```
http://localhost:8080/openapi.json
```

## Key Implementation Details

### Custom Error Types

Custom error types are defined in `endpoint/errors.go`:

```go
type ValidationError struct {
    rest.ProblemDetail
    Errors map[string][]string `json:"errors"`
}
```

By embedding `rest.ProblemDetail`, the error type automatically implements:
- The `error` interface (via ProblemDetail.Error())
- A private marker interface (detected by ProblemDetailsErrorHandler)

### Error Handler Configuration

Each endpoint configures the Problem Details error handler:

```go
errorHandler := rest.NewProblemDetailsErrorHandler(
    rest.WithDefaultType("https://api.example.com/errors"),
)

return rest.Operation(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(h),
    rest.OnError(errorHandler),
)
```

**Security Note**: Generic errors (not embedding `rest.ProblemDetail`) automatically use a hardcoded detail message to prevent leaking sensitive information. Only custom errors with explicit `ProblemDetail` fields will include your specified error details.

### Constructor Functions

Constructor functions encapsulate error creation:

```go
func newValidationError(errors map[string][]string) ValidationError {
    return ValidationError{
        ProblemDetail: rest.ProblemDetail{
            Type:   ErrTypeValidation,
            Title:  "Validation Failed",
            Status: http.StatusBadRequest,
            Detail: "One or more validation errors occurred",
        },
        Errors: errors,
    }
}
```

### In-Memory Store

The `endpoint/store.go` file provides a simple in-memory user store with:
- Thread-safe operations using `sync.RWMutex`
- Email uniqueness enforcement (returns ConflictError on duplicates)
- User lookups (returns NotFoundError for missing users)

## Production Considerations

1. **Use Explicit ProblemDetail Errors**: Always use custom errors embedding `rest.ProblemDetail` for user-facing errors with controlled detail messages
2. **Use a Real Database**: Replace the in-memory store with a proper database
3. **Add Authentication**: Implement JWT or API key authentication
4. **Add Rate Limiting**: Protect endpoints from abuse
5. **Configure Logging**: Use structured logging for production monitoring

## See Also

- [RFC 7807 Specification](https://tools.ietf.org/html/rfc7807)
- [Humus Error Handling Documentation](https://z5labs.dev/humus/features/rest/error-handling/)
- [Humus REST Documentation](https://z5labs.dev/humus/features/rest/)
