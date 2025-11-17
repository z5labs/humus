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
	"io"
	"net/http"
	"net/url"
	"strconv"
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

// QueryResult contains the result of a Query operation.
type QueryResult struct {
	Orders     []Order
	HasMore    bool
	NextCursor string
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

```go
func (s *DataClient) Query(ctx context.Context, accountID string, status OrderStatus, cursor string, limit int) (*QueryResult, error) {
	u, err := url.Parse(s.baseURL + "/data/orders")
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}

	q := u.Query()
	q.Set("account_id", accountID)
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if status != "" {
		q.Set("status", string(status))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result struct {
		Orders     []Order `json:"orders"`
		HasMore    bool    `json:"has_more"`
		NextCursor string  `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &QueryResult{
		Orders:     result.Orders,
		HasMore:    result.HasMore,
		NextCursor: result.NextCursor,
	}, nil
}
```

## PutItem Implementation

```go
func (s *DataClient) PutItem(ctx context.Context, order Order) error {
	u := s.baseURL + "/data/orders"

	body, err := json.Marshal(order)
	if err != nil {
		return fmt.Errorf("failed to marshal order: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}
```

## Key Patterns

1. **Context Propagation** - All methods accept `context.Context` for timeouts and trace propagation
2. **Error Wrapping** - Use `fmt.Errorf("message: %w", err)` for error chains
3. **Resource Cleanup** - Use `defer func() { _ = resp.Body.Close() }()` to ensure bodies are closed while satisfying the linter's error checking
4. **Status Code Validation** - Check for expected HTTP status codes
5. **Reusable Request Bodies** - Set `req.GetBody` to support HTTP redirects and retries by allowing the HTTP client to recreate the request body when needed

## What's Next

Let's implement the restriction and eligibility services that validate order placement.

[Next: Backend Services →]({{< ref "/walkthroughs/orders-rest/05-backend-services" >}})
