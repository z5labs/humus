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

// ListOrdersResponse is the response for listing orders with pagination.
type ListOrdersResponse struct {
	Orders   []Order  `json:"orders"`
	PageInfo PageInfo `json:"page_info"`
}

// QueryResult contains the result of a Query operation.
type QueryResult struct {
	Orders     []Order
	HasMore    bool
	NextCursor string
}

// Restriction represents a single restriction on an account.
type Restriction struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

// EligibilityResult contains the result of an eligibility check.
type EligibilityResult struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}
