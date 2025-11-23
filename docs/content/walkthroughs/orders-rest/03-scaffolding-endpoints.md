---
title: Scaffolding Endpoints
description: Quickly scaffold endpoints with dummy responses
weight: 3
type: docs
slug: scaffolding-endpoints
---

Now that we have a running API, let's scaffold our two endpoints with dummy responses. This demonstrates how quickly you can get working endpoints before implementing any business logic.

We'll organize the code into a clean structure:
- `model/` - Domain models
- `endpoint/` - HTTP endpoint handlers
- `app/` - Application initialization

## Define Domain Models

First, create `model/order.go` with the core domain types:

```go
package model

// OrderStatus represents the current state of an order.
type OrderStatus string

const (
	// OrderStatusPending indicates the order is waiting to be processed.
	OrderStatusPending OrderStatus = "pending"
	// OrderStatusProcessing indicates the order is currently being processed.
	OrderStatusProcessing OrderStatus = "processing"
	// OrderStatusCompleted indicates the order has been successfully completed.
	OrderStatusCompleted OrderStatus = "completed"
	// OrderStatusFailed indicates the order has failed.
	OrderStatusFailed OrderStatus = "failed"
)

// Order represents a customer order.
type Order struct {
	OrderID    string      `json:"order_id"`
	AccountID  string      `json:"account_id"`
	CustomerID string      `json:"customer_id"`
	Status     OrderStatus `json:"status"`
}

// PageInfo contains pagination information for list responses.
type PageInfo struct {
	HasNextPage bool   `json:"has_next_page"`
	EndCursor   string `json:"end_cursor,omitempty"`
}

// ListOrdersResponse is the response for listing orders with pagination.
type ListOrdersResponse struct {
	Orders   []Order  `json:"orders"`
	PageInfo PageInfo `json:"page_info"`
}
```

Notice we're using a typed `OrderStatus` constant rather than plain strings—this provides better type safety and IDE support.

## Create List Orders Endpoint

Create `endpoint/list_orders.go` to handle GET /v1/orders:

```go
package endpoint

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/model"
	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"
)

// ListOrders creates the GET /v1/orders endpoint.
func ListOrders() rest.ApiOption {
	handler := rpc.ProducerFunc[model.ListOrdersResponse](func(ctx context.Context) (*model.ListOrdersResponse, error) {
		// Return empty list for now
		return &model.ListOrdersResponse{
			Orders: []model.Order{},
			PageInfo: model.PageInfo{
				HasNextPage: false,
			},
		}, nil
	})

	return rest.Handle(
		http.MethodGet,
		rest.BasePath("/v1").Segment("orders"),
		rpc.ProduceJson(handler),
	)
}
```

This is a **Producer**—it produces a response without consuming a request body. Perfect for GET endpoints.

## Create Place Order Endpoint

Create `endpoint/place_order.go` to handle POST /v1/order:

```go
package endpoint

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/model"
	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"
)

// PlaceOrderRequest is the request body for placing an order.
type PlaceOrderRequest struct {
	CustomerID string `json:"customer_id"`
	AccountID  string `json:"account_id"`
}

// PlaceOrderResponse is the response for a successfully placed order.
type PlaceOrderResponse struct {
	OrderID string `json:"order_id"`
}

// PlaceOrder creates the POST /v1/order endpoint.
func PlaceOrder() rest.ApiOption {
	handler := rpc.HandlerFunc[PlaceOrderRequest, PlaceOrderResponse](
		func(ctx context.Context, req *PlaceOrderRequest) (*PlaceOrderResponse, error) {
			// Return dummy success response
			return &PlaceOrderResponse{
				OrderID: "dummy-order-123",
			}, nil
		},
	)

	return rest.Handle(
		http.MethodPost,
		rest.BasePath("/v1").Segment("order"),
		rpc.HandleJson(handler),
	)
}
```

Request/response types are defined in the endpoint package—they represent the API contract, not the domain model.

## Wire Up the Application

Update `app/app.go` to register both endpoints:

```go
package app

import (
	"context"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/endpoint"
	"github.com/z5labs/humus/rest"
)

// Config defines the application configuration.
type Config struct {
	rest.Config `config:",squash"`
}

// Init initializes the REST API with all endpoints.
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
	api := rest.NewApi(
		cfg.OpenApi.Title,
		cfg.OpenApi.Version,
		endpoint.ListOrders(),
		endpoint.PlaceOrder(),
	)

	return api, nil
}
```

Key patterns demonstrated here:
- **Separation of concerns** - Models, endpoints, and app logic are in separate packages
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
  "order_id": "dummy-order-123"
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
1. ✅ Created a clean package structure (model, endpoint, app)
2. ✅ Defined domain types with type-safe constants
3. ✅ Registered two working endpoints with dummy handlers
4. ✅ Tested both with real HTTP requests
5. ✅ Generated OpenAPI documentation automatically

This demonstrates Humus's productivity: you defined types, registered handlers, and got working endpoints with OpenAPI docs—no schema files, no decorators, just Go code.

The clean separation between packages also sets you up for success as the application grows:
- `model/` owns the domain concepts
- `endpoint/` owns the HTTP contract
- `app/` composes everything together

## What's Next

Now that we have working endpoints, let's add real business logic with backend services and proper validation.

[Next: Domain Model →]({{< ref "/walkthroughs/orders-rest/04-domain-model" >}})
