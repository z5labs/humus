---
title: Configuration
description: Understanding the config.Reader system
weight: 30
type: docs
---


Humus uses a type-safe configuration system based on `config.Reader[T]`. This provides composable, environment-aware configuration without YAML files.

## Basic Configuration

The `config.Reader[T]` interface allows you to read configuration values from various sources:

```go
type Reader[T any] interface {
    Read(context.Context) (Value[T], error)
}
```

### Static Values

For fixed configuration values, use `config.ReaderOf()`:

```go
listener := httpserver.NewTCPListener(
    httpserver.Addr(config.ReaderOf(":8080")),
)
```

### Environment Variables

Read from environment variables using `config.Env()`:

```go
listener := httpserver.NewTCPListener(
    httpserver.Addr(config.Env("HTTP_ADDR")),
)
```

### Default Values

Provide a default value using `config.Default()`:

```go
addr := config.Default(":8080", config.Env("HTTP_ADDR"))
```

This reads from `HTTP_ADDR` environment variable, falling back to `":8080"` if not set.

## Configuration Composition

### Multiple Sources

Try multiple sources in order using `config.Or()`:

```go
port := config.Or(
    config.Env("PORT"),           // Try PORT first
    config.Env("HTTP_PORT"),      // Then HTTP_PORT
    config.ReaderOf("8080"),      // Finally default to 8080
)
```

Use `config.Or()` when you need to try multiple environment variables or sources. For a single source with a default, prefer `config.Default()`.

### Transforming Values

Use `config.Map()` to transform configuration values:

```go
// Convert string port to full address
addr := config.Map(
    config.Env("PORT"),
    func(ctx context.Context, port string) (string, error) {
        return ":" + port, nil
    },
)
```

### Complex Configuration

For complex setup, use `config.ReaderFunc`:

```go
listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
    addr := config.MustOr(ctx, ":8080", config.Env("HTTP_ADDR"))
    ln, err := net.Listen("tcp", addr)
    if err != nil {
        return config.Value[net.Listener]{}, err
    }
    return config.ValueOf(ln), nil
})
```

## OpenTelemetry Configuration

### Basic OpenTelemetry Setup

Humus provides `otel` package for OpenTelemetry configuration:

```go
import (
    "github.com/z5labs/humus/otel"
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/trace"
)

// Configure resource with service information
resource := otel.NewResource(
    otel.ServiceName(config.ReaderOf("my-service")),
    otel.ServiceVersion(config.ReaderOf("1.0.0")),
)

// Configure tracing
traces := otel.NewTraces(
    otel.TraceResource(resource),
    otel.TraceSampler(config.ReaderOf(1.0)),  // Sample 100% of traces
)

// Configure metrics
metrics := otel.NewMetrics(
    otel.MetricResource(resource),
)

// Build SDK
sdk := otel.SDK{
    TracerProvider: traces,
    MeterProvider:  metrics,
}
```

### Environment-Based Configuration

Use environment variables for production configuration:

```go
resource := otel.NewResource(
    otel.ServiceName(otel.ServiceNameFromEnv()),     // OTEL_SERVICE_NAME
    otel.ServiceVersion(otel.ServiceVersionFromEnv()), // OTEL_SERVICE_VERSION
)

traces := otel.NewTraces(
    otel.TraceResource(resource),
    otel.TraceSampler(otel.SamplerRatioFromEnv()), // OTEL_TRACES_SAMPLER_RATIO
)
```

### Disabling OpenTelemetry

For development or testing, disable OpenTelemetry by providing empty readers:

```go
sdk := otel.SDK{
    TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
        return config.Value[trace.TracerProvider]{}, nil
    }),
    MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
        return config.Value[metric.MeterProvider]{}, nil
    }),
}
```

## Complete Example

Here's a complete example showing configuration composition:

```go
package main

import (
    "context"
    "net"

    "github.com/z5labs/humus/app"
    "github.com/z5labs/humus/config"
    httpserver "github.com/z5labs/humus/http"
    "github.com/z5labs/humus/otel"
    "github.com/z5labs/humus/rest"
)

func main() {
    // Configure HTTP address with fallback
    listener := httpserver.NewTCPListener(
        httpserver.Addr(config.Default(":8080", config.Env("HTTP_ADDR"))),
    )

    srv := httpserver.NewServer(listener)
    
    // Build your API
    api := rest.NewApi("My Service", "1.0.0")
    
    restBuilder := rest.Build(srv, api)

    // Configure OpenTelemetry from environment
    resource := otel.NewResource(
        otel.ServiceName(config.Default("my-service", otel.ServiceNameFromEnv())),
        otel.ServiceVersion(config.Default("dev", otel.ServiceVersionFromEnv())),
    )

    traces := otel.NewTraces(
        otel.TraceResource(resource),
        otel.TraceSampler(config.Default(1.0, otel.SamplerRatioFromEnv())),
    )

    sdk := otel.SDK{
        TracerProvider: traces,
        MeterProvider:  otel.NewMetrics(otel.MetricResource(resource)),
    }

    otelBuilder := otel.Build(sdk, restBuilder)

    _ = app.Run(context.Background(), otelBuilder)
}
```

## Best Practices

1. **Use Environment Variables for Secrets**: Never hardcode credentials. Use `config.Env()`:
   ```go
   password := config.Env("DB_PASSWORD")
   ```

2. **Provide Defaults**: Use `config.Default()` for single source with fallback:
   ```go
   port := config.Default("8080", config.Env("PORT"))
   ```

   Or `config.Or()` for multiple sources:
   ```go
   port := config.Or(config.Env("PORT"), config.Env("HTTP_PORT"), config.ReaderOf("8080"))
   ```

3. **Separate Environments**: Use environment variables to differentiate dev/staging/prod.

4. **Validate Early**: Validate configuration in your builders:
   ```go
   listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
       addr := config.MustOr(ctx, ":8080", config.Env("HTTP_ADDR"))
       if addr == "" {
           return config.Value[net.Listener]{}, fmt.Errorf("HTTP_ADDR is required")
       }
       ln, err := net.Listen("tcp", addr)
       if err != nil {
           return config.Value[net.Listener]{}, err
       }
       return config.ValueOf(ln), nil
   })
   ```

5. **Use config.MustOr for Required Values**: When you need a value immediately in a function body:
   ```go
   func buildDatabase(ctx context.Context) (*sql.DB, error) {
       url := config.MustOr(ctx, "postgres://localhost/db", config.Env("DATABASE_URL"))
       return sql.Open("postgres", url)
   }
   ```

## Next Steps

- Learn about [Project Structure]({{< ref "project-structure" >}}) for organizing your application
- Explore [Core Concepts]({{< ref "/concepts/configuration-system" >}}) for advanced configuration patterns
- See [Observability]({{< ref "/concepts/observability" >}}) for more OTel configuration details

