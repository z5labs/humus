---
title: Configuration
description: Understanding the YAML configuration system
weight: 30
type: docs
---


Humus uses YAML-based configuration with Go template support. This provides a flexible, environment-aware configuration system.

## Basic Configuration

A minimal configuration file looks like this:

```yaml
rest:
  port: 8080

otel:
  service:
    name: my-service
```

## Configuration Structure

### Service Type Sections

Each service type has its own configuration section:

**REST Services:**
```yaml
rest:
  port: 8080
  host: localhost  # optional, defaults to all interfaces
```

**gRPC Services:**
```yaml
grpc:
  port: 9090
  host: localhost  # optional
```

**Job Services:**
```yaml
# Jobs don't need server configuration
# Just OTel and app-specific config
```

### OpenTelemetry Configuration

The `otel` section configures observability:

```yaml
otel:
  service:
    name: my-service
    version: 1.0.0  # optional

  sdk:
    disabled: false  # Set to true to disable OTel entirely

  traces:
    exporter:
      otlp:
        endpoint: http://localhost:4318
        protocol: http/protobuf

  metrics:
    exporter:
      otlp:
        endpoint: http://localhost:4318
        protocol: http/protobuf

  logs:
    exporter:
      otlp:
        endpoint: http://localhost:4318
        protocol: http/protobuf
```

## Go Template Support

Configuration files support Go template syntax for dynamic values:

### Environment Variables

Use the `env` function to read environment variables:

```yaml
otel:
  service:
    name: {{env "SERVICE_NAME"}}

rest:
  port: {{env "PORT"}}
```

### Default Values

Use the `default` function to provide fallbacks:

```yaml
otel:
  service:
    name: {{env "SERVICE_NAME" | default "my-service"}}

rest:
  port: {{env "PORT" | default "8080"}}
```

### Complete Example

```yaml
rest:
  port: {{env "HTTP_PORT" | default "8080"}}
  host: {{env "HTTP_HOST" | default "0.0.0.0"}}

otel:
  service:
    name: {{env "OTEL_SERVICE_NAME" | default "my-service"}}
    version: {{env "APP_VERSION" | default "dev"}}

  sdk:
    disabled: {{env "OTEL_DISABLED" | default "false"}}

  traces:
    exporter:
      otlp:
        endpoint: {{env "OTEL_EXPORTER_OTLP_ENDPOINT" | default "http://localhost:4318"}}
```

## Configuration in Code

### Basic Config Struct

Embed the appropriate config type for your service:

```go
type Config struct {
    rest.Config `config:",squash"`  // For REST services
    // Add your custom config fields here
}
```

For gRPC:
```go
type Config struct {
    grpc.Config `config:",squash"`
}
```

For Jobs:
```go
type Config struct {
    humus.Config `config:",squash"`  // Base OTel config only
}
```

### Custom Configuration Fields

Add your own configuration fields using struct tags:

```go
type Config struct {
    rest.Config `config:",squash"`

    Database struct {
        Host     string `config:"host"`
        Port     int    `config:"port"`
        Name     string `config:"name"`
    } `config:"database"`

    Features struct {
        EnableCache bool `config:"enable_cache"`
    } `config:"features"`
}
```

Corresponding YAML:

```yaml
rest:
  port: 8080

database:
  host: localhost
  port: 5432
  name: mydb

features:
  enable_cache: true
```

## Configuration Sources

### YAML File

The most common source:

```go
rest.Run(rest.YamlSource("config.yaml"), Init)
```

### Multiple Sources

Use `bedrockcfg.MultiSource` to compose configurations:

```go
import (
    "github.com/z5labs/bedrock/pkg/config"
    bedrockcfg "github.com/z5labs/bedrock/pkg/config"
)

func main() {
    source := bedrockcfg.MultiSource(
        bedrockcfg.FromYaml("default_config.yaml"),  // Defaults
        bedrockcfg.FromYaml("config.yaml"),          // Overrides
    )

    rest.Run(source, Init)
}
```

### Environment-Specific Configs

```go
import "os"

func main() {
    env := os.Getenv("ENV")
    if env == "" {
        env = "dev"
    }

    configFile := fmt.Sprintf("config.%s.yaml", env)
    rest.Run(rest.YamlSource(configFile), Init)
}
```

This allows you to have:
- `config.dev.yaml`
- `config.staging.yaml`
- `config.prod.yaml`

## Default Configuration

Humus includes a `default_config.yaml` with sensible defaults for OpenTelemetry. You can compose this with your config:

```go
source := bedrockcfg.MultiSource(
    bedrockcfg.FromYaml("default_config.yaml"),  // Framework defaults
    bedrockcfg.FromYaml("config.yaml"),          // Your overrides
)
```

## Best Practices

1. **Use Environment Variables for Secrets**: Never commit credentials to YAML files. Use `env` function:
   ```yaml
   database:
     password: {{env "DB_PASSWORD"}}
   ```

2. **Provide Defaults**: Always use `default` with `env` for non-secret values:
   ```yaml
   port: {{env "PORT" | default "8080"}}
   ```

3. **Separate Environments**: Use different config files or environment variables for dev/staging/prod.

4. **Document Your Config**: Add comments to your YAML files explaining each section.

5. **Validate Early**: Use the `Init` function to validate configuration:
   ```go
   func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
       if cfg.Database.Host == "" {
           return nil, fmt.Errorf("database host is required")
       }
       // ...
   }
   ```

## Next Steps

- Learn about [Project Structure]({{< ref "project-structure" >}}) for organizing your config files
- Explore [Core Concepts]({{< ref "/concepts/configuration-system" >}}) for advanced configuration patterns
- See [Observability]({{< ref "/concepts/observability" >}}) for OTel configuration details
