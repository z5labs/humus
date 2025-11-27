---
title: Your First Service
description: Build a Hello World REST service with Humus
weight: 20
type: docs
---


In this guide, you'll build a simple REST service that responds with "Hello, World!". This will introduce you to the core concepts of Humus.

## Project Setup

Create a new directory and initialize a Go module:

```bash
mkdir hello-humus
cd hello-humus
go mod init hello-humus
go get github.com/z5labs/humus
```

## Configuration File

Create a `config.yaml` file in your project root:

```yaml
rest:
  port: 8080

otel:
  service:
    name: hello-humus
  sdk:
    disabled: true  # Disable for this simple example
```

This configuration:
- Sets the HTTP server port to 8080
- Names the service "hello-humus"
- Disables OpenTelemetry for simplicity (you'll enable this in production)

## Application Code

Create a `main.go` file:

```go
package main

import (
    "context"
    "net/http"

    "github.com/z5labs/humus/rest"
)

// Config embeds rest.Config to get HTTP server configuration
type Config struct {
    rest.Config `config:",squash"`
}

func main() {
    // rest.Run handles configuration loading, app initialization, and execution
    rest.Run(rest.YamlSource("config.yaml"), Init)
}

// Init is called with the loaded configuration and returns the API
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    // Create a simple handler that returns "Hello, World!"
    handler := rest.ProducerFunc[string](func(ctx context.Context) (*string, error) {
        msg := "Hello, World!"
        return &msg, nil
    })

    // Create a new API with name and version and register the handler at GET /hello
    api := rest.NewApi(
        "Hello Service",
        "1.0.0",
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/hello"),
            rest.ProduceJson(handler),
        ),
    )

    return api, nil
}
```

## Running the Service

Run your service:

```bash
go run main.go
```

You should see output indicating the server has started. The service is now running on http://localhost:8080.

## Testing the Endpoint

In another terminal, test your endpoint:

```bash
curl http://localhost:8080/hello
```

You should see:

```
"Hello, World!"
```

## Exploring Built-in Endpoints

Humus automatically provides several endpoints:

### OpenAPI Specification

```bash
curl http://localhost:8080/openapi.json
```

This returns the OpenAPI 3.0 specification for your API, automatically generated from your code.

### Health Checks

```bash
# Liveness probe
curl http://localhost:8080/health/liveness

# Readiness probe
curl http://localhost:8080/health/readiness
```

Both should return `200 OK` with `{"healthy":true}`.

## Understanding the Code

Let's break down what's happening:

1. **Configuration**: The `Config` struct embeds `rest.Config`, which provides HTTP server configuration fields that are automatically populated from `config.yaml`.

2. **rest.Run()**: This function orchestrates the entire application lifecycle:
   - Reads configuration from the YAML file
   - Calls `Init()` with the parsed configuration
   - Starts the HTTP server
   - Handles graceful shutdown on OS signals

3. **Init Function**: This is where you build your API:
   - Create an `Api` instance with a name and version
   - Define handlers for your endpoints
   - Register handlers with HTTP methods and paths
   - Return the configured API

4. **Handler Pattern**: The `rest.ProducerFunc` creates a type-safe handler. In this example, it produces a `string` response with no request body.

## Next Steps

Now that you have a working service, you can:

- Learn about [Configuration]({{< ref "configuration" >}}) to customize your service
- Explore [Project Structure]({{< ref "project-structure" >}}) for organizing larger applications
- Read about [REST Services]({{< ref "/features/rest" >}}) for more advanced HTTP patterns
- Understand [Core Concepts]({{< ref "/concepts" >}}) for a deeper dive into Humus architecture

## Complete Example

The complete code for this example is available in the [Humus repository](https://github.com/z5labs/humus/tree/main/example).
