package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// EligibilityResult contains the result of an eligibility check.
type EligibilityResult struct {
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

func (s *EligibilityClient) CheckEligibility(ctx context.Context, accountID string) (*EligibilityResult, error) {
	u := fmt.Sprintf("%s/eligibility/%s", s.baseURL, accountID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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

	var result EligibilityResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}
