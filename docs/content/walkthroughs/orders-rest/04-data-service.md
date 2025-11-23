---
title: Data Service
description: Implement the storage service layer with HTTP client
weight: 4
type: docs
slug: data-service
---

The Data Service provides a DynamoDB-like interface for storing and querying orders. Following Go idioms, the `endpoint` package (consumer) defines the `DataService` interface, while the `service` package (provider) defines the types and implements the interface.

## HTTP Client Implementation

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

## Query Implementation

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

## PutItem Implementation

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

## Key Patterns

1. **DynamoDB-Like API** - All parameters sent via JSON request body instead of query parameters, matching DynamoDB's API pattern
2. **Context Propagation** - All methods accept `context.Context` for timeouts and trace propagation
3. **Error Wrapping** - Use `fmt.Errorf("message: %w", err)` for error chains
4. **Resource Cleanup** - Use `defer func() { _ = resp.Body.Close() }()` to ensure bodies are closed while satisfying the linter's error checking
5. **Status Code Validation** - Check for expected HTTP status codes
6. **Automatic Request Body Reuse** - Passing `*bytes.Buffer` to `http.NewRequestWithContext()` automatically sets `GetBody`, enabling HTTP redirects and retries without manual configuration
7. **Optional Fields** - Use `omitempty` JSON tags for optional parameters (status, cursor) so they're omitted when empty

## What's Next

Let's implement the restriction and eligibility services that validate order placement.

[Next: Backend Services →]({{< ref "/walkthroughs/orders-rest/05-backend-services" >}})
