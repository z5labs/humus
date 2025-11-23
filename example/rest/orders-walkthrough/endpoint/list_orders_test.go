package endpoint

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
	"github.com/z5labs/humus/rest"
)

// mockDataService implements DataService for testing.
type mockDataService struct {
	queryFunc   func(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error)
	putItemFunc func(ctx context.Context, order service.Order) error
}

func (m *mockDataService) Query(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error) {
	if m.queryFunc != nil {
		return m.queryFunc(ctx, accountID, status, cursor, limit)
	}
	return nil, errors.New("not implemented")
}

func (m *mockDataService) PutItem(ctx context.Context, order service.Order) error {
	if m.putItemFunc != nil {
		return m.putItemFunc(ctx, order)
	}
	return errors.New("not implemented")
}

func TestListOrdersHandler_Produce_Success(t *testing.T) {
	expectedOrders := []Order{
		{OrderID: "order-1", AccountID: "acct-123", CustomerID: "cust-1", Status: "pending"},
		{OrderID: "order-2", AccountID: "acct-123", CustomerID: "cust-2", Status: "completed"},
	}

	dataSvc := &mockDataService{
		queryFunc: func(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error) {
			require.Equal(t, "acct-123", accountID)
			require.Equal(t, 10, limit)
			require.Equal(t, "", cursor)
			require.Equal(t, service.OrderStatusPending, status)

			return &service.QueryResult{
				Orders: []service.Order{
					{OrderID: "order-1", AccountID: "acct-123", CustomerID: "cust-1", Status: service.OrderStatusPending},
					{OrderID: "order-2", AccountID: "acct-123", CustomerID: "cust-2", Status: service.OrderStatusCompleted},
				},
				HasMore:    false,
				NextCursor: "",
			}, nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders?accountNumber=acct-123&limit=10&status=pending", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var listResp ListOrdersResponse
	err = json.NewDecoder(resp.Body).Decode(&listResp)
	require.NoError(t, err)

	require.Equal(t, expectedOrders, listResp.Orders)
	require.False(t, listResp.PageInfo.HasNextPage)
	require.Empty(t, listResp.PageInfo.EndCursor)
}

func TestListOrdersHandler_Produce_WithPagination(t *testing.T) {
	expectedCursor := "next-cursor-123"
	encodedInputCursor := base64.StdEncoding.EncodeToString([]byte("current-cursor"))

	dataSvc := &mockDataService{
		queryFunc: func(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error) {
			require.Equal(t, "acct-123", accountID)
			require.Equal(t, 20, limit)
			require.Equal(t, "current-cursor", cursor)

			return &service.QueryResult{
				Orders: []service.Order{
					{OrderID: "order-1", AccountID: "acct-123", CustomerID: "cust-1", Status: service.OrderStatusPending},
				},
				HasMore:    true,
				NextCursor: expectedCursor,
			}, nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders?accountNumber=acct-123&after="+encodedInputCursor+"&limit=20&status=pending", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var listResp ListOrdersResponse
	err = json.NewDecoder(resp.Body).Decode(&listResp)
	require.NoError(t, err)

	require.True(t, listResp.PageInfo.HasNextPage)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(expectedCursor)), listResp.PageInfo.EndCursor)
}

func TestListOrdersHandler_Produce_WithStatusFilter(t *testing.T) {
	dataSvc := &mockDataService{
		queryFunc: func(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error) {
			require.Equal(t, "acct-123", accountID)
			require.Equal(t, service.OrderStatusCompleted, status)

			return &service.QueryResult{
				Orders: []service.Order{
					{OrderID: "order-1", AccountID: "acct-123", CustomerID: "cust-1", Status: service.OrderStatusCompleted},
				},
				HasMore:    false,
				NextCursor: "",
			}, nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders?accountNumber=acct-123&limit=10&status=completed", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var listResp ListOrdersResponse
	err = json.NewDecoder(resp.Body).Decode(&listResp)
	require.NoError(t, err)

	require.Len(t, listResp.Orders, 1)
	require.Equal(t, "completed", listResp.Orders[0].Status)
}

func TestListOrdersHandler_Produce_DefaultsWhenOptionalParamsProvided(t *testing.T) {
	dataSvc := &mockDataService{
		queryFunc: func(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error) {
			// When optional params are provided with values matching defaults
			require.Equal(t, "", cursor)
			require.Equal(t, 10, limit)
			require.Equal(t, service.OrderStatusPending, status)

			return &service.QueryResult{
				Orders:     []service.Order{},
				HasMore:    false,
				NextCursor: "",
			}, nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders?accountNumber=acct-123&limit=10&status=pending", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestListOrdersHandler_Produce_InvalidLimitRejected(t *testing.T) {
	dataSvc := &mockDataService{}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders?accountNumber=acct-123&limit=invalid", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 400 Bad Request when limit parameter doesn't match regex
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestListOrdersHandler_Produce_ValidNumericLimit(t *testing.T) {
	dataSvc := &mockDataService{
		queryFunc: func(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error) {
			// Numeric limit value of 25 should be parsed and used
			require.Equal(t, 25, limit)

			return &service.QueryResult{
				Orders:     []service.Order{},
				HasMore:    false,
				NextCursor: "",
			}, nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders?accountNumber=acct-123&limit=25&status=pending", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestListOrdersHandler_Produce_DataServiceError(t *testing.T) {
	dataSvc := &mockDataService{
		queryFunc: func(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error) {
			return nil, errors.New("database connection failed")
		},
	}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders?accountNumber=acct-123&limit=10&status=pending", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 500 Internal Server Error on data service failure
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestListOrdersHandler_Produce_MissingRequiredAccountNumber(t *testing.T) {
	dataSvc := &mockDataService{}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 400 Bad Request when required parameter is missing
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestListOrdersHandler_Produce_InvalidStatusFilter(t *testing.T) {
	dataSvc := &mockDataService{}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders?accountNumber=acct-123&status=invalid-status", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 400 Bad Request when status filter doesn't match regex
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestListOrdersHandler_Produce_NextCursorWithoutHasMore(t *testing.T) {
	dataSvc := &mockDataService{
		queryFunc: func(ctx context.Context, accountID string, status service.OrderStatus, cursor string, limit int) (*service.QueryResult, error) {
			return &service.QueryResult{
				Orders: []service.Order{
					{OrderID: "order-1", AccountID: "acct-123", CustomerID: "cust-1", Status: service.OrderStatusPending},
				},
				HasMore:    false,
				NextCursor: "cursor-should-be-ignored",
			}, nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", ListOrders(dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/orders?accountNumber=acct-123&limit=10&status=pending", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var listResp ListOrdersResponse
	err = json.NewDecoder(resp.Body).Decode(&listResp)
	require.NoError(t, err)

	require.False(t, listResp.PageInfo.HasNextPage)
	// EndCursor should be empty when HasMore is false
	require.Empty(t, listResp.PageInfo.EndCursor)
}
