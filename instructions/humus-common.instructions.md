# Humus Framework - Common Patterns

This file provides common patterns and best practices applicable to all Humus application types (REST, gRPC, Queue, Job). Copy this file along with your application-type-specific instructions to your repository's `.github/` or `instructions/` directory.

## Overview

Humus is a modular Go framework built on [Bedrock](https://github.com/z5labs/bedrock) for creating production-ready REST APIs, gRPC services, jobs, and queue processors. Every application automatically includes OpenTelemetry instrumentation, health monitoring, and graceful shutdown.

## Configuration

**Always embed the appropriate Config type:**
```go
type Config struct {
    rest.Config `config:",squash"`  // For REST services
    // OR
    grpc.Config `config:",squash"`  // For gRPC services
    // OR
    humus.Config `config:",squash"` // For Job/Queue services
    
    // Service-specific configuration
    Database struct {
        URL string `config:"url"`
    } `config:"database"`
}
```

**Implement provider interfaces when needed:**
```go
func (c Config) Listener(ctx context.Context) (net.Listener, error) {
    return net.Listen("tcp", fmt.Sprintf(":%d", c.HTTP.Port))
}
```

**Use Go templates in config.yaml:**
```yaml
openapi:
  title: {{env "SERVICE_NAME" | default "My Service"}}
  version: {{env "VERSION" | default "v1.0.0"}}

database:
  url: {{env "DATABASE_URL" | default "postgres://localhost/mydb"}}
```

## Error Handling

**Runner-Level (applies to all service types):**
```go
runner := humus.NewRunner(builder, humus.OnError(humus.ErrorHandlerFunc(func(err error) {
    log.Fatal(err)
})))
```

**Operation-Level (REST-specific, see humus-rest.instructions.md):**
```go
rest.Handle(method, path, handler,
    rest.OnError(rest.ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
        w.WriteHeader(http.StatusInternalServerError)
        json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
    })),
)
```

## Logging & Observability

**Always use humus.Logger:**
```go
log := humus.Logger("service-name")
log.Info("user created", slog.String("user_id", userID))
log.Error("failed to process", slog.String("error", err.Error()))
```

**Logs automatically correlate with OpenTelemetry traces** - no manual instrumentation needed.

## Health Monitoring

**Binary Health Check:**
```go
monitor := new(health.Binary)
monitor.MarkHealthy()    // Service is healthy
monitor.MarkUnhealthy()  // Service is unhealthy
```

**Composite Monitors:**
```go
// Both must be healthy
health.And(dbMonitor, cacheMonitor)

// At least one must be healthy
health.Or(replica1Monitor, replica2Monitor)
```

## Lifecycle Management

**Use lifecycle hooks for resource cleanup:**
```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db, err := sql.Open("postgres", cfg.DB.URL)
    if err != nil {
        return nil, err
    }
    
    lc, _ := lifecycle.FromContext(ctx)
    lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
        return db.Close()
    }))
    
    return api, nil
}
```

## Testing Patterns

### Table-Driven Tests

```go
func TestCreateUser(t *testing.T) {
    tests := []struct {
        name    string
        input   CreateUserRequest
        want    *UserResponse
        wantErr bool
    }{
        {
            name:  "valid user",
            input: CreateUserRequest{Name: "John"},
            want:  &UserResponse{ID: "123", Name: "John"},
        },
        {
            name:    "empty name",
            input:   CreateUserRequest{Name: ""},
            wantErr: true,
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got, err := handler(context.Background(), &tt.input)
            if tt.wantErr {
                require.Error(t, err)
                return
            }
            require.NoError(t, err)
            require.Equal(t, tt.want, got)
        })
    }
}
```

### Use testify/require (not assert)

❌ **Wrong:**
```go
assert.Equal(t, expected, actual)  // Test continues on failure
```

✅ **Correct:**
```go
require.Equal(t, expected, actual)  // Test stops immediately on failure
```

## Best Practices

### DO ✅

1. **Keep main.go minimal** - just call `rest.Run()`, `grpc.Run()`, or `queue.Run()` with `app.Init`
2. **Embed configuration files** - use `//go:embed config.yaml` for portability
3. **Use the app/ package for Init function** - keeps business logic separate from main
4. **Embed humus.Config (or rest.Config/grpc.Config) in custom Config** - required for OpenTelemetry
5. **Use Go templates in config.yaml** - `{{env "VAR" | default "value"}}`
6. **Return early to reduce nesting** - keep the happy path left-aligned
7. **Test with `-race` flag** - catch concurrency issues early
8. **Use lifecycle hooks for cleanup** - ensures resources are released properly
9. **Always handle errors** - don't ignore them
10. **Use humus.Logger** - integrates automatically with OpenTelemetry

### DON'T ❌

1. **Don't bypass lifecycle wrappers** - manually starting servers breaks OTel and graceful shutdown
2. **Don't put business logic in main.go** - use app/app.go and domain packages
3. **Don't hardcode configuration** - use environment variables with templates
4. **Don't duplicate package declarations** - each .go file has exactly ONE package line
5. **Don't use assert in tests** - use require.* to fail fast
6. **Don't forget to close resources** - use lifecycle hooks for cleanup
7. **Don't ignore errors** - always handle or propagate them
8. **Don't manually initialize OpenTelemetry** - Humus does this automatically
9. **Don't create goroutines without cleanup** - know how they will exit
10. **Don't share state without synchronization** - use mutexes or channels

## Common Pitfalls

### Incorrect Config Embedding

❌ **Wrong:**
```go
type Config struct {
    Title string
    // Missing humus.Config embedding
}
```

✅ **Correct:**
```go
type Config struct {
    rest.Config `config:",squash"`  // or grpc.Config or humus.Config
    // Your config here
}
```

### Not Using Lifecycle Hooks

❌ **Wrong (resource leak):**
```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db, _ := sql.Open("postgres", cfg.DB.URL)
    // db never gets closed!
    return api, nil
}
```

✅ **Correct:**
```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db, _ := sql.Open("postgres", cfg.DB.URL)
    
    lc, _ := lifecycle.FromContext(ctx)
    lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
        return db.Close()
    }))
    
    return api, nil
}
```

## Health Endpoints

All services automatically include health endpoints:

- **Liveness**: `/health/liveness` - Always returns 200 when server is running
- **Readiness**: `/health/readiness` - Returns 200 when service is ready (checks monitors)

## Additional Resources

- **Documentation**: https://z5labs.dev/humus/
- **Repository**: https://github.com/z5labs/humus
- **Bedrock Framework**: https://github.com/z5labs/bedrock
- **Examples**: https://github.com/z5labs/humus/tree/main/example

## Version Information

This instructions file is designed for Humus applications using:
- Go 1.24 or later
- Humus framework latest version

Keep this file updated as you add project-specific patterns and conventions.
