---
title: Interceptors
description: Operation-level request and response processing
weight: 50
type: docs
---

Interceptors provide operation-level request and response processing in Humus REST services. They allow you to execute custom logic before your handler runs, making them ideal for cross-cutting concerns like logging, authentication, metrics collection, and request transformation.

## Overview

Interceptors operate at the operation level, meaning they're configured per-endpoint using the `rest.Intercept()` option. Unlike global middleware, interceptors:

- Are applied to specific operations via `rest.Handle()` options
- Use an error-returning signature that integrates with the operation's error handling
- Can modify the request, inspect the response, or short-circuit execution
- Have access to the full request/response lifecycle
- Execute in the order they were registered

## Core Concepts

### ServerInterceptor Interface

The `ServerInterceptor` interface defines a single method:

```go
type ServerInterceptor interface {
    Intercept(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error
}
```

The `Intercept` method receives the next handler in the chain and returns a new handler function. This allows interceptors to:

- Execute code before calling `next`
- Execute code after calling `next`
- Conditionally call `next` (or skip it entirely)
- Modify the request before passing it to `next`
- Handle errors returned by `next`

### ServerInterceptorFunc Adapter

For convenience, Humus provides `ServerInterceptorFunc` to create interceptors from functions:

```go
type ServerInterceptorFunc func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error
```

This allows you to define interceptors inline without creating a new type.

### Error-Returning Signature

Interceptors work with handlers that return errors:

```go
func(http.ResponseWriter, *http.Request) error
```

When an interceptor (or the handler it wraps) returns an error:
1. The error propagates up through the interceptor chain
2. The operation's error handler processes it (configured via `rest.OnError()`)
3. An appropriate HTTP response is sent to the client

This design integrates interceptors seamlessly with Humus's error handling system.

## Basic Usage

### Simple Header Injection

Add a custom header to all responses:

```go
interceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        // Set header before calling next handler
        w.Header().Set("X-Service-Version", "1.0.0")
        
        // Call the next handler in the chain
        return next(w, r)
    }
})

rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/data"),
    handler,
    rest.Intercept(interceptor),
)
```

### Request Logging

Log request details:

```go
loggingInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        start := time.Now()
        
        // Log request
        log := humus.Logger("api")
        log.InfoContext(r.Context(), "incoming request",
            slog.String("method", r.Method),
            slog.String("path", r.URL.Path),
            slog.String("remote_addr", r.RemoteAddr),
        )
        
        // Call next handler
        err := next(w, r)
        
        // Log response
        duration := time.Since(start)
        if err != nil {
            log.ErrorContext(r.Context(), "request failed",
                slog.Duration("duration", duration),
                slog.Any("error", err),
            )
        } else {
            log.InfoContext(r.Context(), "request completed",
                slog.Duration("duration", duration),
            )
        }
        
        return err
    }
})

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/users"),
    createUserHandler,
    rest.Intercept(loggingInterceptor),
)
```

## Common Use Cases

### Authentication

Verify authentication and inject user context:

```go
func authInterceptor(tokenValidator TokenValidator) rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            // Extract token from header
            authHeader := r.Header.Get("Authorization")
            if authHeader == "" {
                return rest.UnauthorizedError{Message: "missing authorization header"}
            }
            
            // Validate token
            userID, err := tokenValidator.Validate(r.Context(), authHeader)
            if err != nil {
                return rest.UnauthorizedError{Message: "invalid token"}
            }
            
            // Inject user ID into context
            ctx := context.WithValue(r.Context(), "user_id", userID)
            
            // Continue with enriched context
            return next(w, r.WithContext(ctx))
        }
    })
}

// Usage
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/profile"),
    profileHandler,
    rest.Intercept(authInterceptor(myTokenValidator)),
)
```

### Request ID Generation

Generate and propagate request IDs:

```go
requestIDInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        // Check for existing request ID
        requestID := r.Header.Get("X-Request-ID")
        if requestID == "" {
            requestID = generateRequestID()
        }
        
        // Inject into context
        ctx := context.WithValue(r.Context(), "request_id", requestID)
        
        // Add to response headers
        w.Header().Set("X-Request-ID", requestID)
        
        // Continue with enriched context
        return next(w, r.WithContext(ctx))
    }
})

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/orders"),
    createOrderHandler,
    rest.Intercept(requestIDInterceptor),
)
```

### Rate Limiting

Implement rate limiting per endpoint:

```go
func rateLimitInterceptor(limiter RateLimiter) rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            // Check rate limit
            allowed, retryAfter := limiter.Allow(r.RemoteAddr)
            if !allowed {
                w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
                return rest.BadRequestError{
                    Message: "rate limit exceeded",
                }
            }
            
            // Continue processing
            return next(w, r)
        }
    })
}

// Usage
rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/expensive-operation"),
    handler,
    rest.Intercept(rateLimitInterceptor(myRateLimiter)),
)
```

### Custom Metrics

Collect operation-specific metrics:

```go
func metricsInterceptor(meter metric.Meter) rest.ServerInterceptor {
    requestCounter, _ := meter.Int64Counter("http.server.requests")
    requestDuration, _ := meter.Float64Histogram("http.server.duration")
    
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            start := time.Now()
            
            // Increment request counter
            requestCounter.Add(r.Context(), 1,
                metric.WithAttributes(
                    attribute.String("method", r.Method),
                    attribute.String("path", r.URL.Path),
                ),
            )
            
            // Call next handler
            err := next(w, r)
            
            // Record duration
            duration := time.Since(start).Seconds()
            status := "success"
            if err != nil {
                status = "error"
            }
            
            requestDuration.Record(r.Context(), duration,
                metric.WithAttributes(
                    attribute.String("method", r.Method),
                    attribute.String("path", r.URL.Path),
                    attribute.String("status", status),
                ),
            )
            
            return err
        }
    })
}
```

### Request Validation

Validate requests before handler execution:

```go
func validateContentTypeInterceptor(allowedTypes ...string) rest.ServerInterceptor {
    allowed := make(map[string]bool)
    for _, ct := range allowedTypes {
        allowed[ct] = true
    }
    
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            contentType := r.Header.Get("Content-Type")
            
            if !allowed[contentType] {
                return rest.InvalidContentTypeError{
                    ContentType: contentType,
                }
            }
            
            return next(w, r)
        }
    })
}

// Usage
rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/data"),
    handler,
    rest.Intercept(validateContentTypeInterceptor("application/json", "application/xml")),
)
```

## Advanced Patterns

### Multiple Interceptors

Chain multiple interceptors by calling `rest.Intercept()` multiple times. They execute in registration order:

```go
rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/orders"),
    createOrderHandler,
    rest.Intercept(requestIDInterceptor),      // Executes first
    rest.Intercept(loggingInterceptor),        // Executes second
    rest.Intercept(authInterceptor(validator)), // Executes third
    rest.Intercept(rateLimitInterceptor(limiter)), // Executes fourth
)
```

**Execution flow:**
1. Request ID interceptor generates ID and injects into context
2. Logging interceptor logs incoming request with ID
3. Auth interceptor validates token and injects user
4. Rate limit interceptor checks limits
5. Handler executes
6. Rate limit interceptor completes (if any post-processing)
7. Auth interceptor completes
8. Logging interceptor logs response
9. Request ID interceptor completes

### Conditional Execution

Skip handler execution based on conditions:

```go
cacheInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        // Check cache
        cacheKey := generateCacheKey(r)
        if cached, found := cache.Get(cacheKey); found {
            // Return cached response, skip handler
            w.Header().Set("Content-Type", "application/json")
            w.Header().Set("X-Cache", "HIT")
            w.Write(cached)
            return nil // Don't call next()
        }
        
        // Cache miss - continue to handler
        w.Header().Set("X-Cache", "MISS")
        return next(w, r)
    }
})
```

### Context Value Injection

Pass data between interceptors and handlers using context:

```go
type contextKey string

const (
    userContextKey    contextKey = "user"
    requestIDKey      contextKey = "request_id"
    sessionContextKey contextKey = "session"
)

// First interceptor: inject request ID
requestIDInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        requestID := generateRequestID()
        ctx := context.WithValue(r.Context(), requestIDKey, requestID)
        return next(w, r.WithContext(ctx))
    }
})

// Second interceptor: use request ID, inject user
authInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        requestID := r.Context().Value(requestIDKey).(string)
        
        user, err := authenticateUser(r, requestID)
        if err != nil {
            return err
        }
        
        ctx := context.WithValue(r.Context(), userContextKey, user)
        return next(w, r.WithContext(ctx))
    }
})

// Handler: use both values
handler := rest.ProducerFunc[UserProfile](func(ctx context.Context) (*UserProfile, error) {
    user := ctx.Value(userContextKey).(*User)
    requestID := ctx.Value(requestIDKey).(string)
    
    log.Printf("Request %s: fetching profile for user %s", requestID, user.ID)
    return getProfile(ctx, user.ID)
})
```

### Interceptor Composition

Create reusable interceptor builders:

```go
// Builder function for auth interceptors
func requireRole(role string, validator TokenValidator) rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            user, err := validator.ValidateAndExtractUser(r.Context(), r.Header.Get("Authorization"))
            if err != nil {
                return rest.UnauthorizedError{Message: "authentication failed"}
            }
            
            if !user.HasRole(role) {
                return rest.UnauthorizedError{Message: "insufficient permissions"}
            }
            
            ctx := context.WithValue(r.Context(), userContextKey, user)
            return next(w, r.WithContext(ctx))
        }
    })
}

// Builder function for logging with custom fields
func logWithFields(fields ...slog.Attr) rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            log := humus.Logger("api")
            attrs := append([]any{
                slog.String("method", r.Method),
                slog.String("path", r.URL.Path),
            }, fields...)
            
            log.InfoContext(r.Context(), "request", attrs...)
            return next(w, r)
        }
    })
}

// Usage
rest.Handle(
    http.MethodDelete,
    rest.BasePath("/admin/users").Param("id"),
    deleteUserHandler,
    rest.Intercept(requireRole("admin", myValidator)),
    rest.Intercept(logWithFields(slog.String("operation", "user_deletion"))),
)
```

### State Management with Closures

Use closures to maintain state across requests:

```go
func createInFlightLimiter(maxConcurrent int) rest.ServerInterceptor {
    var (
        mu      sync.Mutex
        current int
    )
    
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            // Acquire slot
            mu.Lock()
            if current >= maxConcurrent {
                mu.Unlock()
                return rest.BadRequestError{Message: "server busy, too many concurrent requests"}
            }
            current++
            mu.Unlock()
            
            // Release slot on exit
            defer func() {
                mu.Lock()
                current--
                mu.Unlock()
            }()
            
            return next(w, r)
        }
    })
}

// Usage
rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/expensive-operation"),
    handler,
    rest.Intercept(createInFlightLimiter(10)),
)
```

## Best Practices

### Keep Interceptors Focused

Each interceptor should have a single, clear responsibility:

```go
// Good - focused interceptors
rest.Intercept(requestIDInterceptor)
rest.Intercept(authInterceptor(validator))
rest.Intercept(loggingInterceptor)

// Bad - do-everything interceptor
rest.Intercept(doEverythingInterceptor) // logging + auth + metrics + validation
```

### Handle Errors Appropriately

Return appropriate error types to ensure correct HTTP status codes:

```go
authInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        token := r.Header.Get("Authorization")
        
        // Use specific error types
        if token == "" {
            return rest.UnauthorizedError{Message: "missing token"}
        }
        
        user, err := validateToken(r.Context(), token)
        if err != nil {
            return rest.UnauthorizedError{Message: "invalid token"}
        }
        
        ctx := context.WithValue(r.Context(), userContextKey, user)
        return next(w, r.WithContext(ctx))
    }
})
```

### Use Type-Safe Context Keys

Avoid string collisions by using typed context keys:

```go
// Define typed keys
type contextKey string

const (
    userKey      contextKey = "user"
    requestIDKey contextKey = "request_id"
)

// Use in interceptor
ctx := context.WithValue(r.Context(), userKey, user)

// Use in handler
user, ok := ctx.Value(userKey).(*User)
if !ok {
    return nil, fmt.Errorf("user not found in context")
}
```

### Consider Performance

Minimize work in interceptors, especially for high-traffic endpoints:

```go
// Good - efficient check
if !isValidFormat(r.Header.Get("X-Custom-Header")) {
    return rest.BadRequestError{Message: "invalid header format"}
}

// Bad - expensive operation on every request
user, err := db.QueryUser(r.Context(), extractUserID(r))
if err != nil {
    return err
}
```

### Order Matters

Place interceptors in logical order:

1. Request ID generation (needed by logging)
2. Logging (needs request ID)
3. Authentication (needed by authorization)
4. Authorization (needs user from auth)
5. Rate limiting (after auth for per-user limits)
6. Validation (after auth/authz checks)

```go
rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/admin/users"),
    handler,
    rest.Intercept(requestIDInterceptor),      // 1. Generate ID
    rest.Intercept(loggingInterceptor),        // 2. Log with ID
    rest.Intercept(authInterceptor),           // 3. Authenticate
    rest.Intercept(requireAdminInterceptor),   // 4. Check permissions
    rest.Intercept(rateLimitInterceptor),      // 5. Check rate limits
    rest.Intercept(validateInputInterceptor),  // 6. Validate request
)
```

## Interceptors vs Error Handlers

Interceptors and error handlers serve different purposes:

**Use Interceptors for:**
- Pre-processing requests (authentication, validation, transformation)
- Post-processing responses (adding headers, logging)
- Short-circuiting execution (caching, rate limiting)
- Context enrichment (user data, request IDs)

**Use Error Handlers (rest.OnError) for:**
- Formatting error responses
- Mapping errors to HTTP status codes
- Logging errors in a consistent format
- Converting errors to RFC 7807 Problem Details

**Example combining both:**

```go
// Interceptor: pre-process request
authInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        user, err := authenticate(r)
        if err != nil {
            return rest.UnauthorizedError{Message: "authentication failed"}
        }
        
        ctx := context.WithValue(r.Context(), userContextKey, user)
        return next(w, r.WithContext(ctx))
    }
})

// Error handler: format errors
errorHandler := rest.NewProblemDetailsErrorHandler(
    rest.WithDefaultType("https://api.example.com/errors"),
)

// Combine
rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/orders"),
    createOrderHandler,
    rest.Intercept(authInterceptor),  // Pre-process
    rest.OnError(errorHandler),       // Format errors
)
```

## Complete Example

```go
package main

import (
    "context"
    "log/slog"
    "net/http"
    "time"

    "github.com/z5labs/humus"
    "github.com/z5labs/humus/rest"
)

type contextKey string

const (
    requestIDKey contextKey = "request_id"
    userKey      contextKey = "user"
)

// Request ID interceptor
func requestIDInterceptor() rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            requestID := r.Header.Get("X-Request-ID")
            if requestID == "" {
                requestID = generateRequestID()
            }
            
            ctx := context.WithValue(r.Context(), requestIDKey, requestID)
            w.Header().Set("X-Request-ID", requestID)
            
            return next(w, r.WithContext(ctx))
        }
    })
}

// Logging interceptor
func loggingInterceptor() rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            start := time.Now()
            log := humus.Logger("api")
            
            requestID, _ := r.Context().Value(requestIDKey).(string)
            
            log.InfoContext(r.Context(), "incoming request",
                slog.String("request_id", requestID),
                slog.String("method", r.Method),
                slog.String("path", r.URL.Path),
            )
            
            err := next(w, r)
            
            duration := time.Since(start)
            if err != nil {
                log.ErrorContext(r.Context(), "request failed",
                    slog.String("request_id", requestID),
                    slog.Duration("duration", duration),
                    slog.Any("error", err),
                )
            } else {
                log.InfoContext(r.Context(), "request completed",
                    slog.String("request_id", requestID),
                    slog.Duration("duration", duration),
                )
            }
            
            return err
        }
    })
}

// Auth interceptor
func authInterceptor(validator TokenValidator) rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            token := r.Header.Get("Authorization")
            if token == "" {
                return rest.UnauthorizedError{Message: "missing authorization"}
            }
            
            user, err := validator.Validate(r.Context(), token)
            if err != nil {
                return rest.UnauthorizedError{Message: "invalid token"}
            }
            
            ctx := context.WithValue(r.Context(), userKey, user)
            return next(w, r.WithContext(ctx))
        }
    })
}

type Order struct {
    ID     string `json:"id"`
    UserID string `json:"user_id"`
    Total  int    `json:"total"`
}

type CreateOrderRequest struct {
    Items []string `json:"items"`
}

func Init(ctx context.Context, cfg rest.Config) (*rest.Api, error) {
    validator := NewTokenValidator()
    
    handler := rest.HandlerFunc[CreateOrderRequest, Order](
        func(ctx context.Context, req *CreateOrderRequest) (*Order, error) {
            user := ctx.Value(userKey).(*User)
            
            order := &Order{
                ID:     generateOrderID(),
                UserID: user.ID,
                Total:  calculateTotal(req.Items),
            }
            
            return order, nil
        },
    )
    
    api := rest.NewApi(
        "Orders API",
        "1.0.0",
        rest.Handle(
            http.MethodPost,
            rest.BasePath("/orders"),
            rest.HandleJson(handler),
            rest.Intercept(requestIDInterceptor()),
            rest.Intercept(loggingInterceptor()),
            rest.Intercept(authInterceptor(validator)),
        ),
    )
    
    return api, nil
}

func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)
}
```

## See Also

- [Error Handling]({{< ref "error-handling" >}}) - Custom error responses and error handlers
- [Authentication]({{< ref "authentication" >}}) - Built-in authentication schemes
- [Handler Helpers]({{< ref "handler-helpers" >}}) - Type-safe request/response handling
- [API Reference](https://pkg.go.dev/github.com/z5labs/humus/rest)
