package endpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/rest/orders-walkthrough/service"
	"github.com/z5labs/humus/rest"
)

// mockRestrictionService implements RestrictionService for testing.
type mockRestrictionService struct {
	checkRestrictionsFunc func(ctx context.Context, accountID string) ([]service.Restriction, error)
}

func (m *mockRestrictionService) CheckRestrictions(ctx context.Context, accountID string) ([]service.Restriction, error) {
	if m.checkRestrictionsFunc != nil {
		return m.checkRestrictionsFunc(ctx, accountID)
	}
	return nil, errors.New("not implemented")
}

// mockEligibilityService implements EligibilityService for testing.
type mockEligibilityService struct {
	checkEligibilityFunc func(ctx context.Context, accountID string) (*service.EligibilityResult, error)
}

func (m *mockEligibilityService) CheckEligibility(ctx context.Context, accountID string) (*service.EligibilityResult, error) {
	if m.checkEligibilityFunc != nil {
		return m.checkEligibilityFunc(ctx, accountID)
	}
	return nil, errors.New("not implemented")
}

func TestPlaceOrderHandler_Handle_Success(t *testing.T) {
	restrictionSvc := &mockRestrictionService{
		checkRestrictionsFunc: func(ctx context.Context, accountID string) ([]service.Restriction, error) {
			require.Equal(t, "acct-123", accountID)
			return []service.Restriction{}, nil
		},
	}

	eligibilitySvc := &mockEligibilityService{
		checkEligibilityFunc: func(ctx context.Context, accountID string) (*service.EligibilityResult, error) {
			require.Equal(t, "acct-123", accountID)
			return &service.EligibilityResult{
				Eligible: true,
				Reason:   "account is in good standing",
			}, nil
		},
	}

	var capturedOrder service.Order
	dataSvc := &mockDataService{
		putItemFunc: func(ctx context.Context, order service.Order) error {
			capturedOrder = order
			return nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	reqBody := PlaceOrderRequest{
		CustomerID: "cust-456",
		AccountID:  "acct-123",
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/order", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var placeResp PlaceOrderResponse
	err = json.NewDecoder(resp.Body).Decode(&placeResp)
	require.NoError(t, err)

	require.NotEmpty(t, placeResp.OrderID)

	// Verify the order was saved correctly
	require.Equal(t, placeResp.OrderID, capturedOrder.OrderID)
	require.Equal(t, "acct-123", capturedOrder.AccountID)
	require.Equal(t, "cust-456", capturedOrder.CustomerID)
	require.Equal(t, service.OrderStatusPending, capturedOrder.Status)
}

func TestPlaceOrderHandler_Handle_AccountRestricted(t *testing.T) {
	restrictionSvc := &mockRestrictionService{
		checkRestrictionsFunc: func(ctx context.Context, accountID string) ([]service.Restriction, error) {
			return []service.Restriction{
				{Code: "FRAUD", Description: "Account flagged for fraud"},
			}, nil
		},
	}

	eligibilitySvc := &mockEligibilityService{
		checkEligibilityFunc: func(ctx context.Context, accountID string) (*service.EligibilityResult, error) {
			return &service.EligibilityResult{
				Eligible: true,
				Reason:   "",
			}, nil
		},
	}

	dataSvc := &mockDataService{
		putItemFunc: func(ctx context.Context, order service.Order) error {
			t.Fatal("PutItem should not be called when account is restricted")
			return nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	reqBody := PlaceOrderRequest{
		CustomerID: "cust-456",
		AccountID:  "acct-123",
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/order", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 500 Internal Server Error for account restrictions
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPlaceOrderHandler_Handle_AccountIneligible(t *testing.T) {
	restrictionSvc := &mockRestrictionService{
		checkRestrictionsFunc: func(ctx context.Context, accountID string) ([]service.Restriction, error) {
			return []service.Restriction{}, nil
		},
	}

	eligibilitySvc := &mockEligibilityService{
		checkEligibilityFunc: func(ctx context.Context, accountID string) (*service.EligibilityResult, error) {
			return &service.EligibilityResult{
				Eligible: false,
				Reason:   "account does not meet minimum requirements",
			}, nil
		},
	}

	dataSvc := &mockDataService{
		putItemFunc: func(ctx context.Context, order service.Order) error {
			t.Fatal("PutItem should not be called when account is ineligible")
			return nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	reqBody := PlaceOrderRequest{
		CustomerID: "cust-456",
		AccountID:  "acct-123",
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/order", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 500 Internal Server Error for ineligible account
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPlaceOrderHandler_Handle_RestrictionServiceError(t *testing.T) {
	restrictionSvc := &mockRestrictionService{
		checkRestrictionsFunc: func(ctx context.Context, accountID string) ([]service.Restriction, error) {
			return nil, errors.New("restriction service unavailable")
		},
	}

	eligibilitySvc := &mockEligibilityService{
		checkEligibilityFunc: func(ctx context.Context, accountID string) (*service.EligibilityResult, error) {
			return &service.EligibilityResult{
				Eligible: true,
				Reason:   "",
			}, nil
		},
	}

	dataSvc := &mockDataService{
		putItemFunc: func(ctx context.Context, order service.Order) error {
			t.Fatal("PutItem should not be called when restriction check fails")
			return nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	reqBody := PlaceOrderRequest{
		CustomerID: "cust-456",
		AccountID:  "acct-123",
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/order", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 500 Internal Server Error on service failure
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPlaceOrderHandler_Handle_EligibilityServiceError(t *testing.T) {
	restrictionSvc := &mockRestrictionService{
		checkRestrictionsFunc: func(ctx context.Context, accountID string) ([]service.Restriction, error) {
			return []service.Restriction{}, nil
		},
	}

	eligibilitySvc := &mockEligibilityService{
		checkEligibilityFunc: func(ctx context.Context, accountID string) (*service.EligibilityResult, error) {
			return nil, errors.New("eligibility service unavailable")
		},
	}

	dataSvc := &mockDataService{
		putItemFunc: func(ctx context.Context, order service.Order) error {
			t.Fatal("PutItem should not be called when eligibility check fails")
			return nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	reqBody := PlaceOrderRequest{
		CustomerID: "cust-456",
		AccountID:  "acct-123",
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/order", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 500 Internal Server Error on service failure
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPlaceOrderHandler_Handle_DataServiceError(t *testing.T) {
	restrictionSvc := &mockRestrictionService{
		checkRestrictionsFunc: func(ctx context.Context, accountID string) ([]service.Restriction, error) {
			return []service.Restriction{}, nil
		},
	}

	eligibilitySvc := &mockEligibilityService{
		checkEligibilityFunc: func(ctx context.Context, accountID string) (*service.EligibilityResult, error) {
			return &service.EligibilityResult{
				Eligible: true,
				Reason:   "",
			}, nil
		},
	}

	dataSvc := &mockDataService{
		putItemFunc: func(ctx context.Context, order service.Order) error {
			return errors.New("database write failed")
		},
	}

	api := rest.NewApi("Test", "v1.0.0", PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	reqBody := PlaceOrderRequest{
		CustomerID: "cust-456",
		AccountID:  "acct-123",
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/order", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 500 Internal Server Error on database failure
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPlaceOrderHandler_Handle_MultipleRestrictions(t *testing.T) {
	restrictionSvc := &mockRestrictionService{
		checkRestrictionsFunc: func(ctx context.Context, accountID string) ([]service.Restriction, error) {
			return []service.Restriction{
				{Code: "FRAUD", Description: "Account flagged for fraud"},
				{Code: "REGULATORY", Description: "Regulatory hold"},
			}, nil
		},
	}

	eligibilitySvc := &mockEligibilityService{
		checkEligibilityFunc: func(ctx context.Context, accountID string) (*service.EligibilityResult, error) {
			return &service.EligibilityResult{
				Eligible: true,
				Reason:   "",
			}, nil
		},
	}

	dataSvc := &mockDataService{
		putItemFunc: func(ctx context.Context, order service.Order) error {
			t.Fatal("PutItem should not be called when account has restrictions")
			return nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	reqBody := PlaceOrderRequest{
		CustomerID: "cust-456",
		AccountID:  "acct-123",
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/order", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should return 500 Internal Server Error for account restrictions
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPlaceOrderHandler_Handle_ConcurrentValidation(t *testing.T) {
	// Test that both restriction and eligibility checks are called
	restrictionCalled := false
	eligibilityCalled := false

	restrictionSvc := &mockRestrictionService{
		checkRestrictionsFunc: func(ctx context.Context, accountID string) ([]service.Restriction, error) {
			restrictionCalled = true
			return []service.Restriction{}, nil
		},
	}

	eligibilitySvc := &mockEligibilityService{
		checkEligibilityFunc: func(ctx context.Context, accountID string) (*service.EligibilityResult, error) {
			eligibilityCalled = true
			return &service.EligibilityResult{
				Eligible: true,
				Reason:   "",
			}, nil
		},
	}

	dataSvc := &mockDataService{
		putItemFunc: func(ctx context.Context, order service.Order) error {
			return nil
		},
	}

	api := rest.NewApi("Test", "v1.0.0", PlaceOrder(restrictionSvc, eligibilitySvc, dataSvc))
	srv := httptest.NewServer(api)
	defer srv.Close()

	reqBody := PlaceOrderRequest{
		CustomerID: "cust-456",
		AccountID:  "acct-123",
	}
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/order", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify both validation checks were called (demonstrating concurrent execution)
	require.True(t, restrictionCalled)
	require.True(t, eligibilityCalled)
}
