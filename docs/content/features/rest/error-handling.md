---
title: Error Handling
description: Custom error responses and RFC 7807 Problem Details
weight: 60
type: docs
---

Humus provides flexible error handling for REST APIs with support for custom error responses and RFC 7807 Problem Details.

## Overview

Error handling in Humus follows a hierarchical approach:

1. **Default Behavior** - Logs errors and returns appropriate HTTP status codes
2. **Custom Error Handlers** - Implement `rest.ErrorHandler` for custom error responses
3. **RFC 7807 Problem Details** - Standardized error format with extension fields

## Default Error Handling

By default, Humus logs all errors and returns HTTP status codes without response bodies:

```go
rest.Operation(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(handler),
    // No OnError option = uses default error handler
)
```

**Default behavior:**
- Errors implementing `rest.HttpResponseWriter` control their own HTTP response
- Framework errors (`rest.BadRequestError`, `rest.UnauthorizedError`) return appropriate status codes
- All other errors return 500 Internal Server Error
- All errors are logged using the configured logger

## Custom Error Handlers

Implement the `rest.ErrorHandler` interface to customize error responses:

```go
type ErrorHandler interface {
    OnError(ctx context.Context, w http.ResponseWriter, err error)
}
```

### Example: JSON Error Response

```go
type jsonErrorHandler struct {
    includeDetails bool
}

func (h *jsonErrorHandler) OnError(ctx context.Context, w http.ResponseWriter, err error) {
    response := map[string]string{
        "error": "An error occurred",
    }

    if h.includeDetails {
        response["detail"] = err.Error()
    }

    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusInternalServerError)
    json.NewEncoder(w).Encode(response)
}

// Use the custom error handler
rest.Operation(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(handler),
    rest.OnError(&jsonErrorHandler{includeDetails: true}),
)
```

### ErrorHandler Function Adapter

Use `rest.ErrorHandlerFunc` to create error handlers from functions:

```go
rest.Operation(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(handler),
    rest.OnError(rest.ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusInternalServerError)
        json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
    })),
)
```

## RFC 7807 Problem Details

Humus provides built-in support for [RFC 7807 Problem Details](https://tools.ietf.org/html/rfc7807), a standardized format for HTTP API error responses.

### Basic Usage

```go
handler := rest.NewProblemDetailsErrorHandler(
    rest.WithDefaultType("https://api.example.com/errors"),
)

rest.Operation(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(createUserHandler),
    rest.OnError(handler),
)
```

**Example error response:**
```json
{
  "type": "https://api.example.com/errors/400",
  "title": "Bad Request",
  "status": 400,
  "detail": "Invalid email format"
}
```

### Custom Errors with Extension Fields

Create type-safe custom errors by embedding `rest.ProblemDetail`:

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
                Type:     "https://api.example.com/errors/validation",
                Title:    "Validation Failed",
                Status:   http.StatusBadRequest,
                Detail:   "Request validation failed",
                Instance: "/users",
            },
            ValidationErrors: []FieldError{
                {Field: "email", Message: "Email is required"},
                {Field: "name", Message: "Name must be at least 3 characters"},
            },
        }
    }
    return &User{}, nil
}
```

**Response:**
```json
{
  "type": "https://api.example.com/errors/validation",
  "title": "Validation Failed",
  "status": 400,
  "detail": "Request validation failed",
  "instance": "/users",
  "validation_errors": [
    {"field": "email", "message": "Email is required"},
    {"field": "name", "message": "Name must be at least 3 characters"}
  ]
}
```

### Configuration Options

#### WithDefaultType

Base URI for error types. Appended to the status code if the error doesn't set a Type:

```go
handler := rest.NewProblemDetailsErrorHandler(
    rest.WithDefaultType("https://api.example.com/errors"),
)
// Error without Type field will become: "https://api.example.com/errors/500"
```

### Security: Error Detail Protection

For security, the Problem Details handler automatically protects against leaking sensitive internal error information:

- **Errors embedding `ProblemDetail`** - Include the actual error details you explicitly set
- **Framework errors** - Use hardcoded detail message: "An internal server error occurred."
- **Generic errors** - Use hardcoded detail message: "An internal server error occurred."

This prevents accidentally exposing sensitive information like database connection strings, internal paths, or stack traces to API clients.

```go
// Custom errors with ProblemDetail will include your explicit details
type ValidationError struct {
    rest.ProblemDetail
    Errors map[string][]string `json:"errors"`
}

return nil, ValidationError{
    ProblemDetail: rest.ProblemDetail{
        Detail: "Request validation failed", // This detail IS included
    },
}

// Generic errors are automatically secured
return nil, errors.New("database failed: password=secret123")
// Response detail will be: "An internal server error occurred."
// The password is NOT leaked to the client
```

### Error Detection Hierarchy

The `ProblemDetailsErrorHandler` detects errors in this order:

1. **Custom errors embedding ProblemDetail** - Serialized directly with all fields (includes your explicit detail message)
2. **Framework errors** (`rest.BadRequestError`, `rest.UnauthorizedError`, etc.) - Converted to standard Problem Details with hardcoded detail
3. **Generic errors** - Wrapped as 500 Internal Server Error with hardcoded detail

```go
// Priority 1: Custom error with ProblemDetail (returns full object with extensions and your explicit detail)
return nil, ValidationError{
    ProblemDetail: rest.ProblemDetail{
        Detail: "Request validation failed", // Your explicit detail IS included
        ...
    },
    ValidationErrors: []FieldError{...},
}

// Priority 2: Framework error (converted to Problem Details with hardcoded detail)
return nil, rest.BadRequestError{Message: "Invalid input"}
// Returns: {"type":"about:blank","title":"Bad Request","status":400,"detail":"An internal server error occurred."}

// Priority 3: Generic error (wrapped as 500 with hardcoded detail)
return nil, errors.New("database connection failed")
// Returns: {"type":"about:blank","title":"Internal Server Error","status":500,"detail":"An internal server error occurred."}
```

## Framework Error Types

Humus provides built-in error types that implement `rest.HttpResponseWriter`:

### BadRequestError

Returns HTTP 400 Bad Request:

```go
return nil, rest.BadRequestError{Message: "Invalid email format"}
```

**Specialized bad request errors:**

- `rest.MissingRequiredParameterError` - Missing required parameter (query, path, header)
- `rest.InvalidParameterValueError` - Invalid parameter value
- `rest.InvalidContentTypeError` - Unsupported Content-Type
- `rest.InvalidJWTError` - Invalid JWT token

### UnauthorizedError

Returns HTTP 401 Unauthorized:

```go
return nil, rest.UnauthorizedError{Message: "Invalid credentials"}
```

## Common Error Patterns

### Validation Errors

```go
type ValidationError struct {
    rest.ProblemDetail
    Errors map[string][]string `json:"errors"`
}

func newValidationError(errors map[string][]string) ValidationError {
    return ValidationError{
        ProblemDetail: rest.ProblemDetail{
            Type:   "https://api.example.com/errors/validation",
            Title:  "Validation Failed",
            Status: http.StatusBadRequest,
            Detail: "One or more validation errors occurred",
        },
        Errors: errors,
    }
}

// Usage
if len(validationErrors) > 0 {
    return nil, newValidationError(validationErrors)
}
```

### Not Found Errors

```go
type NotFoundError struct {
    rest.ProblemDetail
    ResourceType string `json:"resource_type"`
    ResourceID   string `json:"resource_id"`
}

func newNotFoundError(resourceType, resourceID string) NotFoundError {
    return NotFoundError{
        ProblemDetail: rest.ProblemDetail{
            Type:   "https://api.example.com/errors/not-found",
            Title:  "Resource Not Found",
            Status: http.StatusNotFound,
            Detail: fmt.Sprintf("%s with ID %s not found", resourceType, resourceID),
        },
        ResourceType: resourceType,
        ResourceID:   resourceID,
    }
}
```

### Rate Limiting Errors

```go
type RateLimitError struct {
    rest.ProblemDetail
    RetryAfter int    `json:"retry_after"`
    Limit      int    `json:"limit"`
    Window     string `json:"window"`
}

func newRateLimitError(retryAfter, limit int, window string) RateLimitError {
    return RateLimitError{
        ProblemDetail: rest.ProblemDetail{
            Type:   "https://api.example.com/errors/rate-limit",
            Title:  "Rate Limit Exceeded",
            Status: http.StatusTooManyRequests,
            Detail: fmt.Sprintf("Rate limit of %d requests per %s exceeded", limit, window),
        },
        RetryAfter: retryAfter,
        Limit:      limit,
        Window:     window,
    }
}
```

### Conflict Errors

```go
type ConflictError struct {
    rest.ProblemDetail
    ConflictingField string `json:"conflicting_field"`
    ExistingValue    string `json:"existing_value"`
}

func newConflictError(field, value string) ConflictError {
    return ConflictError{
        ProblemDetail: rest.ProblemDetail{
            Type:   "https://api.example.com/errors/conflict",
            Title:  "Resource Conflict",
            Status: http.StatusConflict,
            Detail: fmt.Sprintf("A resource with %s=%s already exists", field, value),
        },
        ConflictingField: field,
        ExistingValue:    value,
    }
}
```

## Best Practices

### Define Error Type Constants

Define error type URIs as constants for consistency:

```go
const (
    ErrTypeValidation = "https://api.example.com/errors/validation"
    ErrTypeNotFound   = "https://api.example.com/errors/not-found"
    ErrTypeRateLimit  = "https://api.example.com/errors/rate-limit"
    ErrTypeConflict   = "https://api.example.com/errors/conflict"
)

type ValidationError struct {
    rest.ProblemDetail
    Errors map[string][]string `json:"errors"`
}

func newValidationError(errors map[string][]string) ValidationError {
    return ValidationError{
        ProblemDetail: rest.ProblemDetail{
            Type:   ErrTypeValidation, // Use constant
            Title:  "Validation Failed",
            Status: http.StatusBadRequest,
        },
        Errors: errors,
    }
}
```

### Use Constructor Functions

Encapsulate error creation logic in constructor functions:

```go
func newNotFoundError(resourceType, resourceID string) NotFoundError {
    return NotFoundError{
        ProblemDetail: rest.ProblemDetail{
            Type:   ErrTypeNotFound,
            Title:  "Resource Not Found",
            Status: http.StatusNotFound,
            Detail: fmt.Sprintf("%s with ID %s not found", resourceType, resourceID),
        },
        ResourceType: resourceType,
        ResourceID:   resourceID,
    }
}

// Usage
user, err := db.GetUser(userID)
if err != nil {
    return nil, newNotFoundError("User", userID)
}
```

### Implement Error() Method

Always implement the `Error()` method for custom error types:

```go
type ValidationError struct {
    rest.ProblemDetail
    Errors map[string][]string `json:"errors"`
}

func (e ValidationError) Error() string {
    return e.Detail // Or construct custom message
}
```

### Use Explicit ProblemDetail for User-Facing Errors

For errors that should provide meaningful details to users, always use custom errors embedding `ProblemDetail`:

```go
// Good - explicit, controlled error details
type ValidationError struct {
    rest.ProblemDetail
    Errors map[string][]string `json:"errors"`
}

return nil, ValidationError{
    ProblemDetail: rest.ProblemDetail{
        Type:   "https://api.example.com/errors/validation",
        Title:  "Validation Failed",
        Status: http.StatusBadRequest,
        Detail: "Request validation failed", // Explicit, safe detail
    },
    Errors: validationErrors,
}

// Bad - generic error may contain sensitive info
return nil, fmt.Errorf("validation failed: %v", internalError)
// Response: "detail": "An internal server error occurred."
```

### Use Extension Fields for Structured Data

Add extension fields beyond the RFC 7807 standard fields for rich error information:

```go
type ValidationError struct {
    rest.ProblemDetail
    ValidationErrors []FieldError       `json:"validation_errors"` // Extension field
    Timestamp        time.Time          `json:"timestamp"`         // Extension field
    RequestID        string             `json:"request_id"`        // Extension field
}
```

## Complete Example

```go
package endpoint

import (
    "context"
    "net/http"
    "github.com/z5labs/humus/rest"
)

const (
    ErrTypeValidation = "https://api.example.com/errors/validation"
    ErrTypeNotFound   = "https://api.example.com/errors/not-found"
)

type ValidationError struct {
    rest.ProblemDetail
    Errors map[string][]string `json:"errors"`
}

func (e ValidationError) Error() string {
    return e.Detail
}

type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type CreateUserResponse struct {
    ID string `json:"id"`
}

type createUserHandler struct {
    // dependencies
}

func CreateUser(ctx context.Context) rest.ApiOption {
    handler := &createUserHandler{}

    // Configure Problem Details error handler
    errorHandler := rest.NewProblemDetailsErrorHandler(
        rest.WithDefaultType("https://api.example.com/errors"),
    )

    return rest.Operation(
        http.MethodPost,
        rest.BasePath("/users"),
        rest.HandleJson(handler),
        rest.OnError(errorHandler),
    )
}

func (h *createUserHandler) Handle(ctx context.Context, req *CreateUserRequest) (*CreateUserResponse, error) {
    // Validate request
    validationErrors := make(map[string][]string)
    if req.Name == "" {
        validationErrors["name"] = []string{"Name is required"}
    }
    if req.Email == "" {
        validationErrors["email"] = []string{"Email is required"}
    }

    if len(validationErrors) > 0 {
        return nil, ValidationError{
            ProblemDetail: rest.ProblemDetail{
                Type:     ErrTypeValidation,
                Title:    "Validation Failed",
                Status:   http.StatusBadRequest,
                Detail:   "Request validation failed",
                Instance: "/users",
            },
            Errors: validationErrors,
        }
    }

    // Create user...
    return &CreateUserResponse{ID: "123"}, nil
}
```

## See Also

- [RFC 7807 Problem Details Specification](https://tools.ietf.org/html/rfc7807)
- [API Reference](https://pkg.go.dev/github.com/z5labs/humus/rest)
- [Problem Details Example](https://github.com/z5labs/humus/tree/main/example/rest/problem-details)
