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
