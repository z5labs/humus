package endpoint

import (
	"context"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
)

// DataService provides access to order data storage.
// Following idiomatic Go, the consumer (endpoint package) defines the interface.
type DataService interface {
	// Query retrieves orders for an account with optional filtering and pagination.
	Query(ctx context.Context, req *service.QueryRequest) (*service.QueryResponse, error)
	// PutItem stores a new order.
	PutItem(ctx context.Context, req *service.PutItemRequest) (*service.PutItemResponse, error)
}

// RestrictionService checks for account restrictions.
// Following idiomatic Go, the consumer (endpoint package) defines the interface.
type RestrictionService interface {
	// CheckRestrictions returns all restrictions for an account.
	CheckRestrictions(ctx context.Context, req *service.CheckRestrictionsRequest) (*service.CheckRestrictionsResponse, error)
}

// EligibilityService checks account eligibility for placing orders.
// Following idiomatic Go, the consumer (endpoint package) defines the interface.
type EligibilityService interface {
	// CheckEligibility determines if an account is eligible to place orders.
	CheckEligibility(ctx context.Context, req *service.CheckEligibilityRequest) (*service.CheckEligibilityResponse, error)
}
