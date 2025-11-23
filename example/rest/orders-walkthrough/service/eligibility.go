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
