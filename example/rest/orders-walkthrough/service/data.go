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

func (s *DataClient) Query(ctx context.Context, accountID string, status OrderStatus, cursor string, limit int) (*QueryResult, error) {
	u := s.baseURL + "/data/orders"

	// Build request body like DynamoDB
	reqBody := struct {
		AccountID string      `json:"account_id"`
		Status    OrderStatus `json:"status,omitempty"`
		Cursor    string      `json:"cursor,omitempty"`
		Limit     int         `json:"limit"`
	}{
		AccountID: accountID,
		Status:    status,
		Cursor:    cursor,
		Limit:     limit,
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(reqBody); err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

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

func (s *DataClient) PutItem(ctx context.Context, order Order) error {
	u := s.baseURL + "/data/orders"

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(order); err != nil {
		return fmt.Errorf("failed to encode order: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

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
