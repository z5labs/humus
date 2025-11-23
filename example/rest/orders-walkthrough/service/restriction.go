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

func (s *RestrictionClient) CheckRestrictions(ctx context.Context, accountID string) ([]Restriction, error) {
	u := fmt.Sprintf("%s/restrictions/%s", s.baseURL, accountID)

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

	var result struct {
		Restrictions []Restriction `json:"restrictions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Restrictions, nil
}
