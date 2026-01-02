---
title: Configuration System
description: Deep dive into config.Reader composition
weight: 10
type: docs
---


Humus uses a powerful type-safe configuration system built on the `config.Reader[T]` interface. This enables composable, testable configuration from multiple sources without YAML files.

## Configuration Anatomy

### The Reader Interface

All configuration in Humus is based on `config.Reader[T]`:

```go
type Reader[T any] interface {
    Read(context.Context) (Value[T], error)
}

type Value[T any] struct {
    val T
    set bool
}
```

A `Reader` can read a value of type `T` from any source: environment variables, files, command-line flags, or even remote configuration services.

### Basic Readers

**Static values:**
```go
port := config.ReaderOf("8080")
```

**Environment variables:**
```go
port := config.Env("PORT")
```

**Functions:**
```go
listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
    ln, err := net.Listen("tcp", ":8080")
    if err != nil {
        return config.Value[net.Listener]{}, err
    }
    return config.ValueOf(ln), nil
})
```

## Reader Composition

### Fallback Values with Or

Use `config.Or()` to try multiple readers in sequence:

```go
// Try PORT, then HTTP_PORT, finally default to 8080
port := config.Or(
    config.Env("PORT"),
    config.Env("HTTP_PORT"),
    config.ReaderOf("8080"),
)
```

### Default Values

Provide a default for a single reader:

```go
port := config.Default("8080", config.Env("PORT"))
```

This reads from the `PORT` environment variable, and if not set, uses `"8080"` as the default.

### Transforming Values with Map

Transform a value after reading it:

```go
// Read port number and convert to full address
addr := config.Map(
    config.Env("PORT"),
    func(ctx context.Context, port string) (string, error) {
        return ":" + port, nil
    },
)
```

Another example - parsing a duration:

```go
timeout := config.Map(
    config.Env("TIMEOUT"),
    func(ctx context.Context, s string) (time.Duration, error) {
        return time.ParseDuration(s)
    },
)
```

### Chaining with Bind

Use `config.Bind()` for dependent configuration:

```go
// Read database type, then create appropriate connection string reader
connStr := config.Bind(
    config.Env("DB_TYPE"),
    func(ctx context.Context, dbType string) config.Reader[string] {
        switch dbType {
        case "postgres":
            return config.Env("POSTGRES_URL")
        case "mysql":
            return config.Env("MYSQL_URL")
        default:
            return config.ReaderOf("sqlite::memory:")
        }
    },
)
```

## Reading Configuration

### Immediate Reading with Must

Use `config.Must()` to read a value immediately, panicking on error:

```go
func main() {
    ctx := context.Background()
    port := config.Must(ctx, config.Env("PORT"))
    // ...
}
```

### Safe Reading with MustOr

Prefer `config.MustOr()` to read with a default:

```go
func buildServer(ctx context.Context) *http.Server {
    addr := config.MustOr(ctx, ":8080", config.Env("HTTP_ADDR"))
    return &http.Server{Addr: addr}
}
```

### Deferred Reading in Builders

Most configuration should be read lazily within builders:

```go
listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
    // Read happens at build time, not declaration time
    addr := config.MustOr(ctx, ":8080", config.Env("HTTP_ADDR"))
    ln, err := net.Listen("tcp", addr)
    if err != nil {
        return config.Value[net.Listener]{}, err
    }
    return config.ValueOf(ln), nil
})
```

This allows configuration to be resolved when `app.Run()` builds the application, giving full access to the context.

## Common Patterns

### HTTP Server Configuration

```go
listener := httpserver.NewTCPListener(
    httpserver.Addr(config.Default(":8080", config.Env("HTTP_ADDR"))),
)

srv := httpserver.NewServer(
    listener,
    httpserver.ReadTimeout(config.Map(
        config.Env("HTTP_READ_TIMEOUT"),
        func(ctx context.Context, s string) (time.Duration, error) {
            return time.ParseDuration(s)
        },
    )),
    httpserver.WriteTimeout(config.Map(
        config.Env("HTTP_WRITE_TIMEOUT"),
        func(ctx context.Context, s string) (time.Duration, error) {
            return time.ParseDuration(s)
        },
    )),
)
```

### Database Configuration

```go
db := config.ReaderFunc[*sql.DB](func(ctx context.Context) (config.Value[*sql.DB], error) {
    url := config.MustOr(ctx, "postgres://localhost/mydb", config.Env("DATABASE_URL"))
    
    conn, err := sql.Open("postgres", url)
    if err != nil {
        return config.Value[*sql.DB]{}, err
    }
    
    if err := conn.PingContext(ctx); err != nil {
        conn.Close()
        return config.Value[*sql.DB]{}, err
    }
    
    return config.ValueOf(conn), nil
})
```

### Feature Flags

```go
enableCache := config.Map(
    config.Default("false", config.Env("ENABLE_CACHE")),
    func(ctx context.Context, s string) (bool, error) {
        return strconv.ParseBool(s)
    },
)
```

## OpenTelemetry Configuration

### Resource Configuration

Define service metadata for OpenTelemetry:

```go
resource := otel.NewResource(
    otel.ServiceName(config.Default("my-service", otel.ServiceNameFromEnv())),
    otel.ServiceVersion(config.Default("1.0.0", otel.ServiceVersionFromEnv())),
)
```

### Trace Configuration

Configure distributed tracing:

```go
traces := otel.NewTraces(
    otel.TraceResource(resource),
    otel.TraceSampler(config.Default(1.0, otel.SamplerRatioFromEnv())),  // Sample 100% by default
    otel.TraceExportInterval(config.Map(
        config.Env("OTEL_BSP_EXPORT_INTERVAL"),
        func(ctx context.Context, s string) (time.Duration, error) {
            return time.ParseDuration(s)
        },
    )),
)
```

### Metrics Configuration

Configure metrics collection:

```go
metrics := otel.NewMetrics(
    otel.MetricResource(resource),
    otel.MetricExportInterval(config.Map(
        config.Env("OTEL_METRIC_EXPORT_INTERVAL"),
        func(ctx context.Context, s string) (time.Duration, error) {
            return time.ParseDuration(s)
        },
    )),
)
```

### Complete SDK Setup

Combine all OTel configuration:

```go
sdk := otel.SDK{
    TracerProvider: traces,
    MeterProvider:  metrics,
    LogProvider:    otel.NewLogs(otel.LogResource(resource)),
}

otelBuilder := otel.Build(sdk, appBuilder)
```

### Disabling OTel for Development

Disable OpenTelemetry entirely:

```go
import (
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/trace"
)

sdk := otel.SDK{
    TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
        return config.Value[trace.TracerProvider]{}, nil
    }),
    MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
        return config.Value[metric.MeterProvider]{}, nil
    }),
}
```

## Best Practices

### 1. Secrets Management

Never hardcode secrets - always use environment variables:

```go
// Bad - hardcoded secret
password := config.ReaderOf("my-password")

// Good - from environment
password := config.Env("DB_PASSWORD")

// Better - required secret with no default
password := config.ReaderFunc[string](func(ctx context.Context) (config.Value[string], error) {
    val, err := config.Env("DB_PASSWORD").Read(ctx)
    if err != nil {
        return config.Value[string]{}, err
    }
    pw, ok := val.Value()
    if !ok || pw == "" {
        return config.Value[string]{}, fmt.Errorf("DB_PASSWORD is required")
    }
    return config.ValueOf(pw), nil
})
```

### 2. Required vs Optional Configuration

Use `config.Default()` for optional values with sensible defaults:

```go
// Optional: has a sensible default
port := config.Default("8080", config.Env("PORT"))

// Required: no default, will error if not set
apiKey := config.Env("API_KEY")
```

Validate required config early in builders:

```go
db := config.ReaderFunc[*sql.DB](func(ctx context.Context) (config.Value[*sql.DB], error) {
    url, err := config.Read(ctx, config.Env("DATABASE_URL"))
    if err != nil {
        return config.Value[*sql.DB]{}, fmt.Errorf("DATABASE_URL is required: %w", err)
    }
    // ...
})
```

### 3. Environment Variable Naming

Use consistent, prefixed naming conventions:

```go
const appPrefix = "MYAPP_"

// Database configuration
dbHost := config.Env(appPrefix + "DB_HOST")
dbPort := config.Env(appPrefix + "DB_PORT")
dbName := config.Env(appPrefix + "DB_NAME")

// Or use a helper
func envVar(name string) config.Reader[string] {
    return config.Env(appPrefix + name)
}

dbHost := envVar("DB_HOST")
```

### 4. Testable Configuration

Configuration readers are testable:

```go
func TestDatabaseConnection(t *testing.T) {
    // Create a test reader
    testURL := config.ReaderOf("postgres://localhost:5432/testdb")
    
    // Use in your builder
    db := buildDatabase(testURL)
    // ...
}

func buildDatabase(urlReader config.Reader[string]) config.Reader[*sql.DB] {
    return config.ReaderFunc[*sql.DB](func(ctx context.Context) (config.Value[*sql.DB], error) {
        url, err := config.Read(ctx, urlReader)
        if err != nil {
            return config.Value[*sql.DB]{}, err
        }
        conn, err := sql.Open("postgres", url)
        // ...
    })
}
```

### 5. Configuration Documentation

Document environment variables your application uses:

```go
// Environment Variables:
//   HTTP_ADDR - HTTP server address (default: :8080)
//   DATABASE_URL - PostgreSQL connection string (required)
//   OTEL_SERVICE_NAME - Service name for telemetry (required)
//   OTEL_TRACES_SAMPLER_RATIO - Trace sampling ratio 0.0-1.0 (default: 1.0)
//   LOG_LEVEL - Logging level: debug, info, warn, error (default: info)
func main() {
    // ...
}
```

Or create a README section:

```markdown
## Configuration

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `HTTP_ADDR` | No | `:8080` | HTTP server listen address |
| `DATABASE_URL` | Yes | - | PostgreSQL connection URL |
| `OTEL_SERVICE_NAME` | Yes | - | Service name for traces/metrics |
```

## Next Steps

- Learn about [Observability]({{< ref "observability" >}}) for OpenTelemetry configuration details
- Explore [Lifecycle Management]({{< ref "lifecycle-management" >}}) for runtime behavior
- See [Getting Started - Configuration]({{< ref "/getting-started/configuration" >}}) for basic examples
