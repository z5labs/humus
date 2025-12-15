---
title: Middleware
description: Request processing patterns
weight: 60
type: docs
---

Humus provides flexible patterns for request processing in REST services through interceptors and error handlers.

## Overview

Request processing in Humus operates at the **operation level**, meaning you configure behavior per-endpoint using options passed to `rest.Handle()`. This approach provides fine-grained control while maintaining simplicity.

## Interceptors

Interceptors provide operation-level request and response processing. They execute before your handler runs and can:

- Pre-process requests (authentication, validation, transformation)
- Post-process responses (add headers, log results)
- Short-circuit execution (caching, rate limiting)
- Enrich request context with data

### Basic Example

```go
loggingInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        start := time.Now()
        log := humus.Logger("api")
        
        log.InfoContext(r.Context(), "incoming request",
            slog.String("method", r.Method),
            slog.String("path", r.URL.Path),
        )
        
        err := next(w, r)
        
        log.InfoContext(r.Context(), "request completed",
            slog.Duration("duration", time.Since(start)),
        )
        
        return err
    }
})

rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/data"),
    handler,
    rest.Intercept(loggingInterceptor),
)
```

### Multiple Interceptors

Chain multiple interceptors by calling `rest.Intercept()` multiple times:

```go
rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/orders"),
    createOrderHandler,
    rest.Intercept(requestIDInterceptor),
    rest.Intercept(loggingInterceptor),
    rest.Intercept(authInterceptor),
    rest.Intercept(rateLimitInterceptor),
)
```

Interceptors execute in registration order.

## Error Handlers

Error handlers control how errors are formatted and returned to clients. They operate at the operation level and can be customized per-endpoint:

```go
errorHandler := rest.NewProblemDetailsErrorHandler(
    rest.WithDefaultType("https://api.example.com/errors"),
)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/users"),
    createUserHandler,
    rest.OnError(errorHandler),
)
```

## Request Processing Flow

The complete request processing flow:

1. **Interceptor chain** (in registration order)
   - Each interceptor can modify the request
   - Each interceptor can short-circuit by not calling `next()`
   - Errors propagate up the chain
2. **Handler execution**
   - Your business logic runs
   - Returns response or error
3. **Error handling** (if error occurred)
   - Custom error handler formats error
   - Appropriate HTTP status code is set
4. **Interceptor chain completion** (in reverse order)
   - Each interceptor's post-processing logic runs
   - Response headers can be modified

## Common Patterns

### Authentication & Authorization

Use interceptors for authentication and context enrichment:

```go
func authInterceptor(validator TokenValidator) rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            user, err := validator.Validate(r.Context(), r.Header.Get("Authorization"))
            if err != nil {
                return rest.UnauthorizedError{Message: "invalid token"}
            }
            
            ctx := context.WithValue(r.Context(), "user", user)
            return next(w, r.WithContext(ctx))
        }
    })
}

rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/profile"),
    profileHandler,
    rest.Intercept(authInterceptor(myValidator)),
)
```

### Logging with Request IDs

Chain interceptors to build context:

```go
// Generate request ID
requestIDInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        requestID := generateRequestID()
        ctx := context.WithValue(r.Context(), "request_id", requestID)
        w.Header().Set("X-Request-ID", requestID)
        return next(w, r.WithContext(ctx))
    }
})

// Log with request ID
loggingInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        requestID := r.Context().Value("request_id").(string)
        log := humus.Logger("api")
        
        log.InfoContext(r.Context(), "request",
            slog.String("request_id", requestID),
            slog.String("path", r.URL.Path),
        )
        
        return next(w, r)
    }
})

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/orders"),
    handler,
    rest.Intercept(requestIDInterceptor),
    rest.Intercept(loggingInterceptor),
)
```

### Rate Limiting

Use interceptors to enforce rate limits:

```go
func rateLimitInterceptor(limiter RateLimiter) rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            if !limiter.Allow(r.RemoteAddr) {
                return rest.BadRequestError{Message: "rate limit exceeded"}
            }
            return next(w, r)
        }
    })
}

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api/expensive-operation"),
    handler,
    rest.Intercept(rateLimitInterceptor(myLimiter)),
)
```

### Caching

Short-circuit handler execution with caching:

```go
cacheInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        cacheKey := generateCacheKey(r)
        
        if cached, found := cache.Get(cacheKey); found {
            w.Header().Set("Content-Type", "application/json")
            w.Header().Set("X-Cache", "HIT")
            w.Write(cached)
            return nil // Skip handler
        }
        
        w.Header().Set("X-Cache", "MISS")
        return next(w, r)
    }
})
```

## Interceptor Composition

Build reusable interceptor factories:

```go
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
            
            ctx := context.WithValue(r.Context(), "user", user)
            return next(w, r.WithContext(ctx))
        }
    })
}

// Usage
rest.Handle(
    http.MethodDelete,
    rest.BasePath("/admin/users").Param("id"),
    deleteUserHandler,
    rest.Intercept(requireRole("admin", myValidator)),
)
```

## Best Practices

### 1. Keep Interceptors Focused

Each interceptor should have a single, clear responsibility:

```go
// Good - focused interceptors
rest.Intercept(requestIDInterceptor)
rest.Intercept(authInterceptor)
rest.Intercept(loggingInterceptor)

// Bad - do-everything interceptor
rest.Intercept(megaInterceptor) // auth + logging + metrics + validation
```

### 2. Order Matters

Place interceptors in logical order:

1. Request ID generation (needed by logging)
2. Logging (needs request ID)
3. Authentication (needed by authorization)
4. Authorization (needs user from auth)
5. Rate limiting (after auth for per-user limits)

### 3. Use Type-Safe Context Keys

Avoid collisions with typed context keys:

```go
type contextKey string

const (
    userKey      contextKey = "user"
    requestIDKey contextKey = "request_id"
)

ctx := context.WithValue(r.Context(), userKey, user)
```

### 4. Return Appropriate Errors

Use framework error types for correct HTTP status codes:

```go
// Returns 401
return rest.UnauthorizedError{Message: "invalid token"}

// Returns 400
return rest.BadRequestError{Message: "missing parameter"}
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

func requestIDInterceptor() rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            requestID := generateRequestID()
            ctx := context.WithValue(r.Context(), "request_id", requestID)
            w.Header().Set("X-Request-ID", requestID)
            return next(w, r.WithContext(ctx))
        }
    })
}

func loggingInterceptor() rest.ServerInterceptor {
    return rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
        return func(w http.ResponseWriter, r *http.Request) error {
            start := time.Now()
            log := humus.Logger("api")
            
            requestID := r.Context().Value("request_id").(string)
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
            
            ctx := context.WithValue(r.Context(), "user", user)
            return next(w, r.WithContext(ctx))
        }
    })
}

func Init(ctx context.Context, cfg rest.Config) (*rest.Api, error) {
    validator := NewTokenValidator()
    
    handler := rest.ProducerFunc[UserProfile](func(ctx context.Context) (*UserProfile, error) {
        user := ctx.Value("user").(*User)
        return getProfile(ctx, user.ID)
    })
    
    api := rest.NewApi(
        "API",
        "1.0.0",
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/profile"),
            rest.ProduceJson(handler),
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

- [Interceptors]({{< ref "/features/rest/interceptors" >}}) - Detailed interceptor documentation
- [Error Handling]({{< ref "/features/rest/error-handling" >}}) - Custom error responses
- [Authentication]({{< ref "/features/rest/authentication" >}}) - Built-in auth schemes
- [API Reference](https://pkg.go.dev/github.com/z5labs/humus/rest)
