---
title: Minimal Running App
description: Create a minimal REST API to verify setup
weight: 2
type: docs
slug: minimal-app
---

Let's get a minimal API running immediately to verify everything works.

## Configuration File

Create `config.yaml`:

```yaml
http:
  port: {{env "HTTP_PORT" | default 8090}}
```

This minimal config just sets the HTTP port. The `{{env "VAR" | default "value"}}` syntax uses Go templating to read environment variables with fallbacks.

## Application Initialization

Create `app/app.go`:

```go
package app

import (
	"context"

	"github.com/z5labs/humus/rest"
)

type Config struct {
	rest.Config `config:",squash"`
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
	api := rest.NewApi("Orders API", "v1.0.0")
	return api, nil
}
```

## Main Entry Point

Create `main.go`:

```go
package main

import (
	"bytes"
	_ "embed"

	"rest-orders-walkthrough/app"
	"github.com/z5labs/humus/rest"
)

//go:embed config.yaml
var configBytes []byte

func main() {
	rest.Run(bytes.NewReader(configBytes), app.Init)
}
```

Key points:
- `//go:embed` embeds config.yaml at compile time
- `rest.Run()` handles OTel initialization, server lifecycle, and graceful shutdown
- Logs go to stdout by default (no external infrastructure needed yet)

## Run It

```bash
go mod tidy
go run .
```

You should see output like:

```
2025-11-21T06:43:07.123Z INFO Starting HTTP server {"port": 8090}
```

The API is running! Press Ctrl+C to stop it.

## Test the Health Endpoints

Humus automatically provides liveness and readiness health endpoints:

```bash
# Liveness probe
curl http://localhost:8090/health/liveness

# Readiness probe
curl http://localhost:8090/health/readiness
```

Both endpoints return `200 OK` by default (empty response body).

## OpenAPI Documentation

Humus also automatically generates OpenAPI documentation:

```bash
curl http://localhost:8090/openapi.json
```

You'll see the OpenAPI 3.0 spec for your API (currently with no operations defined).

## What's Next

Now that we have a running API, let's quickly scaffold our endpoints to see how fast we can get them working.

[Next: Scaffolding Endpoints →]({{< ref "/walkthroughs/orders-rest/03-scaffolding-endpoints" >}})
