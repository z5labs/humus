package endpoint

import (
	"context"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
)

// DataService provides access to order data storage.
// Following idiomatic Go, the consumer (endpoint package) defines the interface.
type DataService interface {
	// Query retrieves orders for an account with optional filtering and pagination.
	Query(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error)
	// PutItem stores a new order.
	PutItem(ctx context.Context, order service.Order) error
}

// RestrictionService checks for account restrictions.
// Following idiomatic Go, the consumer (endpoint package) defines the interface.
type RestrictionService interface {
	// CheckRestrictions returns all restrictions for an account.
	CheckRestrictions(ctx context.Context, accountID string) ([]service.Restriction, error)
}

// EligibilityService checks account eligibility for placing orders.
// Following idiomatic Go, the consumer (endpoint package) defines the interface.
type EligibilityService interface {
	// CheckEligibility determines if an account is eligible to place orders.
	CheckEligibility(ctx context.Context, accountID string) (*service.EligibilityResult, error)
}
