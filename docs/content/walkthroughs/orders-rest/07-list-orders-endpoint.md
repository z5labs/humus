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

	"rest-orders-walkthrough/model"
	"rest-orders-walkthrough/service"
	"github.com/z5labs/humus/rest"
	"github.com/z5labs/humus/rest/rpc"
)

// ListOrders creates the GET /v1/orders endpoint.
func ListOrders(dataSvc service.DataService) rest.ApiOption {
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
	dataSvc service.DataService
}

func (h *listOrdersHandler) Produce(ctx context.Context) (*model.ListOrdersResponse, error) {
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
	var status *model.OrderStatus
	if statusStr != "" {
		s := model.OrderStatus(statusStr)
		status = &s
	}

	// Query data service
	result, err := h.dataSvc.Query(ctx, accountNumber, status, cursor, limit)
	if err != nil {
		return nil, err
	}

	// Build response with cursor-based pagination
	response := &model.ListOrdersResponse{
		Orders: result.Orders,
		PageInfo: model.PageInfo{
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
