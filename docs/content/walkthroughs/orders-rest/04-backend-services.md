---
title: Backend Services
description: Implement data storage, restriction, and eligibility service clients
weight: 4
type: docs
slug: backend-services
---

Let's implement the three backend services that power our order management system: a data storage service for persisting orders, and two validation services that check if an order can be placed.

## Data Service

The Data Service provides a DynamoDB-like interface for storing and querying orders. Following Go idioms, the `endpoint` package (consumer) defines the `DataService` interface, while the `service` package (provider) defines the types and implements the interface.

### HTTP Client Implementation

Create `service/data.go` with the types and HTTP client implementation:

```go
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

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

// QueryRequest contains the parameters for a Query operation.
type QueryRequest struct {
	AccountID string      `json:"account_id"`
	Status    OrderStatus `json:"status,omitempty"`
	Cursor    string      `json:"cursor,omitempty"`
	Limit     int         `json:"limit"`
}

// QueryResponse contains the result of a Query operation.
type QueryResponse struct {
	Orders     []Order `json:"orders"`
	HasMore    bool    `json:"has_more"`
	NextCursor string  `json:"next_cursor"`
}

// PutItemRequest contains the parameters for a PutItem operation.
type PutItemRequest struct {
	Order Order `json:"order"`
}

// PutItemResponse contains the result of a PutItem operation.
type PutItemResponse struct {
}

// DataClient is a client for the data service.
type DataClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewDataClient creates a new data service client.
func NewDataClient(baseURL string, httpClient *http.Client) *DataClient {
	return &DataClient{
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}
```

### Query Implementation

The Query method sends all parameters in a JSON request body, following DynamoDB's API pattern:

```go
func (s *DataClient) Query(ctx context.Context, req *QueryRequest) (*QueryResponse, error) {
	u := s.baseURL + "/data/orders"

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(req); err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result QueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}
```

### PutItem Implementation

```go
func (s *DataClient) PutItem(ctx context.Context, req *PutItemRequest) (*PutItemResponse, error) {
	u := s.baseURL + "/data/orders"

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(req); err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return &PutItemResponse{}, nil
}
```

## Restriction Service

Create `service/restriction.go`:

```go
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Restriction represents a single restriction on an account.
type Restriction struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

// CheckRestrictionsRequest contains the parameters for checking account restrictions.
type CheckRestrictionsRequest struct {
	AccountID string `json:"account_id"`
}

// CheckRestrictionsResponse contains the result of a restriction check.
type CheckRestrictionsResponse struct {
	Restrictions []Restriction `json:"restrictions"`
}

// RestrictionClient is a client for the restriction service.
type RestrictionClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewRestrictionClient creates a new restriction service client.
func NewRestrictionClient(baseURL string, httpClient *http.Client) *RestrictionClient {
	return &RestrictionClient{
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

func (s *RestrictionClient) CheckRestrictions(ctx context.Context, req *CheckRestrictionsRequest) (*CheckRestrictionsResponse, error) {
	u := fmt.Sprintf("%s/restrictions/%s", s.baseURL, req.AccountID)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result CheckRestrictionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}
```

## Eligibility Service

Create `service/eligibility.go`:

```go
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// CheckEligibilityRequest contains the parameters for checking order eligibility.
type CheckEligibilityRequest struct {
	AccountID string `json:"account_id"`
}

// CheckEligibilityResponse contains the result of an eligibility check.
type CheckEligibilityResponse struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}

// EligibilityClient is a client for the eligibility service.
type EligibilityClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewEligibilityClient creates a new eligibility service client.
func NewEligibilityClient(baseURL string, httpClient *http.Client) *EligibilityClient {
	return &EligibilityClient{
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

func (s *EligibilityClient) CheckEligibility(ctx context.Context, req *CheckEligibilityRequest) (*CheckEligibilityResponse, error) {
	u := fmt.Sprintf("%s/eligibility/%s", s.baseURL, req.AccountID)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result CheckEligibilityResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}
```

## Business Logic Scenarios

The Wiremock stubs provide several test scenarios:

**Restriction Service:**
- `ACC-001` - No restrictions (order can proceed)
- `ACC-FRAUD` - Fraud restriction (order blocked)
- `ACC-BLOCKED` - Multiple restrictions

**Eligibility Service:**
- `ACC-001` - Eligible (order can proceed)
- `ACC-INELIGIBLE` - Ineligible - account type not supported
- `ACC-NOFUNDS` - Ineligible - insufficient funds

## Common Patterns

All three service clients follow the same structure:
1. **Export domain types** - `Restriction`, `Order`, `OrderStatus`
2. **Define Request/Response types** - Each method takes a `*Request` and returns a `*Response`
3. **Export client struct** - Holds `baseURL` and `httpClient`
4. **Constructor function** - Returns pointer to concrete type
5. **Methods with consistent signature** - `(ctx context.Context, req *Request) (*Response, error)`

**Key Benefits of Request/Response Pattern:**
- **Extensibility** - Easy to add new fields without breaking method signatures
- **Consistency** - All service methods follow the same pattern
- **Clarity** - Request types document what data is needed
- **Testability** - Easy to construct test cases with different request parameters

**Key Architectural Decision:** Services define their own types (like `Restriction`, `CheckRestrictionsResponse`, `Order`, `OrderStatus`) and never import from the endpoint package. This ensures clean separation of concerns and prevents circular dependencies.

**Data Service Specific Patterns:**
1. **DynamoDB-Like API** - All parameters sent via JSON request body instead of query parameters, matching DynamoDB's API pattern
2. **Context Propagation** - All methods accept `context.Context` for timeouts and trace propagation
3. **Error Wrapping** - Use `fmt.Errorf("message: %w", err)` for error chains
4. **Resource Cleanup** - Use `defer func() { _ = resp.Body.Close() }()` to ensure bodies are closed while satisfying the linter's error checking
5. **Status Code Validation** - Check for expected HTTP status codes
6. **Automatic Request Body Reuse** - Passing `*bytes.Buffer` to `http.NewRequestWithContext()` automatically sets `GetBody`, enabling HTTP redirects and retries without manual configuration
7. **Optional Fields** - Use `omitempty` JSON tags for optional parameters (status, cursor) so they're omitted when empty

### Idiomatic Go: Consumer-Defined Interfaces

Following idiomatic Go, **interfaces are defined by the consumer, not the provider**. The service package provides concrete implementations, while the endpoint package defines the interfaces it needs:

**`endpoint/interfaces.go`:**
```go
type DataService interface {
    Query(ctx context.Context, req *service.QueryRequest) (*service.QueryResponse, error)
    PutItem(ctx context.Context, req *service.PutItemRequest) (*service.PutItemResponse, error)
}

type RestrictionService interface {
    CheckRestrictions(ctx context.Context, req *service.CheckRestrictionsRequest) (*service.CheckRestrictionsResponse, error)
}

type EligibilityService interface {
    CheckEligibility(ctx context.Context, req *service.CheckEligibilityRequest) (*service.CheckEligibilityResponse, error)
}
```

This approach:
- Keeps dependencies minimal (endpoints only depend on methods they use)
- Makes testing easier (mock only what you need)
- Follows Go proverbs: "Accept interfaces, return structs"

## Testing with httptest.Server

The service clients include comprehensive table-driven tests using `httptest.Server`:

**`service/restriction_test.go`:**
```go
func TestRestrictionServiceClient_CheckRestrictions(t *testing.T) {
    tests := []struct {
        name             string
        req              *CheckRestrictionsRequest
        mockResponse     string
        mockStatusCode   int
        wantErr          bool
        wantRestrictions int
    }{
        {
            name: "no restrictions",
            req:  &CheckRestrictionsRequest{AccountID: "ACC-001"},
            mockResponse: `{
                "restrictions": []
            }`,
            mockStatusCode:   http.StatusOK,
            wantErr:          false,
            wantRestrictions: 0,
        },
        // ... more test cases
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.WriteHeader(tt.mockStatusCode)
                _, _ = w.Write([]byte(tt.mockResponse))
            }))
            defer server.Close()

            client := NewRestrictionClient(server.URL, http.DefaultClient)
            resp, err := client.CheckRestrictions(context.Background(), tt.req)

            if tt.wantErr {
                require.Error(t, err)
                return
            }

            require.NoError(t, err)
            require.Len(t, resp.Restrictions, tt.wantRestrictions)
        })
    }
}
```

Key testing patterns:
- **Table-driven tests** - Easy to add new scenarios
- **Request/Response types** - Clear test case structure
- **httptest.Server** - Mock HTTP backends without network calls
- **testify/require** - Fail-fast assertions (repository standard)

See `service/*_test.go` for complete test suites covering success cases, errors, and edge cases.

## What's Next

With all backend services implemented and tested, let's implement the first endpoint.

[Next: List Orders Endpoint →]({{< ref "/walkthroughs/orders-rest/06-list-orders-endpoint" >}})
