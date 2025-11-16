---
title: Configuration System
description: Deep dive into config composition and templating
weight: 10
type: docs
---


Humus uses a powerful configuration system built on YAML with Go template support and multi-source composition.

## Configuration Anatomy

### Basic Structure

A Humus configuration file has three main sections:

```yaml
# 1. Service Configuration (REST, gRPC, or omitted for Jobs)
rest:
  port: 8080
  host: 0.0.0.0

# 2. OpenTelemetry Configuration (optional but recommended)
otel:
  service:
    name: my-service
    version: 1.0.0

# 3. Application-Specific Configuration
database:
  host: localhost
  port: 5432
```

### Config Struct Mapping

The YAML maps to a Go struct:

```go
type Config struct {
    // 1. Service config (embedded with squash)
    rest.Config `config:",squash"`

    // 2. OTel is embedded in rest.Config/grpc.Config
    // No need to explicitly include it

    // 3. Application-specific fields
    Database struct {
        Host string `config:"host"`
        Port int    `config:"port"`
    } `config:"database"`
}
```

## Template Engine

### Template Functions

Humus supports Go template syntax with two key functions:

#### `env` - Read Environment Variables

```yaml
otel:
  service:
    name: {{env "SERVICE_NAME"}}
```

Reads the `SERVICE_NAME` environment variable.

#### `default` - Provide Fallback Values

```yaml
rest:
  port: {{env "PORT" | default "8080"}}
```

Uses `PORT` environment variable, falling back to `"8080"` if not set.

### Template Examples

**Database Configuration:**
```yaml
database:
  host: {{env "DB_HOST" | default "localhost"}}
  port: {{env "DB_PORT" | default "5432"}}
  name: {{env "DB_NAME"}}
  user: {{env "DB_USER"}}
  password: {{env "DB_PASSWORD"}}  # No default for secrets!
```

**Feature Flags:**
```yaml
features:
  enable_cache: {{env "ENABLE_CACHE" | default "true"}}
  enable_auth: {{env "ENABLE_AUTH" | default "false"}}
```

**Environment-Specific Values:**
```yaml
otel:
  sdk:
    disabled: {{env "OTEL_DISABLED" | default "false"}}

  traces:
    exporter:
      otlp:
        endpoint: {{env "OTEL_ENDPOINT" | default "http://localhost:4318"}}
```

## Multi-Source Configuration

Compose multiple configuration files with `config.MultiSource`:

```go
import (
    bedrockcfg "github.com/z5labs/bedrock/pkg/config"
    "github.com/z5labs/humus/rest"
)

func main() {
    source := bedrockcfg.MultiSource(
        bedrockcfg.FromYaml("defaults.yaml"),    // Base configuration
        bedrockcfg.FromYaml("config.yaml"),      // Overrides
    )

    rest.Run(source, Init)
}
```

### Use Cases for Multi-Source

#### 1. Framework Defaults + App Config

```go
source := bedrockcfg.MultiSource(
    bedrockcfg.FromYaml("default_config.yaml"),  // Humus framework defaults
    bedrockcfg.FromYaml("config.yaml"),          // Your application config
)
```

**default_config.yaml** (framework):
```yaml
otel:
  service:
    name: unnamed-service
  sdk:
    disabled: false
```

**config.yaml** (your app):
```yaml
otel:
  service:
    name: my-actual-service  # Overrides framework default
```

#### 2. Environment-Specific Overrides

```go
import "os"

func main() {
    env := os.Getenv("ENV")
    if env == "" {
        env = "dev"
    }

    sources := []bedrockcfg.Source{
        bedrockcfg.FromYaml("config.base.yaml"),
    }

    envConfig := fmt.Sprintf("config.%s.yaml", env)
    if _, err := os.Stat(envConfig); err == nil {
        sources = append(sources, bedrockcfg.FromYaml(envConfig))
    }

    rest.Run(bedrockcfg.MultiSource(sources...), Init)
}
```

**config.base.yaml:**
```yaml
rest:
  port: 8080

otel:
  service:
    name: my-service
```

**config.prod.yaml:**
```yaml
rest:
  host: 0.0.0.0  # Only override what changes

otel:
  traces:
    exporter:
      otlp:
        endpoint: https://otel-collector.prod.example.com
```

#### 3. Local Development Overrides

```go
source := bedrockcfg.MultiSource(
    bedrockcfg.FromYaml("config.yaml"),
    bedrockcfg.FromYaml("config.local.yaml"),  // Gitignored local overrides
)
```

Add to `.gitignore`:
```
config.local.yaml
```

Developers can create `config.local.yaml` for personal settings without affecting others.

## Struct Tags

### The `squash` Tag

Embeds fields directly into the parent:

```go
type Config struct {
    rest.Config `config:",squash"`  // Fields embedded at root level
}
```

**Without squash:**
```yaml
rest_config:  # Would need this nesting
  port: 8080
```

**With squash:**
```yaml
rest:  # Direct access
  port: 8080
```

### Custom Field Names

```go
type Config struct {
    DatabaseURL string `config:"database_url"`  // Maps to database_url in YAML
    APIKey      string `config:"api_key"`       // Maps to api_key
}
```

### Nested Structures

```go
type Config struct {
    rest.Config `config:",squash"`

    Database struct {
        Primary struct {
            Host string `config:"host"`
            Port int    `config:"port"`
        } `config:"primary"`

        Replica struct {
            Host string `config:"host"`
            Port int    `config:"port"`
        } `config:"replica"`
    } `config:"database"`
}
```

**Corresponding YAML:**
```yaml
database:
  primary:
    host: primary.db.example.com
    port: 5432
  replica:
    host: replica.db.example.com
    port: 5432
```

## OpenTelemetry Configuration

### Full OTel Config Structure

```yaml
otel:
  service:
    name: my-service        # Required
    version: 1.0.0          # Optional
    namespace: production   # Optional
    instance_id: pod-1234   # Optional

  sdk:
    disabled: false         # Set true to disable all OTel

  # Resource attributes (optional)
  resource:
    attributes:
      deployment.environment: production
      service.team: platform

  # Trace configuration
  traces:
    sampler:
      type: parentbased_traceidratio  # or always_on, always_off, etc.
      arg: 0.1  # Sample 10% of traces

    exporter:
      otlp:
        endpoint: http://localhost:4318
        protocol: http/protobuf  # or grpc
        headers:
          x-custom-header: value

  # Metrics configuration
  metrics:
    exporter:
      otlp:
        endpoint: http://localhost:4318
        protocol: http/protobuf

  # Logs configuration
  logs:
    exporter:
      otlp:
        endpoint: http://localhost:4318
        protocol: http/protobuf
```

### Disabling OTel

For development or testing:

```yaml
otel:
  sdk:
    disabled: true
```

Or via environment variable:

```yaml
otel:
  sdk:
    disabled: {{env "OTEL_DISABLED" | default "false"}}
```

## Best Practices

### 1. Secrets Management

**Never commit secrets:**
```yaml
# Bad
database:
  password: super-secret-password

# Good
database:
  password: {{env "DB_PASSWORD"}}
```

### 2. Required vs Optional

**Use templates for optional values:**
```yaml
rest:
  port: {{env "PORT" | default "8080"}}  # Optional, has default
```

**Validate required values in Init:**
```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    if cfg.Database.Password == "" {
        return nil, fmt.Errorf("DB_PASSWORD environment variable required")
    }
    // ...
}
```

### 3. Environment Variable Naming

Use consistent prefixes:
```yaml
# Good
database:
  host: {{env "MYAPP_DB_HOST"}}
  port: {{env "MYAPP_DB_PORT"}}

# Avoids conflicts with other apps
```

### 4. Document Your Config

Add comments to YAML files:
```yaml
# HTTP Server Configuration
rest:
  # Port to listen on. Set via PORT environment variable.
  # Default: 8080
  port: {{env "PORT" | default "8080"}}

  # Host to bind to. Use 0.0.0.0 for all interfaces.
  # Default: localhost (for security)
  host: {{env "HOST" | default "localhost"}}
```

### 5. Config Validation

Validate in `Init` function:
```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Validate ranges
    if cfg.REST.Port < 1024 || cfg.REST.Port > 65535 {
        return nil, fmt.Errorf("port must be between 1024 and 65535")
    }

    // Validate required fields
    if cfg.Database.Host == "" {
        return nil, fmt.Errorf("database host is required")
    }

    // Validate mutually exclusive options
    if cfg.Features.UseCache && cfg.Features.UseMemory {
        return nil, fmt.Errorf("cannot enable both cache and memory mode")
    }

    // Continue with initialization...
}
```

## Next Steps

- Learn about [Observability]({{< ref "observability" >}}) to understand OTel configuration
- Explore [Lifecycle Management]({{< ref "lifecycle-management" >}}) for runtime behavior
- See [Getting Started - Configuration]({{< ref "/getting-started/configuration" >}}) for basic examples
