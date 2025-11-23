---
title: Scaffolding Endpoints
description: Quickly scaffold endpoints with dummy responses
weight: 3
type: docs
slug: scaffolding-endpoints
---

Now that we have a running API, let's scaffold our two endpoints with dummy responses. This demonstrates how quickly you can get working endpoints before implementing any business logic.

We'll organize the code into a clean structure:
- `endpoint/` - Domain models and HTTP endpoint handlers
- `app/` - Application initialization

## Define Common Domain Models

First, create `endpoint/model.go` with common types shared across endpoints:

```go
package endpoint

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
```

### Design Decisions

**Typed Constants:** We use a typed `OrderStatus` constant rather than plain strings—this provides better type safety and IDE support.

**Cursor-Based Pagination:** We use cursor-based pagination instead of offset-based for several reasons:
1. **Consistency** - No skipped or duplicated items when data changes
2. **Performance** - Database can efficiently seek to cursor position
3. **Scalability** - Works well with large datasets

The cursor is an opaque token (base64-encoded OrderID) that points to the last item returned.

**JSON Tags:** All fields have explicit JSON tags:
- `json:"order_id"` - Uses snake_case for API consistency
- `json:"end_cursor,omitempty"` - Omits field if empty

This ensures the API response matches the OpenAPI schema exactly.

**Why in the Endpoint Package?** These endpoint-specific types (like `ListOrdersResponse`, `PageInfo`) are defined in the `endpoint` package because:
1. **API response structure** - These types define the shape of HTTP responses returned to clients
2. **Consumer-defined interfaces** - Following idiomatic Go, the endpoint package defines the service interfaces it needs (in `endpoint/interfaces.go`)
3. **Separation from service layer** - Service packages define their own domain types (`service.Order`, `service.OrderStatus`), while endpoints convert these to API response types

**Package Architecture:**
- `service/` packages define domain types and business logic types
- `endpoint/` package defines API request/response types and service interfaces
- Endpoints import service types and convert them to API responses (see `orderFromService()` helper)
- Services **never** import from endpoint package - this prevents circular dependencies

Only types shared across multiple endpoints in the same package belong in `model.go`.

## Create List Orders Endpoint

Create `endpoint/list_orders.go` to handle GET /v1/orders:

```go
package endpoint

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"
)

// ListOrdersResponse is the response for listing orders with pagination.
type ListOrdersResponse struct {
	Orders   []Order  `json:"orders"`
	PageInfo PageInfo `json:"page_info"`
}

// ListOrders creates the GET /v1/orders endpoint.
func ListOrders() rest.ApiOption {
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
		rest.BasePath("/v1").Segment("orders"),
		rpc.ProduceJson(handler),
	)
}
```

This is a **Producer**—it produces a response without consuming a request body. Perfect for GET endpoints. The response type is defined here since it's specific to this endpoint.

## Create Place Order Endpoint

Create `endpoint/place_order.go` to handle POST /v1/order:

```go
package endpoint

import (
	"context"
	"net/http"

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

Request/response types are defined alongside their endpoints—they represent the API contract specific to that operation.

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
- **Separation of concerns** - Endpoints and app logic are in separate packages
- **GET endpoint** - Uses `rpc.ProducerFunc` (no request body) with `rpc.ProduceJson`
- **POST endpoint** - Uses `rpc.HandlerFunc` (request + response) with `rpc.HandleJson`
- **Dummy responses** - Hardcoded values allow immediate testing
- **No dependencies** - No services or database needed yet
- **Type organization** - Common types in `model.go`, endpoint-specific types alongside their handlers

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
1. ✅ Created a clean package structure (endpoint, app)
2. ✅ Defined domain types with type-safe constants
3. ✅ Registered two working endpoints with dummy handlers
4. ✅ Tested both with real HTTP requests
5. ✅ Generated OpenAPI documentation automatically

This demonstrates Humus's productivity: you defined types, registered handlers, and got working endpoints with OpenAPI docs—no schema files, no decorators, just Go code.

The clean code organization also sets you up for success as the application grows:
- `endpoint/model.go` - Common domain types shared across endpoints
- `endpoint/*.go` - Each endpoint file contains its specific request/response types and handler
- `app/` - Application composition and wiring

## What's Next

Now that we have working endpoints with our domain model defined, let's implement the backend services that power our order management system.

[Next: Backend Services →]({{< ref "/walkthroughs/orders-rest/04-backend-services" >}})
