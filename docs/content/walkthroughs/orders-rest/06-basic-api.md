---
title: Basic API
description: Create the application entry point and configuration
weight: 6
type: docs
slug: basic-api
---

Now let's create the application entry point that ties everything together.

## Configuration Structure

Create `app/config.go`:

```go
package app

import "github.com/z5labs/humus/rest"

// Config defines the application configuration.
type Config struct {
	rest.Config `config:",squash"`

	Services struct {
		DataURL        string `config:"data_url"`
		RestrictionURL string `config:"restriction_url"`
		EligibilityURL string `config:"eligibility_url"`
	} `config:"services"`
}
```

Key points:
- Embed `rest.Config` with `config:",squash"` to inherit HTTP server and OTel settings
- Add custom `Services` struct for backend service URLs
- Tags use `config:` not `json:` for bedrock configuration system

## Application Initialization

Create `app/app.go`:

```go
package app

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/endpoint"
	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
	"github.com/z5labs/humus/rest"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Init initializes the REST API with all endpoints and services.
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
	// Create OTel-instrumented HTTP client for service calls
	httpClient := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	// Initialize services
	dataSvc := service.NewDataClient(cfg.Services.DataURL, httpClient)
	restrictionSvc := service.NewRestrictionClient(cfg.Services.RestrictionURL, httpClient)
	eligibilitySvc := service.NewEligibilityClient(cfg.Services.EligibilityURL, httpClient)

	// Create API with endpoints
	api := rest.NewApi(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
		endpoint.ListOrders(dataSvc),
		endpoint.PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc),
	)

	return api, nil
}
```

Important aspects:
- Use `otelhttp.NewTransport` to automatically instrument outgoing HTTP calls
- Initialize services with URLs from config
- Pass services to endpoints (dependency injection)
- Register all endpoints in `rest.NewApi()`

## Main Entry Point

Create `main.go`:

```go
package main

import (
	"bytes"
	_ "embed"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/app"
	"github.com/z5labs/humus/rest"
)

//go:embed config.yaml
var configBytes []byte

func main() {
	rest.Run(bytes.NewReader(configBytes), app.Init)
}
```

This is the standard Humus pattern:
- Embed config.yaml at compile time
- Call `rest.Run()` with config reader and init function
- Framework handles OTel setup, server lifecycle, and graceful shutdown

## Configuration File

Update `config.yaml` to add service URLs and OTel configuration:

```yaml
otel:
  resource:
    service_name: orders-api

http:
  port: {{env "HTTP_PORT" | default 8090}}

services:
  data_url: {{env "DATA_SERVICE_URL" | default "http://localhost:8080"}}
  restriction_url: {{env "RESTRICTION_SERVICE_URL" | default "http://localhost:8080"}}
  eligibility_url: {{env "ELIGIBILITY_SERVICE_URL" | default "http://localhost:8080"}}
```

The config uses Go templating:
- `{{env "VAR"}}` reads environment variables
- `| default "value"` provides fallbacks
- All three service URLs point to a mock server (we'll set up Wiremock later)
- OTel is minimal for now (logs go to stdout)

## What's Next

Now let's implement the GET /v1/orders endpoint with cursor-based pagination.

[Next: List Orders Endpoint →]({{< ref "/walkthroughs/orders-rest/07-list-orders-endpoint" >}})
