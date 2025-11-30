---
title: Health Checks
description: Monitoring service health
weight: 80
type: docs
---


Humus provides built-in health check endpoints and flexible health monitoring abstractions to support liveness and readiness probes in container orchestration platforms like Kubernetes.

## Overview

Health checks help orchestration platforms determine:

- **Liveness** - Is the application running? Should it be restarted?
- **Readiness** - Is the application ready to serve traffic? Should it receive requests?

All Humus REST services automatically include default health endpoints that return `200 OK`. You can customize these endpoints to check actual application health, such as database connectivity, dependency availability, or internal state.

## Built-in Endpoints

Every REST API created with `rest.NewApi()` automatically provides:

### GET /health/liveness

Indicates whether the application is alive and running. If this endpoint fails, the container orchestrator should restart the service.

**Default Behavior:** Returns `200 OK` immediately

**Use Cases:**
- Detect deadlocks or infinite loops
- Identify unrecoverable application states
- Trigger automatic restarts for frozen services

### GET /health/readiness

Indicates whether the application is ready to accept traffic. If this endpoint fails, the orchestrator should stop routing requests to this instance.

**Default Behavior:** Returns `200 OK` immediately

**Use Cases:**
- Database connection checks
- Cache warmup completion
- Dependency service availability
- Configuration loading completion

## Custom Health Checks

Override the default health endpoints by passing custom handlers to `rest.NewApi()`.

### Basic Custom Handler

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Check your dependencies
        if !isDatabaseConnected() {
            w.WriteHeader(http.StatusServiceUnavailable)
            w.Write([]byte("database unavailable"))
            return
        }

        w.WriteHeader(http.StatusOK)
        w.Write([]byte("ready"))
    })

    api := rest.NewApi(
        "My Service",
        "1.0.0",
        rest.Readiness(readinessHandler),
        // ... your operations
    )

    return api, nil
}
```

### Using Health Monitors

Humus provides the `health.Monitor` interface for composable health checking:

```go
type Monitor interface {
    Healthy(context.Context) (bool, error)
}
```

This abstraction allows you to:
- Compose multiple health checks
- Share health monitoring logic between liveness and readiness
- Test health logic independently

## Health Monitor Implementations

### Binary Monitor

A simple thread-safe monitor with two states: healthy or unhealthy.

```go
import "github.com/z5labs/humus/health"

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Create a binary health monitor
    var appHealth health.Binary

    // Start as unhealthy (zero value)
    // Mark healthy after initialization completes
    defer appHealth.MarkHealthy()

    // Initialize database
    db, err := sql.Open("postgres", cfg.DatabaseURL)
    if err != nil {
        return nil, err
    }

    // Create readiness handler
    readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        healthy, err := appHealth.Healthy(r.Context())
        if err != nil || !healthy {
            w.WriteHeader(http.StatusServiceUnavailable)
            w.Write([]byte("not ready"))
            return
        }

        w.WriteHeader(http.StatusOK)
        w.Write([]byte("ready"))
    })

    api := rest.NewApi(
        "My Service",
        "1.0.0",
        rest.Readiness(readinessHandler),
    )

    // Mark unhealthy on shutdown
    lc, _ := lifecycle.FromContext(ctx)
    lc.OnPreShutdown(lifecycle.HookFunc(func(ctx context.Context) error {
        appHealth.MarkUnhealthy()
        return nil
    }))

    return api, nil
}
```

**Thread Safety:** Binary monitors use `atomic.Bool` internally and are safe for concurrent use.

### AndMonitor - Fail Fast Composition

Combines multiple monitors with logical AND (&&) semantics. Returns healthy only if **all** monitors are healthy.

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Create individual monitors
    var dbHealth health.Binary
    var cacheHealth health.Binary
    var queueHealth health.Binary

    // Combine with AND - all must be healthy
    readinessMonitor := health.And(&dbHealth, &cacheHealth, &queueHealth)

    // Initialize components
    db, err := initDatabase(cfg)
    if err != nil {
        return nil, err
    }
    dbHealth.MarkHealthy()

    cache, err := initCache(cfg)
    if err != nil {
        return nil, err
    }
    cacheHealth.MarkHealthy()

    queue, err := initQueue(cfg)
    if err != nil {
        return nil, err
    }
    queueHealth.MarkHealthy()

    // Create handler using combined monitor
    readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        healthy, err := readinessMonitor.Healthy(r.Context())
        if err != nil {
            w.WriteHeader(http.StatusServiceUnavailable)
            w.Write([]byte(fmt.Sprintf("health check error: %v", err)))
            return
        }

        if !healthy {
            w.WriteHeader(http.StatusServiceUnavailable)
            w.Write([]byte("not ready"))
            return
        }

        w.WriteHeader(http.StatusOK)
        w.Write([]byte("ready"))
    })

    api := rest.NewApi(
        "My Service",
        "1.0.0",
        rest.Readiness(readinessHandler),
    )

    return api, nil
}
```

**Behavior:**
- **Fail-fast** - Stops checking after the first unhealthy monitor
- **Returns immediately** on the first error encountered
- **All must pass** for the combined check to be healthy

### OrMonitor - Check All Composition

Combines multiple monitors with logical OR (||) semantics. Returns healthy if **any** monitor is healthy.

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Create monitors for primary and fallback databases
    var primaryDBHealth health.Binary
    var fallbackDBHealth health.Binary

    // Combine with OR - at least one must be healthy
    dbMonitor := health.Or(&primaryDBHealth, &fallbackDBHealth)

    // Initialize primary database
    primaryDB, err := sql.Open("postgres", cfg.PrimaryDatabaseURL)
    if err == nil {
        primaryDBHealth.MarkHealthy()
    }

    // Initialize fallback database
    fallbackDB, err := sql.Open("postgres", cfg.FallbackDatabaseURL)
    if err == nil {
        fallbackDBHealth.MarkHealthy()
    }

    // Create handler - service is ready if either DB is available
    readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        healthy, err := dbMonitor.Healthy(r.Context())
        if err != nil {
            // OrMonitor collects all errors via errors.Join
            w.WriteHeader(http.StatusServiceUnavailable)
            w.Write([]byte(fmt.Sprintf("all health checks failed: %v", err)))
            return
        }

        if !healthy {
            w.WriteHeader(http.StatusServiceUnavailable)
            w.Write([]byte("no healthy database available"))
            return
        }

        w.WriteHeader(http.StatusOK)
        w.Write([]byte("ready"))
    })

    api := rest.NewApi(
        "My Service",
        "1.0.0",
        rest.Readiness(readinessHandler),
    )

    return api, nil
}
```

**Behavior:**
- **Checks all monitors** even if one is healthy
- **Collects all errors** and returns them joined via `errors.Join()`
- **At least one must pass** for the combined check to be healthy

## Custom Monitor Implementation

Implement the `health.Monitor` interface for custom health logic:

```go
import (
    "context"
    "database/sql"
    "github.com/z5labs/humus/health"
)

// DatabaseMonitor checks database connectivity
type DatabaseMonitor struct {
    db *sql.DB
}

func NewDatabaseMonitor(db *sql.DB) *DatabaseMonitor {
    return &DatabaseMonitor{db: db}
}

func (m *DatabaseMonitor) Healthy(ctx context.Context) (bool, error) {
    // Ping with timeout from context
    err := m.db.PingContext(ctx)
    if err != nil {
        return false, err
    }
    return true, nil
}

// CacheMonitor checks cache connectivity
type CacheMonitor struct {
    client *redis.Client
}

func NewCacheMonitor(client *redis.Client) *CacheMonitor {
    return &CacheMonitor{client: client}
}

func (m *CacheMonitor) Healthy(ctx context.Context) (bool, error) {
    err := m.client.Ping(ctx).Err()
    if err != nil {
        return false, err
    }
    return true, nil
}

// Usage
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    db, err := sql.Open("postgres", cfg.DatabaseURL)
    if err != nil {
        return nil, err
    }

    cache, err := initRedis(cfg)
    if err != nil {
        return nil, err
    }

    // Combine custom monitors
    dbMonitor := NewDatabaseMonitor(db)
    cacheMonitor := NewCacheMonitor(cache)

    readinessMonitor := health.And(dbMonitor, cacheMonitor)

    readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        healthy, err := readinessMonitor.Healthy(r.Context())
        if err != nil {
            w.WriteHeader(http.StatusServiceUnavailable)
            json.NewEncoder(w).Encode(map[string]string{
                "status": "unhealthy",
                "error":  err.Error(),
            })
            return
        }

        if !healthy {
            w.WriteHeader(http.StatusServiceUnavailable)
            json.NewEncoder(w).Encode(map[string]string{
                "status": "unhealthy",
            })
            return
        }

        w.WriteHeader(http.StatusOK)
        json.NewEncoder(w).Encode(map[string]string{
            "status": "healthy",
        })
    })

    api := rest.NewApi(
        "My Service",
        "1.0.0",
        rest.Readiness(readinessHandler),
    )

    return api, nil
}
```

## Kubernetes Integration

Configure liveness and readiness probes in your Kubernetes deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-service
spec:
  template:
    spec:
      containers:
      - name: my-service
        image: my-service:latest
        ports:
        - containerPort: 8080

        # Liveness probe - restart if unhealthy
        livenessProbe:
          httpGet:
            path: /health/liveness
            port: 8080
          initialDelaySeconds: 10
          periodSeconds: 30
          timeoutSeconds: 5
          failureThreshold: 3

        # Readiness probe - remove from load balancer if unhealthy
        readinessProbe:
          httpGet:
            path: /health/readiness
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 10
          timeoutSeconds: 3
          failureThreshold: 2
          successThreshold: 1
```

## Best Practices

### 1. Separate Liveness from Readiness

**Liveness** should detect unrecoverable states:
- Application deadlocks
- Out of memory conditions
- Corrupted internal state

**Readiness** should detect recoverable dependencies:
- Database connectivity
- External API availability
- Cache connectivity

```go
// Liveness - simple alive check
livenessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    // Just return OK - server is running if it responds
    w.WriteHeader(http.StatusOK)
})

// Readiness - dependency checks
readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    // Check database, cache, etc.
    if !checkDependencies(r.Context()) {
        w.WriteHeader(http.StatusServiceUnavailable)
        return
    }
    w.WriteHeader(http.StatusOK)
})

api := rest.NewApi(
    "My Service",
    "1.0.0",
    rest.Liveness(livenessHandler),
    rest.Readiness(readinessHandler),
)
```

### 2. Use Context Timeouts

Health checks should respect context deadlines:

```go
func (m *DatabaseMonitor) Healthy(ctx context.Context) (bool, error) {
    // Add timeout if context doesn't have one
    ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
    defer cancel()

    err := m.db.PingContext(ctx)
    return err == nil, err
}
```

### 3. Mark Unhealthy During Graceful Shutdown

Prevent new traffic during shutdown:

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    var appHealth health.Binary
    appHealth.MarkHealthy()

    // ... initialization ...

    lc, _ := lifecycle.FromContext(ctx)
    lc.OnPreShutdown(lifecycle.HookFunc(func(ctx context.Context) error {
        // Stop accepting new requests
        appHealth.MarkUnhealthy()
        // Give load balancer time to detect
        time.Sleep(5 * time.Second)
        return nil
    }))

    return api, nil
}
```

### 4. Return Descriptive Error Messages

Help operators diagnose issues:

```go
readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    type HealthStatus struct {
        Status     string            `json:"status"`
        Components map[string]string `json:"components,omitempty"`
    }

    status := HealthStatus{
        Status:     "healthy",
        Components: make(map[string]string),
    }

    // Check database
    if err := db.PingContext(r.Context()); err != nil {
        status.Status = "unhealthy"
        status.Components["database"] = err.Error()
    } else {
        status.Components["database"] = "healthy"
    }

    // Check cache
    if err := cache.Ping(r.Context()).Err(); err != nil {
        status.Status = "unhealthy"
        status.Components["cache"] = err.Error()
    } else {
        status.Components["cache"] = "healthy"
    }

    if status.Status == "unhealthy" {
        w.WriteHeader(http.StatusServiceUnavailable)
    } else {
        w.WriteHeader(http.StatusOK)
    }

    json.NewEncoder(w).Encode(status)
})
```

### 5. Avoid Heavy Operations

Health checks run frequently - keep them lightweight:

```go
// Good - quick ping
func (m *DatabaseMonitor) Healthy(ctx context.Context) (bool, error) {
    return m.db.PingContext(ctx) == nil, nil
}

// Bad - expensive query
func (m *DatabaseMonitor) Healthy(ctx context.Context) (bool, error) {
    var count int
    err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM large_table").Scan(&count)
    return err == nil, err
}
```

### 6. Test Health Checks

Write tests for your health monitoring logic:

```go
func TestDatabaseMonitor(t *testing.T) {
    // Setup test database
    db, mock, err := sqlmock.New()
    require.NoError(t, err)
    defer db.Close()

    monitor := NewDatabaseMonitor(db)

    t.Run("returns healthy when database is connected", func(t *testing.T) {
        mock.ExpectPing()

        healthy, err := monitor.Healthy(context.Background())

        require.NoError(t, err)
        assert.True(t, healthy)
    })

    t.Run("returns unhealthy when database is disconnected", func(t *testing.T) {
        mock.ExpectPing().WillReturnError(sql.ErrConnDone)

        healthy, err := monitor.Healthy(context.Background())

        require.Error(t, err)
        assert.False(t, healthy)
    })
}
```

## Complete Example

```go
package main

import (
    "context"
    "database/sql"
    "encoding/json"
    "net/http"
    "time"

    "github.com/z5labs/bedrock/lifecycle"
    "github.com/z5labs/humus/health"
    "github.com/z5labs/humus/rest"
)

type DatabaseMonitor struct {
    db *sql.DB
}

func (m *DatabaseMonitor) Healthy(ctx context.Context) (bool, error) {
    ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
    defer cancel()

    err := m.db.PingContext(ctx)
    return err == nil, err
}

type Config struct {
    rest.Config `config:",squash"`
    DatabaseURL string `config:"database_url"`
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Initialize database
    db, err := sql.Open("postgres", cfg.DatabaseURL)
    if err != nil {
        return nil, err
    }

    // Create health monitors
    var appHealth health.Binary
    dbMonitor := &DatabaseMonitor{db: db}

    // Combine for readiness
    readinessMonitor := health.And(&appHealth, dbMonitor)

    // Create health handlers
    livenessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
        w.Write([]byte("alive"))
    })

    readinessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        healthy, err := readinessMonitor.Healthy(r.Context())

        status := map[string]interface{}{
            "status": "healthy",
        }

        if err != nil || !healthy {
            status["status"] = "unhealthy"
            if err != nil {
                status["error"] = err.Error()
            }
            w.WriteHeader(http.StatusServiceUnavailable)
        } else {
            w.WriteHeader(http.StatusOK)
        }

        json.NewEncoder(w).Encode(status)
    })

    // Create API
    api := rest.NewApi(
        "My Service",
        "1.0.0",
        rest.Liveness(livenessHandler),
        rest.Readiness(readinessHandler),
        // ... your operations
    )

    // Mark healthy after initialization
    appHealth.MarkHealthy()

    // Mark unhealthy during shutdown
    lc, _ := lifecycle.FromContext(ctx)
    lc.OnPreShutdown(lifecycle.HookFunc(func(ctx context.Context) error {
        appHealth.MarkUnhealthy()
        time.Sleep(5 * time.Second) // Grace period
        return nil
    }))
    lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
        return db.Close()
    }))

    return api, nil
}

func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)
}
```

## Next Steps

- Learn about [OpenAPI]({{< ref "openapi" >}}) generation and documentation
- Explore [Error Handling]({{< ref "error-handling" >}}) for custom error responses
- Read [Authentication]({{< ref "authentication" >}}) for securing health endpoints (if needed)
- See the [health package documentation](https://pkg.go.dev/github.com/z5labs/humus/health) for API reference
