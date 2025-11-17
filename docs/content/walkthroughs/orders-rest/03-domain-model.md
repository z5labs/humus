---
title: Domain Model
description: Define Order types and pagination structures
weight: 3
type: docs
slug: domain-model
---

Let's define the core domain types for our order system.

## Order Model

Create `model/order.go`:

```go
package model

// OrderStatus represents the current state of an order.
type OrderStatus string

const (
	OrderStatusPending    OrderStatus = "pending"
	OrderStatusProcessing OrderStatus = "processing"
	OrderStatusCompleted  OrderStatus = "completed"
	OrderStatusFailed     OrderStatus = "failed"
)

// Order represents an order in the system.
type Order struct {
	OrderID    string      `json:"order_id"`
	AccountID  string      `json:"account_id"`
	CustomerID string      `json:"customer_id"`
	Status     OrderStatus `json:"status"`
}
```

## Pagination Types

Add pagination support to the same file:

```go
// PageInfo contains pagination metadata for cursor-based pagination.
type PageInfo struct {
	HasNextPage bool   `json:"has_next_page"`
	EndCursor   string `json:"end_cursor,omitempty"`
}

// ListOrdersResponse is the response for listing orders.
type ListOrdersResponse struct {
	Orders   []Order  `json:"orders"`
	PageInfo PageInfo `json:"page_info"`
}
```

## Why Cursor-Based Pagination?

We use cursor-based pagination instead of offset-based for several reasons:

1. **Consistency** - No skipped or duplicated items when data changes
2. **Performance** - Database can efficiently seek to cursor position
3. **Scalability** - Works well with large datasets

The cursor is an opaque token (base64-encoded OrderID) that points to the last item returned.

## JSON Tags

All fields have explicit JSON tags:
- `json:"order_id"` - Uses snake_case for API consistency
- `json:"end_cursor,omitempty"` - Omits field if empty

This ensures the API response matches the OpenAPI schema exactly.

## What's Next

With our domain model defined, let's implement the data service that will store and retrieve orders.

[Next: Data Service →]({{< ref "/walkthroughs/orders-rest/04-data-service" >}})
