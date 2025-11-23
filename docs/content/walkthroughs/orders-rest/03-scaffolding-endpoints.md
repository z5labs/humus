---
title: Scaffolding Endpoints
description: Quickly scaffold endpoints with dummy responses
weight: 3
type: docs
slug: scaffolding-endpoints
---

Now that we have a running API, let's scaffold our two endpoints with dummy responses. This demonstrates how quickly you can get working endpoints before implementing any business logic.

## Define Response Types

First, create `app/types.go` with simple response structures:

```go
package app

// Order represents an order in the system.
type Order struct {
	OrderID    string `json:"order_id"`
	AccountID  string `json:"account_id"`
	CustomerID string `json:"customer_id"`
	Status     string `json:"status"`
}

// PageInfo contains pagination metadata.
type PageInfo struct {
	HasNextPage bool   `json:"has_next_page"`
	EndCursor   string `json:"end_cursor,omitempty"`
}

// ListOrdersResponse is the response for listing orders.
type ListOrdersResponse struct {
	Orders   []Order  `json:"orders"`
	PageInfo PageInfo `json:"page_info"`
}

// PlaceOrderRequest is the request for placing an order.
type PlaceOrderRequest struct {
	AccountID  string `json:"account_id"`
	CustomerID string `json:"customer_id"`
}

// PlaceOrderResponse is the response for placing an order.
type PlaceOrderResponse struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}
```

These are minimal types just to get the endpoints working. We'll refine them later.

## Add Endpoint Registrations

Update `app/app.go` to register both endpoints with dummy handlers:

```go
package app

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"
)

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
	api := rest.NewApi(
		"Orders API",
		"v1.0.0",
		listOrders(),
		placeOrder(),
	)
	return api, nil
}

// listOrders scaffolds the GET /v1/orders endpoint with a dummy response.
func listOrders() rest.ApiOption {
	handler := rpc.ProducerFunc[ListOrdersResponse](func(ctx context.Context) (*ListOrdersResponse, error) {
		// Return empty list for now
		return &ListOrdersResponse{
			Orders: []Order{},
			PageInfo: PageInfo{
				HasNextPage: false,
			},
		}, nil
	})

	return rest.Handle(
		http.MethodGet,
		rest.BasePath("/v1/orders"),
		rpc.ProduceJson(handler),
	)
}

// placeOrder scaffolds the POST /v1/order endpoint with a dummy response.
func placeOrder() rest.ApiOption {
	handler := rpc.HandlerFunc[PlaceOrderRequest, PlaceOrderResponse](
		func(ctx context.Context, req *PlaceOrderRequest) (*PlaceOrderResponse, error) {
			// Return dummy success response
			return &PlaceOrderResponse{
				OrderID: "dummy-order-123",
				Status:  "pending",
			}, nil
		},
	)

	return rest.Handle(
		http.MethodPost,
		rest.BasePath("/v1/order"),
		rpc.HandleJson(handler),
	)
}
```

Key patterns demonstrated here:
- **GET endpoint** - Uses `rpc.ProducerFunc` (no request body) with `rpc.ProduceJson`
- **POST endpoint** - Uses `rpc.HandlerFunc` (request + response) with `rpc.HandleJson`
- **Dummy responses** - Hardcoded values allow immediate testing
- **No dependencies** - No services or database needed yet

## Run the API

```bash
go run .
```

## Test the List Endpoint

```bash
curl http://localhost:8090/v1/orders
```

Response:
```json
{
  "orders": [],
  "page_info": {
    "has_next_page": false
  }
}
```

## Test the Place Order Endpoint

```bash
curl -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{
    "account_id": "ACC123",
    "customer_id": "CUST456"
  }'
```

Response:
```json
{
  "order_id": "dummy-order-123",
  "status": "pending"
}
```

## Inspect the OpenAPI Schema

Humus automatically generates OpenAPI documentation from your Go types:

```bash
curl http://localhost:8090/openapi.json | jq
```

You'll see both operations defined with schemas for `ListOrdersResponse`, `PlaceOrderRequest`, and `PlaceOrderResponse`. The JSON struct tags determine the schema field names.

Look for:
- `paths["/v1/orders"]["get"]` - The list endpoint
- `paths["/v1/order"]["post"]` - The place order endpoint
- `components.schemas` - All your type definitions

## What We Accomplished

In just a few minutes, you:
1. ✅ Registered two working endpoints
2. ✅ Tested both with real HTTP requests
3. ✅ Generated OpenAPI documentation automatically
4. ✅ Saw responses matching your Go types exactly

This demonstrates Humus's productivity: you defined types, registered handlers, and got working endpoints with OpenAPI docs—no schema files, no decorators, just Go code.

## What's Next

Now let's properly define our domain model and start implementing real business logic.

[Next: Domain Model →]({{< ref "/walkthroughs/orders-rest/04-domain-model" >}})
