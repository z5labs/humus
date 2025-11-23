package endpoint

import "github.com/z5labs/humus/example/rest/orders-walkthrough/service"

// Order represents a customer order in the API response.
type Order struct {
	OrderID    string `json:"order_id"`
	AccountID  string `json:"account_id"`
	CustomerID string `json:"customer_id"`
	Status     string `json:"status"`
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

// orderFromService converts a service.Order to an endpoint.Order for API responses.
func orderFromService(svcOrder service.Order) Order {
	return Order{
		OrderID:    svcOrder.OrderID,
		AccountID:  svcOrder.AccountID,
		CustomerID: svcOrder.CustomerID,
		Status:     string(svcOrder.Status),
	}
}
