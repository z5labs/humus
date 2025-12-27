---
title: Project Structure
description: Recommended project layout patterns
weight: 40
type: docs
---


This guide provides recommended patterns for organizing Humus applications.

## Simple Service

For small services with a single purpose:

```
my-service/
├── main.go              # Entry point
├── go.mod
├── go.sum
└── README.md
```

**main.go:**
```go
package main

import (
    "context"
    "net/http"
    
    "github.com/z5labs/humus/app"
    "github.com/z5labs/humus/config"
    httpserver "github.com/z5labs/humus/http"
    "github.com/z5labs/humus/otel"
    "github.com/z5labs/humus/rest"
    
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/trace"
)

func main() {
    listener := httpserver.NewTCPListener(
        httpserver.Addr(config.Default(":8080", config.Env("HTTP_ADDR"))),
    )
    srv := httpserver.NewServer(listener)
    
    api := rest.NewApi("My Service", "1.0.0")
    // Register handlers...
    
    restBuilder := rest.Build(srv, api)
    
    sdk := otel.SDK{
        TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
            return config.Value[trace.TracerProvider]{}, nil
        }),
        MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
            return config.Value[metric.MeterProvider]{}, nil
        }),
    }
    
    otelBuilder := otel.Build(sdk, restBuilder)
    _ = app.Run(context.Background(), otelBuilder)
}
```

## Organized Service

For services with multiple handlers or business logic:

```
my-service/
├── cmd/
│   └── server/
│       └── main.go      # Entry point
├── internal/
│   ├── app/
│   │   └── api.go       # API builder
│   ├── handlers/
│   │   ├── users.go     # User handlers
│   │   └── posts.go     # Post handlers
│   └── models/
│       └── user.go      # Domain models
├── go.mod
└── go.sum
```

**cmd/server/main.go:**
```go
package main

import (
    "context"
    
    "my-service/internal/app"
    
    "github.com/z5labs/humus/app"
    "github.com/z5labs/humus/config"
    httpserver "github.com/z5labs/humus/http"
    "github.com/z5labs/humus/otel"
    "github.com/z5labs/humus/rest"
)

func main() {
    listener := httpserver.NewTCPListener(
        httpserver.Addr(config.Default(":8080", config.Env("HTTP_ADDR"))),
    )
    srv := httpserver.NewServer(listener)
    
    // Build API from internal package
    api := app.BuildAPI(context.Background())
    
    restBuilder := rest.Build(srv, api)
    otelBuilder := otel.Build(app.BuildOTelSDK(), restBuilder)
    
    _ = app.Run(context.Background(), otelBuilder)
}
```

**internal/app/api.go:**
```go
package app

import (
    "context"
    
    "my-service/internal/handlers"
    "github.com/z5labs/humus/rest"
)

func BuildAPI(ctx context.Context) *rest.Api {
    api := rest.NewApi("My Service", "1.0.0")
    
    // Register handlers from different packages
    handlers.RegisterUserHandlers(api)
    handlers.RegisterPostHandlers(api)
    
    return api
}
```

## Large Application

For larger applications with multiple domains:

```
my-service/
├── cmd/
│   └── server/
│       └── main.go
├── internal/
│   ├── app/
│   │   ├── api.go        # API builder
│   │   ├── config.go     # Config readers
│   │   └── otel.go       # OTel setup
│   ├── user/
│   │   ├── handler.go    # HTTP handlers
│   │   ├── service.go    # Business logic
│   │   └── store.go      # Data access
│   ├── post/
│   │   ├── handler.go
│   │   ├── service.go
│   │   └── store.go
│   └── common/
│       └── middleware.go
├── pkg/
│   └── client/           # Public client library (optional)
│       └── client.go
├── go.mod
└── go.sum
```

**Key points:**
- Configuration via environment variables (no config files)
- Builder functions in `internal/app/`
- Domain packages under `internal/`
- Optional public client in `pkg/`

## gRPC Service

For gRPC services with Protocol Buffers:

```
my-grpc-service/
├── cmd/
│   └── server/
│       └── main.go
├── internal/
│   ├── app/
│   │   └── app.go
│   └── user/
│       └── service.go   # gRPC service implementation
├── proto/
│   ├── user/
│   │   └── user.proto   # Proto definitions
│   └── common/
│       └── types.proto
├── gen/                 # Generated code
│   └── proto/
│       └── user/
│           ├── user.pb.go
│           └── user_grpc.pb.go
├── Makefile            # For proto generation
├── config.yaml
├── go.mod
└── go.sum
```

**Example Makefile for proto generation:**
```makefile
.PHONY: proto
proto:
    protoc --go_out=gen --go_opt=paths=source_relative \
           --go-grpc_out=gen --go-grpc_opt=paths=source_relative \
           proto/**/*.proto
```

## Job Service

For batch processing or one-off jobs:

```
my-job/
├── cmd/
│   └── job/
│       └── main.go
├── internal/
│   ├── app/
│   │   └── app.go
│   ├── processor/
│   │   └── processor.go  # Job logic
│   └── store/
│       └── database.go   # Data access
├── config.yaml
├── go.mod
└── go.sum
```

**internal/app/app.go:**
```go
package app

import (
    "context"
    "my-job/internal/processor"
    "github.com/z5labs/humus/job"
)

type Config struct {
    humus.Config `config:",squash"`
    // Job-specific config
}

func Init(ctx context.Context, cfg Config) (job.Handler, error) {
    proc := processor.New(cfg)
    return proc, nil
}
```

## Monorepo with Multiple Services

For projects with multiple related services:

```
my-project/
├── services/
│   ├── api/             # REST API
│   │   ├── cmd/
│   │   ├── internal/
│   │   └── go.mod
│   ├── worker/          # gRPC service
│   │   ├── cmd/
│   │   ├── internal/
│   │   └── go.mod
│   └── jobs/            # Background jobs
│       ├── cmd/
│       ├── internal/
│       └── go.mod
├── pkg/                 # Shared packages
│   ├── models/
│   └── common/
└── proto/               # Shared proto files
    └── common/
```

## Configuration Files

### Multiple Environments

```
my-service/
├── configs/
│   ├── base.yaml        # Shared config
│   ├── dev.yaml         # Development
│   ├── staging.yaml     # Staging
│   └── prod.yaml        # Production
└── cmd/server/main.go
```

**Loading environment-specific config:**
```go
import (
    "os"
    "github.com/z5labs/bedrock/pkg/config"
)

func main() {
    env := os.Getenv("ENV")
    if env == "" {
        env = "dev"
    }

    source := config.MultiSource(
        config.FromYaml("configs/base.yaml"),
        config.FromYaml(fmt.Sprintf("configs/%s.yaml", env)),
    )

    rest.Run(source, app.Init)
}
```

## Package Organization Best Practices

### Use `internal/` for Private Code

Place code that shouldn't be imported by other projects in `internal/`:

```
my-service/
├── internal/           # Cannot be imported by external projects
│   ├── app/
│   └── handlers/
└── pkg/                # Can be imported by others
    └── client/
```

### Domain-Driven Structure

Group by domain/feature rather than technical layer:

**Good:**
```
internal/
├── user/
│   ├── handler.go    # HTTP layer
│   ├── service.go    # Business logic
│   └── store.go      # Data layer
└── post/
    ├── handler.go
    ├── service.go
    └── store.go
```

**Less Ideal:**
```
internal/
├── handlers/         # All HTTP handlers
├── services/         # All business logic
└── stores/           # All data access
```

### Separate Main Package

Keep `main.go` minimal - just wiring, not logic:

```go
// Good: main.go just calls Run
func main() {
    rest.Run(rest.YamlSource("config.yaml"), app.Init)
}

// Less ideal: main.go contains business logic
func main() {
    // Lots of setup code...
    // Handler definitions...
    // Database initialization...
}
```

## Testing Structure

Place tests alongside the code:

```
internal/
├── user/
│   ├── handler.go
│   ├── handler_test.go
│   ├── service.go
│   └── service_test.go
```

For integration tests:

```
my-service/
├── internal/
└── test/
    ├── integration/
    │   └── api_test.go
    └── testdata/
        └── fixtures.json
```

## Next Steps

Now that you understand project structure:

- Explore [REST Services]({{< ref "/features/rest" >}}) to build HTTP APIs
- Learn about [gRPC Services]({{< ref "/features/grpc" >}}) for microservices
- Read [Job Services]({{< ref "/features/job" >}}) for batch processing
- Review [Core Concepts]({{< ref "/concepts" >}}) for deeper understanding
