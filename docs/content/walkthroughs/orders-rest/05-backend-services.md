---
title: Backend Services
description: Implement restriction and eligibility service clients
weight: 5
type: docs
slug: backend-services
---

Let's implement the two validation services that check if an order can be placed.

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

### Idiomatic Go: Consumer-Defined Interfaces

Following idiomatic Go, **interfaces are defined by the consumer, not the provider**. The service package provides concrete implementations, while the endpoint package defines the interfaces it needs:

**`endpoint/interfaces.go`:**
```go
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

With all services implemented and tested, let's implement the first endpoint.

[Next: List Orders Endpoint →]({{< ref "/walkthroughs/orders-rest/06-list-orders-endpoint" >}})
