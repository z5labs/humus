---
title: Core Concepts
description: Understanding Humus architecture and patterns
weight: 2
type: docs
---


This section covers the fundamental concepts and patterns that power Humus applications.

## What You'll Learn

Understanding these core concepts will help you build better applications with Humus:

- [Configuration System]({{< ref "configuration-system" >}}) - Deep dive into YAML config composition and templating
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
// Minimal configuration needed
type Config struct {
    rest.Config `config:",squash"`
}
```

### Composition Over Inheritance

Build complex applications by composing simple pieces:

```go
// Compose multiple configuration sources
source := config.MultiSource(
    config.FromYaml("defaults.yaml"),
    config.FromYaml("overrides.yaml"),
)
```

### Separation of Concerns

Humus separates:
- **Configuration** - What to run
- **Initialization** - How to build it
- **Execution** - When to run it

```go
func main() {
    // Configuration source
    source := rest.YamlSource("config.yaml")

    // Initialization function
    init := app.Init

    // Execution
    rest.Run(source, init)
}
```

## Common Patterns

### The Init Function

Every Humus service has an `Init` function that receives configuration and returns the service:

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Build and return your service
}
```

This function is called after configuration is loaded but before the service starts.

### Configuration Embedding

Embed framework configs to inherit standard fields:

```go
type Config struct {
    rest.Config `config:",squash"`  // Inherits HTTP server config
    Database DatabaseConfig `config:"database"`  // Your custom config
}
```

### Error Handling

Use errors to fail fast during initialization:

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    if cfg.Database.Host == "" {
        return nil, fmt.Errorf("database host required")
    }
    // ...
}
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
