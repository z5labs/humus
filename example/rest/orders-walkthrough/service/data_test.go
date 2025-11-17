package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDataServiceClient_Query(t *testing.T) {
	tests := []struct {
		name           string
		accountID      string
		status         OrderStatus
		cursor         string
		limit          int
		mockResponse   string
		mockStatusCode int
		wantErr        bool
		wantOrders     int
		wantHasMore    bool
		wantNextCursor string
	}{
		{
			name:      "success with results",
			accountID: "ACC-001",
			limit:     10,
			mockResponse: `{
				"orders": [
					{"order_id": "order-1", "account_id": "ACC-001", "customer_id": "CUST-001", "status": "pending"},
					{"order_id": "order-2", "account_id": "ACC-001", "customer_id": "CUST-001", "status": "completed"}
				],
				"has_more": true,
				"next_cursor": "cursor-abc"
			}`,
			mockStatusCode: http.StatusOK,
			wantErr:        false,
			wantOrders:     2,
			wantHasMore:    true,
			wantNextCursor: "cursor-abc",
		},
		{
			name:           "success with no results",
			accountID:      "ACC-002",
			limit:          10,
			mockResponse:   `{"orders": [], "has_more": false, "next_cursor": ""}`,
			mockStatusCode: http.StatusOK,
			wantErr:        false,
			wantOrders:     0,
			wantHasMore:    false,
			wantNextCursor: "",
		},
		{
			name:      "success with status filter",
			accountID: "ACC-001",
			status:    OrderStatusPending,
			limit:     10,
			mockResponse: `{
				"orders": [
					{"order_id": "order-1", "account_id": "ACC-001", "customer_id": "CUST-001", "status": "pending"}
				],
				"has_more": false,
				"next_cursor": ""
			}`,
			mockStatusCode: http.StatusOK,
			wantErr:        false,
			wantOrders:     1,
			wantHasMore:    false,
		},
		{
			name:      "success with cursor pagination",
			accountID: "ACC-001",
			cursor:    "cursor-previous",
			limit:     10,
			mockResponse: `{
				"orders": [
					{"order_id": "order-3", "account_id": "ACC-001", "customer_id": "CUST-001", "status": "completed"}
				],
				"has_more": false,
				"next_cursor": ""
			}`,
			mockStatusCode: http.StatusOK,
			wantErr:        false,
			wantOrders:     1,
			wantHasMore:    false,
		},
		{
			name:           "non-200 status code",
			accountID:      "ACC-001",
			limit:          10,
			mockResponse:   ``,
			mockStatusCode: http.StatusInternalServerError,
			wantErr:        true,
		},
		{
			name:           "malformed JSON response",
			accountID:      "ACC-001",
			limit:          10,
			mockResponse:   `{"orders": [invalid json`,
			mockStatusCode: http.StatusOK,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/data/orders", r.URL.Path)

				// Verify query parameters
				q := r.URL.Query()
				require.Equal(t, tt.accountID, q.Get("account_id"))
				if tt.cursor != "" {
					require.Equal(t, tt.cursor, q.Get("cursor"))
				}
				if tt.status != "" {
					require.Equal(t, string(tt.status), q.Get("status"))
				}

				w.WriteHeader(tt.mockStatusCode)
				_, _ = w.Write([]byte(tt.mockResponse))
			}))
			defer server.Close()

			client := NewDataClient(server.URL, http.DefaultClient)
			result, err := client.Query(context.Background(), tt.accountID, tt.status, tt.cursor, tt.limit)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, result.Orders, tt.wantOrders)
			require.Equal(t, tt.wantHasMore, result.HasMore)
			require.Equal(t, tt.wantNextCursor, result.NextCursor)
		})
	}
}

func TestDataServiceClient_PutItem(t *testing.T) {
	tests := []struct {
		name           string
		order          Order
		mockStatusCode int
		wantErr        bool
		validateBody   bool
	}{
		{
			name: "success",
			order: Order{
				OrderID:    "order-123",
				AccountID:  "ACC-001",
				CustomerID: "CUST-001",
				Status:     OrderStatusPending,
			},
			mockStatusCode: http.StatusCreated,
			wantErr:        false,
			validateBody:   true,
		},
		{
			name: "non-201 status code",
			order: Order{
				OrderID:    "order-456",
				AccountID:  "ACC-002",
				CustomerID: "CUST-002",
				Status:     OrderStatusPending,
			},
			mockStatusCode: http.StatusInternalServerError,
			wantErr:        true,
			validateBody:   false,
		},
		{
			name: "conflict status",
			order: Order{
				OrderID:    "order-789",
				AccountID:  "ACC-003",
				CustomerID: "CUST-003",
				Status:     OrderStatusPending,
			},
			mockStatusCode: http.StatusConflict,
			wantErr:        true,
			validateBody:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/data/orders", r.URL.Path)
				require.Equal(t, "application/json", r.Header.Get("Content-Type"))

				if tt.validateBody {
					var receivedOrder Order
					err := json.NewDecoder(r.Body).Decode(&receivedOrder)
					require.NoError(t, err)
					require.Equal(t, tt.order.OrderID, receivedOrder.OrderID)
					require.Equal(t, tt.order.AccountID, receivedOrder.AccountID)
					require.Equal(t, tt.order.CustomerID, receivedOrder.CustomerID)
					require.Equal(t, tt.order.Status, receivedOrder.Status)
				}

				w.WriteHeader(tt.mockStatusCode)
			}))
			defer server.Close()

			client := NewDataClient(server.URL, http.DefaultClient)
			err := client.PutItem(context.Background(), tt.order)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
		})
	}
}
