---
title: List Orders Endpoint
description: Implement GET /v1/orders with cursor-based pagination
weight: 5
type: docs
slug: list-orders-endpoint
---

Let's implement the GET /v1/orders endpoint with query parameters and pagination.

## Configuration Structure

First, create `app/config.go` to define the application configuration:

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

## Endpoint Registration

Create `endpoint/list_orders.go`:

```go
package endpoint

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"
)

// ListOrders creates the GET /v1/orders endpoint.
func ListOrders(dataSvc DataService) rest.ApiOption {
	handler := &listOrdersHandler{dataSvc: dataSvc}

	return rest.Handle(
		http.MethodGet,
		rest.BasePath("/v1").Segment("orders"),
		rpc.ProduceJson(handler),
		rest.QueryParam("accountNumber", rest.Required()),
		rest.QueryParam("after"),
		rest.QueryParam("limit"),
		rest.QueryParam("status"),
	)
}
```

Key components:
- `rest.Handle()` registers the endpoint
- `rest.BasePath("/v1").Segment("orders")` creates path `/v1/orders`
- `rpc.ProduceJson()` returns JSON responses (GET pattern)
- `rest.QueryParam()` defines query parameters
- `rest.Required()` marks parameter as mandatory

## Handler Implementation

```go
type listOrdersHandler struct {
	dataSvc DataService
}

func (h *listOrdersHandler) Produce(ctx context.Context) (*ListOrdersResponse, error) {
	// Extract query parameters from context
	accountNumberValues := rest.QueryParamValue(ctx, "accountNumber")
	accountNumber := ""
	if len(accountNumberValues) > 0 {
		accountNumber = accountNumberValues[0]
	}

	afterValues := rest.QueryParamValue(ctx, "after")
	afterCursor := ""
	if len(afterValues) > 0 {
		afterCursor = afterValues[0]
	}

	limitValues := rest.QueryParamValue(ctx, "limit")
	limitStr := ""
	if len(limitValues) > 0 {
		limitStr = limitValues[0]
	}

	statusValues := rest.QueryParamValue(ctx, "status")
	statusStr := ""
	if len(statusValues) > 0 {
		statusStr = statusValues[0]
	}

	// Default limit
	limit := 10
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	// Decode cursor if provided
	cursor := ""
	if afterCursor != "" {
		decoded, err := base64.StdEncoding.DecodeString(afterCursor)
		if err == nil {
			cursor = string(decoded)
		}
	}

	// Parse status filter
	var status *service.OrderStatus
	if statusStr != "" {
		s := service.OrderStatus(statusStr)
		status = &s
	}

	// Query data service
	result, err := h.dataSvc.Query(ctx, accountNumber, status, cursor, limit)
	if err != nil {
		return nil, err
	}

	// Convert service orders to endpoint orders
	orders := make([]Order, len(result.Orders))
	for i, svcOrder := range result.Orders {
		orders[i] = orderFromService(svcOrder)
	}

	// Build response with cursor-based pagination
	response := &ListOrdersResponse{
		Orders: orders,
		PageInfo: PageInfo{
			HasNextPage: result.HasMore,
		},
	}

	// Encode next cursor if there are more results
	if result.HasMore && result.NextCursor != "" {
		response.PageInfo.EndCursor = base64.StdEncoding.EncodeToString([]byte(result.NextCursor))
	}

	return response, nil
}
```

## Cursor Encoding

The cursor is base64-encoded for:
- **Opacity** - Clients treat it as opaque token
- **Safety** - Safe for URLs and JSON
- **Flexibility** - Can contain any string (OrderID, timestamp, etc.)

## Application Initialization

Create `app/app.go` to initialize the API and register the endpoint:

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

	// Create API with ListOrders endpoint
	api := rest.NewApi(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
		endpoint.ListOrders(dataSvc),
	)

	return api, nil
}
```

Important aspects:
- Use `otelhttp.NewTransport` to automatically instrument outgoing HTTP calls
- Initialize data service with URL from config
- Pass service to endpoint via dependency injection
- Register endpoint in `rest.NewApi()`

## Main Entry Point

Create `main.go` as the application entry point:

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

Create `config.yaml` with service URLs and OTel configuration:

```yaml
otel:
  resource:
    service_name: orders-api

openapi:
  title: Orders API
  version: v1.0.0

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
- OpenAPI metadata defines the API title and version

## Testing the Endpoint

Start the application:

```bash
go run .
```

Query orders:

```bash
curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001" | jq .
```

Response:

```json
{
  "orders": [
    {
      "order_id": "ORD-001",
      "account_id": "ACC-001",
      "customer_id": "CUST-001",
      "status": "completed"
    },
    {
      "order_id": "ORD-002",
      "account_id": "ACC-001",
      "customer_id": "CUST-001",
      "status": "pending"
    }
  ],
  "page_info": {
    "has_next_page": true,
    "end_cursor": "T1JELTAwMw=="
  }
}
```

To get the next page, use the cursor:

```bash
curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001&after=T1JELTAwMw==" | jq .
```

## What's Next

Now let's implement the POST /v1/order endpoint with service orchestration.

[Next: Place Order Endpoint →]({{< ref "/walkthroughs/orders-rest/07-place-order-endpoint" >}})
