---
title: List Orders Endpoint
description: Implement GET /v1/orders with cursor-based pagination
weight: 7
type: docs
slug: list-orders-endpoint
---

Let's implement the GET /v1/orders endpoint with query parameters and pagination.

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

## Registering the Endpoint

Now update `app/app.go` to register the endpoint:

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
	_ = service.NewRestrictionClient(cfg.Services.RestrictionURL, httpClient)
	_ = service.NewEligibilityClient(cfg.Services.EligibilityURL, httpClient)

	// Create API with ListOrders endpoint
	api := rest.NewApi(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
		endpoint.ListOrders(dataSvc),
	)

	return api, nil
}
```

Changes:
- Import the `endpoint` package
- Remove `_` from `dataSvc` variable (now used)
- Pass `dataSvc` to `endpoint.ListOrders()`
- Register endpoint in `rest.NewApi()`

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

[Next: Place Order Endpoint →]({{< ref "/walkthroughs/orders-rest/08-place-order-endpoint" >}})
