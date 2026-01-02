---
title: Core Concepts
description: Understanding Humus architecture and patterns
weight: 2
type: docs
---


This section covers the fundamental concepts and patterns that power Humus applications.

## What You'll Learn

Understanding these core concepts will help you build better applications with Humus:

- [Configuration System]({{< ref "configuration-system" >}}) - Type-safe config with config.Reader[T]
- [Observability]({{< ref "observability" >}}) - OpenTelemetry integration for traces, metrics, and logs
- [Lifecycle Management]({{< ref "lifecycle-management" >}}) - Graceful shutdown and signal handling

## Architecture Overview

Humus is built on [Bedrock](https://github.com/z5labs/bedrock), a foundational framework for application lifecycle management. Humus extends Bedrock with:

1. **Service-Specific Builders** - Pre-configured builders for REST, gRPC, and Job services
2. **Automatic Instrumentation** - Built-in OpenTelemetry integration
3. **Standardized Patterns** - Consistent interfaces across service types

## Key Principles

### Convention Over Configuration

Humus provides sensible defaults so you can start quickly. Override only what you need:

```go
// Minimal configuration using defaults
listener := httpserver.NewTCPListener(
    httpserver.Addr(config.Default(":8080", config.Env("HTTP_ADDR"))),
)
```

### Composition Over Inheritance

Build complex applications by composing simple pieces:

```go
// Compose multiple configuration readers
port := config.Or(
    config.Env("PORT"),
    config.Env("HTTP_PORT"),
    config.ReaderOf("8080"),
)
```

### Separation of Concerns

Humus separates:
- **Configuration** - Declarative config readers
- **Building** - Application builders that construct components
- **Execution** - Runtime that manages lifecycle

```go
func main() {
    // Configure components
    listener := httpserver.NewTCPListener(...)
    srv := httpserver.NewServer(listener)
    
    // Build application
    api := rest.NewApi(...)
    restBuilder := rest.Build(srv, api)
    
    // Execute with lifecycle management
    app.Run(context.Background(), restBuilder)
}
```

## Common Patterns

### Builder Pattern

Every Humus service uses builders that construct applications:

```go
// Build HTTP application
restBuilder := rest.Build(srv, api)

// Wrap with OpenTelemetry
otelBuilder := otel.Build(sdk, restBuilder)

// Run
app.Run(ctx, otelBuilder)
```

Builders are composable - you can wrap one builder with another to add functionality.

### Configuration Readers

Use `config.Reader[T]` for all configuration:

```go
// Simple default
port := config.Default("8080", config.Env("PORT"))

// Multiple fallbacks
addr := config.Or(
    config.Env("HTTP_ADDR"),
    config.Env("ADDR"),
    config.ReaderOf(":8080"),
)

// Transform values
timeout := config.Map(
    config.Env("TIMEOUT"),
    func(ctx context.Context, s string) (time.Duration, error) {
        return time.ParseDuration(s)
    },
)
```

### Error Handling

Builders return errors during construction to fail fast:

```go
listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
    addr := config.MustOr(ctx, ":8080", config.Env("HTTP_ADDR"))
    ln, err := net.Listen("tcp", addr)
    if err != nil {
        return config.Value[net.Listener]{}, fmt.Errorf("failed to create listener: %w", err)
    }
    return config.ValueOf(ln), nil
})
```

## Next Steps

Dive deeper into specific concepts:

- [Configuration System]({{< ref "configuration-system" >}}) - Master config composition
- [Observability]({{< ref "observability" >}}) - Learn about traces, metrics, and logs
- [Lifecycle Management]({{< ref "lifecycle-management" >}}) - Understand graceful shutdown

Or explore service-specific features:

- [REST Services]({{< ref "/features/rest" >}})
- [gRPC Services]({{< ref "/features/grpc" >}})
- [Job Services]({{< ref "/features/job" >}})

