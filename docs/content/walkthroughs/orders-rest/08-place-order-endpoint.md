---
title: Place Order Endpoint
description: Implement POST /v1/order with service orchestration
weight: 8
type: docs
slug: place-order-endpoint
---

Now let's implement the POST endpoint that orchestrates multiple services.

## Request and Response Types

Create `endpoint/place_order.go`:

```go
package endpoint

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/sourcegraph/conc/pool"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
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

// ErrAccountRestricted indicates the account has restrictions preventing order placement.
var ErrAccountRestricted = errors.New("account has restrictions")

// ErrAccountIneligible indicates the account is not eligible to place orders.
var ErrAccountIneligible = errors.New("account is not eligible")
```

Note the error variable naming convention: `ErrAccountRestricted` follows Go's `ErrFoo` pattern.

## Endpoint Registration

```go
// PlaceOrder creates the POST /v1/order endpoint.
func PlaceOrder(restrictionSvc RestrictionService, eligibilitySvc EligibilityService, dataSvc DataService) rest.ApiOption {
	handler := &placeOrderHandler{
		restrictionSvc: restrictionSvc,
		eligibilitySvc: eligibilitySvc,
		dataSvc:        dataSvc,
	}

	return rest.Handle(
		http.MethodPost,
		rest.BasePath("/v1").Segment("order"),
		rpc.HandleJson(handler),
	)
}
```

Key differences from GET:
- Uses `http.MethodPost`
- Uses `rpc.HandleJson()` which consumes request body AND returns response
- No query parameters needed
- Interfaces defined locally (see `endpoint/interfaces.go`)

## Concurrent Validation with conc/pool

The handler runs validation checks concurrently for optimal performance:

```go
type placeOrderHandler struct {
	restrictionSvc RestrictionService
	eligibilitySvc EligibilityService
	dataSvc        DataService
}

func (h *placeOrderHandler) Handle(ctx context.Context, req *PlaceOrderRequest) (*PlaceOrderResponse, error) {
	// Run validation checks concurrently using conc/pool
	p := pool.New().WithContext(ctx)

	// Check restrictions concurrently
	p.Go(func(ctx context.Context) error {
		restrictions, err := h.restrictionSvc.CheckRestrictions(ctx, req.AccountID)
		if err != nil {
			return err
		}
		if len(restrictions) > 0 {
			return ErrAccountRestricted
		}
		return nil
	})

	// Check eligibility concurrently
	p.Go(func(ctx context.Context) error {
		eligibility, err := h.eligibilitySvc.CheckEligibility(ctx, req.AccountID)
		if err != nil {
			return err
		}
		if !eligibility.Eligible {
			return ErrAccountIneligible
		}
		return nil
	})

	// Wait for both checks to complete
	if err := p.Wait(); err != nil {
		return nil, err
	}

	// Create and store the order
	orderID := uuid.New().String()
	order := service.Order{
		OrderID:    orderID,
		AccountID:  req.AccountID,
		CustomerID: req.CustomerID,
		Status:     service.OrderStatusPending,
	}

	if err := h.dataSvc.PutItem(ctx, order); err != nil {
		return nil, err
	}

	return &PlaceOrderResponse{
		OrderID: orderID,
	}, nil
}
```

The handler demonstrates:
1. **Concurrent validation** - Both checks run in parallel using `conc/pool`
2. **Performance optimization** - ~50% latency reduction when both services are healthy
3. **Fail-fast behavior** - `p.Wait()` returns on first error
4. **Panic safety** - `conc/pool` handles panics gracefully
5. **Context propagation** - Cancellation flows to all goroutines

### Why conc/pool?

The `github.com/sourcegraph/conc/pool` library provides:
- **Structured concurrency** - Automatic cleanup and error handling
- **Context integration** - Respects cancellation and deadlines
- **Panic recovery** - Converts panics to errors instead of crashing
- **Production-tested** - Used in Sourcegraph's infrastructure

## Testing the Endpoint

Test successful order placement:

```bash
curl -s -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{"customer_id":"CUST-001","account_id":"ACC-001"}' | jq .
```

Response:

```json
{
  "order_id": "649cfc69-8323-4c60-8745-c7071506943d"
}
```

Test with restricted account:

```bash
curl -s -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{"customer_id":"CUST-001","account_id":"ACC-FRAUD"}' | jq .
```

This will return an error because ACC-FRAUD has fraud restrictions in the Wiremock stub.

## OpenAPI Schema

Check the auto-generated OpenAPI schema:

```bash
curl -s http://localhost:8090/openapi.json | jq '.paths["/v1/order"].post'
```

The framework automatically generates:
- Request body schema from `PlaceOrderRequest` struct
- Response schema from `PlaceOrderResponse` struct
- Proper content types

## What's Next

With both endpoints complete, let's set up the full observability infrastructure to see traces, metrics, and logs in Grafana.

[Next: Infrastructure & Observability →]({{< ref "/walkthroughs/orders-rest/09-infrastructure" >}})
