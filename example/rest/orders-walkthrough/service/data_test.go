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
		req            *QueryRequest
		mockResponse   string
		mockStatusCode int
		wantErr        bool
		wantOrders     int
		wantHasMore    bool
		wantNextCursor string
	}{
		{
			name: "success with results",
			req: &QueryRequest{
				AccountID: "ACC-001",
				Limit:     10,
			},
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
			name: "success with no results",
			req: &QueryRequest{
				AccountID: "ACC-002",
				Limit:     10,
			},
			mockResponse:   `{"orders": [], "has_more": false, "next_cursor": ""}`,
			mockStatusCode: http.StatusOK,
			wantErr:        false,
			wantOrders:     0,
			wantHasMore:    false,
			wantNextCursor: "",
		},
		{
			name: "success with status filter",
			req: &QueryRequest{
				AccountID: "ACC-001",
				Status:    OrderStatusPending,
				Limit:     10,
			},
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
			name: "success with cursor pagination",
			req: &QueryRequest{
				AccountID: "ACC-001",
				Cursor:    "cursor-previous",
				Limit:     10,
			},
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
			name: "non-200 status code",
			req: &QueryRequest{
				AccountID: "ACC-001",
				Limit:     10,
			},
			mockResponse:   ``,
			mockStatusCode: http.StatusInternalServerError,
			wantErr:        true,
		},
		{
			name: "malformed JSON response",
			req: &QueryRequest{
				AccountID: "ACC-001",
				Limit:     10,
			},
			mockResponse:   `{"orders": [invalid json`,
			mockStatusCode: http.StatusOK,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/data/orders", r.URL.Path)
				require.Equal(t, "application/json", r.Header.Get("Content-Type"))

				// Verify request body
				var reqBody QueryRequest
				err := json.NewDecoder(r.Body).Decode(&reqBody)
				require.NoError(t, err)
				require.Equal(t, tt.req.AccountID, reqBody.AccountID)
				require.Equal(t, tt.req.Limit, reqBody.Limit)
				if tt.req.Cursor != "" {
					require.Equal(t, tt.req.Cursor, reqBody.Cursor)
				}
				if tt.req.Status != "" {
					require.Equal(t, tt.req.Status, reqBody.Status)
				}

				w.WriteHeader(tt.mockStatusCode)
				_, _ = w.Write([]byte(tt.mockResponse))
			}))
			defer server.Close()

			client := NewDataClient(server.URL, http.DefaultClient)
			result, err := client.Query(context.Background(), tt.req)

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
		req            *PutItemRequest
		mockStatusCode int
		wantErr        bool
		validateBody   bool
	}{
		{
			name: "success",
			req: &PutItemRequest{
				Order: Order{
					OrderID:    "order-123",
					AccountID:  "ACC-001",
					CustomerID: "CUST-001",
					Status:     OrderStatusPending,
				},
			},
			mockStatusCode: http.StatusCreated,
			wantErr:        false,
			validateBody:   true,
		},
		{
			name: "non-201 status code",
			req: &PutItemRequest{
				Order: Order{
					OrderID:    "order-456",
					AccountID:  "ACC-002",
					CustomerID: "CUST-002",
					Status:     OrderStatusPending,
				},
			},
			mockStatusCode: http.StatusInternalServerError,
			wantErr:        true,
			validateBody:   false,
		},
		{
			name: "conflict status",
			req: &PutItemRequest{
				Order: Order{
					OrderID:    "order-789",
					AccountID:  "ACC-003",
					CustomerID: "CUST-003",
					Status:     OrderStatusPending,
				},
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
					var receivedReq PutItemRequest
					err := json.NewDecoder(r.Body).Decode(&receivedReq)
					require.NoError(t, err)
					require.Equal(t, tt.req.Order.OrderID, receivedReq.Order.OrderID)
					require.Equal(t, tt.req.Order.AccountID, receivedReq.Order.AccountID)
					require.Equal(t, tt.req.Order.CustomerID, receivedReq.Order.CustomerID)
					require.Equal(t, tt.req.Order.Status, receivedReq.Order.Status)
				}

				w.WriteHeader(tt.mockStatusCode)
			}))
			defer server.Close()

			client := NewDataClient(server.URL, http.DefaultClient)
			_, err := client.PutItem(context.Background(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
		})
	}
}
