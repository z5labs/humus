---
title: Lifecycle Management
description: Graceful shutdown and signal handling
weight: 30
type: docs
---

# Lifecycle Management

Humus provides automatic lifecycle management for all service types, including graceful shutdown, panic recovery, and OS signal handling.

## Service Lifecycle

Every Humus service follows this lifecycle:

```
1. Configuration Loading
   ↓
2. Initialization (Init function)
   ↓
3. Service Startup
   ↓
4. Running (handling requests/jobs)
   ↓
5. Shutdown Signal Received
   ↓
6. Graceful Shutdown
   ↓
7. Cleanup & Exit
```

## Lifecycle Phases

### 1. Configuration Loading

Before your code runs, Humus loads configuration from the specified source:

```go
func main() {
    // Configuration is loaded here
    rest.Run(rest.YamlSource("config.yaml"), Init)
}
```

If configuration loading fails, the service exits with an error.

### 2. Initialization

Your `Init` function is called with the loaded configuration:

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Set up resources
    db, err := connectDatabase(ctx, cfg.Database)
    if err != nil {
        return nil, fmt.Errorf("failed to connect to database: %w", err)
    }

    // Build the API
    api := rest.NewApi("My Service", "1.0.0")

    // Register handlers
    rest.Handle(http.MethodGet, rest.BasePath("/users"), userHandler)

    return api, nil
}
```

If `Init` returns an error, the service exits without starting.

**The context passed to Init:**
- Contains trace context for instrumentation
- Is NOT cancelled when shutdown begins
- Should be used for initialization operations that need context

### 3. Service Startup

After successful initialization, Humus starts the service:

- **REST**: HTTP server starts listening on configured port
- **gRPC**: gRPC server starts listening on configured port
- **Job**: Job handler begins execution

### 4. Running

The service handles requests or executes jobs:

- **REST/gRPC**: Servers handle incoming requests
- **Job**: Handler executes once, then service waits for shutdown signal

### 5. Shutdown Signal

Humus listens for OS signals:

- **SIGINT** (Ctrl+C)
- **SIGTERM** (Docker/Kubernetes termination)

When received, graceful shutdown begins.

### 6. Graceful Shutdown

Humus gracefully shuts down the service:

**For REST/gRPC:**
1. Stop accepting new connections
2. Wait for in-flight requests to complete (with timeout)
3. Close the server

**For Jobs:**
1. Cancel the job context
2. Wait for job to return (handler should respect context cancellation)

### 7. Cleanup & Exit

After shutdown completes, the service exits with appropriate status code.

## Shutdown Handling

### Automatic Graceful Shutdown

Shutdown is automatic - no code needed:

```go
func main() {
    // Graceful shutdown is built-in
    rest.Run(rest.YamlSource("config.yaml"), Init)
}
```

When SIGTERM/SIGINT is received:
1. Server stops accepting new connections
2. Existing requests are allowed to complete
3. Server shuts down after all requests finish (or timeout)

### Context Cancellation

For long-running operations, respect context cancellation:

```go
func processJob(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            // Context was cancelled (shutdown signal received)
            log.Info("shutting down gracefully")
            return ctx.Err()

        default:
            // Do work
            if err := processNextItem(ctx); err != nil {
                return err
            }
        }
    }
}
```

### Cleanup Resources

Clean up in your handlers when context is cancelled:

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db, err := connectDatabase(ctx, cfg.Database)
    if err != nil {
        return nil, err
    }

    api := rest.NewApi("My Service", "1.0.0")

    handler := rpc.NewOperation(
        rpc.Handle(func(ctx context.Context, req Request) (Response, error) {
            // Handler automatically respects context cancellation
            return processRequest(ctx, db, req)
        }),
    )

    rest.Handle(http.MethodPost, rest.BasePath("/process"), handler)
    return api, nil
}

// No explicit cleanup needed - Humus handles server shutdown
```

For resources that need explicit cleanup, use the handler's context:

```go
type JobHandler struct {
    db *sql.DB
}

func (h *JobHandler) Handle(ctx context.Context) error {
    // Job will be cancelled when shutdown signal received
    defer h.db.Close()

    return processWithDatabase(ctx, h.db)
}
```

## Panic Recovery

Humus automatically recovers from panics in handlers:

```go
func handleRequest(ctx context.Context, req Request) (Response, error) {
    // If this panics, Humus recovers and returns 500
    panic("something went wrong")
}
```

**What happens:**
1. Panic is caught
2. Error is logged with stack trace
3. For REST: HTTP 500 response sent
4. For gRPC: Internal error status returned
5. Service continues running (not crashed)

**Don't rely on panic recovery:**
- Use proper error handling with `error` returns
- Panic recovery is a safety net, not a pattern

## Job Lifecycle

Jobs have a simpler lifecycle:

```go
type MyJob struct{}

func (j *MyJob) Handle(ctx context.Context) error {
    // Job starts executing immediately
    log.InfoContext(ctx, "job started")

    // Do work...
    if err := doWork(ctx); err != nil {
        return err  // Job fails, service exits with error
    }

    log.InfoContext(ctx, "job completed")
    return nil  // Job succeeds, service exits cleanly
}

func main() {
    job.Run(job.YamlSource("config.yaml"), func(ctx context.Context, cfg Config) (job.Handler, error) {
        return &MyJob{}, nil
    })
}
```

**Job execution:**
1. Job starts immediately after initialization
2. Context is valid until job returns OR shutdown signal received
3. If job returns `nil`, service exits with code 0
4. If job returns error, service exits with code 1
5. If shutdown signal received, context is cancelled

**Respecting shutdown in jobs:**

```go
func (j *MyJob) Handle(ctx context.Context) error {
    items, err := fetchItems(ctx)
    if err != nil {
        return err
    }

    for _, item := range items {
        // Check if shutdown was requested
        select {
        case <-ctx.Done():
            log.InfoContext(ctx, "shutdown requested, stopping job")
            return ctx.Err()
        default:
            // Process item
            if err := processItem(ctx, item); err != nil {
                return err
            }
        }
    }

    return nil
}
```

## Timeouts

### Shutdown Timeout

REST and gRPC servers have default shutdown timeouts. If requests don't complete in time, the server forcefully shuts down.

This is managed by Bedrock and typically doesn't need configuration.

### Request Timeouts

For long-running requests, implement your own timeouts:

```go
func handleRequest(ctx context.Context, req Request) (Response, error) {
    // Create timeout context
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()

    // This will fail if it takes > 30 seconds
    return processRequest(ctx, req)
}
```

### Job Timeouts

Jobs can implement their own timeouts:

```go
func (j *MyJob) Handle(ctx context.Context) error {
    // Set maximum job duration
    ctx, cancel := context.WithTimeout(ctx, 1*time.Hour)
    defer cancel()

    return processJob(ctx)
}
```

## Health During Lifecycle

REST services provide health endpoints that reflect lifecycle state:

**During Initialization:**
- Liveness: Not ready (server hasn't started)
- Readiness: Not ready

**During Normal Operation:**
- Liveness: Healthy
- Readiness: Healthy (unless custom health check fails)

**During Shutdown:**
- Liveness: Healthy (but server is shutting down)
- Readiness: Unhealthy (stops receiving traffic)

See [REST Health Checks]({{< ref "/features/rest/health-checks" >}}) for details.

## Best Practices

### 1. Fast Initialization

Keep `Init` function fast:

```go
// Good - quick setup
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db := newDatabaseClient(cfg.Database)  // Just create client
    api := rest.NewApi("My Service", "1.0.0")
    // Register handlers...
    return api, nil
}

// Avoid - slow startup
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db := newDatabaseClient(cfg.Database)
    if err := db.Migrate(); err != nil {  // Don't run migrations here!
        return nil, err
    }
    // ...
}
```

Run migrations as a separate job service.

### 2. Respect Context Cancellation

Always check context in loops:

```go
// Good
func processItems(ctx context.Context, items []Item) error {
    for _, item := range items {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            process(item)
        }
    }
    return nil
}

// Bad - ignores shutdown
func processItems(ctx context.Context, items []Item) error {
    for _, item := range items {
        process(item)  // Won't stop on shutdown!
    }
    return nil
}
```

### 3. Proper Error Handling

Return errors from `Init` for startup failures:

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db, err := connectDatabase(ctx, cfg.Database)
    if err != nil {
        return nil, fmt.Errorf("database connection failed: %w", err)
    }

    api := rest.NewApi("My Service", "1.0.0")
    // ...
    return api, nil
}
```

This ensures the service doesn't start in a broken state.

### 4. Resource Cleanup

For most resources, cleanup is automatic:
- HTTP/gRPC servers are closed by Humus
- Contexts are cancelled on shutdown

For resources that need explicit cleanup (database connections, file handles), either:

**Option 1: Defer in handlers**
```go
func (h *Handler) Handle(ctx context.Context) error {
    defer h.cleanup()
    return h.process(ctx)
}
```

**Option 2: Use finalizers (advanced)**

For advanced lifecycle hooks, use Bedrock's lifecycle management directly. See [Advanced Topics]({{< ref "/advanced" >}}).

### 5. Don't Block Shutdown

Avoid operations that might block shutdown:

```go
// Bad - might block shutdown indefinitely
func processJob(ctx context.Context) error {
    for {
        item := blockingQueue.Get()  // Blocks forever!
        process(item)
    }
}

// Good - respects cancellation
func processJob(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case item := <-queue:
            process(item)
        }
    }
}
```

## Next Steps

- Learn about [REST Health Checks]({{< ref "/features/rest/health-checks" >}}) for monitoring service health
- Explore [Advanced Topics]({{< ref "/advanced" >}}) for custom lifecycle hooks
- See [Job Services]({{< ref "/features/job" >}}) for job-specific lifecycle patterns
