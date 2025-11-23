package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEligibilityServiceClient_CheckEligibility(t *testing.T) {
	tests := []struct {
		name           string
		req            *CheckEligibilityRequest
		mockResponse   string
		mockStatusCode int
		wantErr        bool
		wantEligible   bool
		wantReason     string
	}{
		{
			name: "eligible account",
			req:  &CheckEligibilityRequest{AccountID: "ACC-001"},
			mockResponse: `{
				"eligible": true,
				"reason": ""
			}`,
			mockStatusCode: http.StatusOK,
			wantErr:        false,
			wantEligible:   true,
			wantReason:     "",
		},
		{
			name: "ineligible - account type not supported",
			req:  &CheckEligibilityRequest{AccountID: "ACC-INELIGIBLE"},
			mockResponse: `{
				"eligible": false,
				"reason": "Account type not supported for ordering"
			}`,
			mockStatusCode: http.StatusOK,
			wantErr:        false,
			wantEligible:   false,
			wantReason:     "Account type not supported for ordering",
		},
		{
			name: "ineligible - insufficient funds",
			req:  &CheckEligibilityRequest{AccountID: "ACC-NOFUNDS"},
			mockResponse: `{
				"eligible": false,
				"reason": "Insufficient funds"
			}`,
			mockStatusCode: http.StatusOK,
			wantErr:        false,
			wantEligible:   false,
			wantReason:     "Insufficient funds",
		},
		{
			name:           "non-200 status code",
			req:            &CheckEligibilityRequest{AccountID: "ACC-ERROR"},
			mockResponse:   ``,
			mockStatusCode: http.StatusInternalServerError,
			wantErr:        true,
			wantEligible:   false,
		},
		{
			name:           "malformed JSON response",
			req:            &CheckEligibilityRequest{AccountID: "ACC-BAD"},
			mockResponse:   `{"eligible": invalid json`,
			mockStatusCode: http.StatusOK,
			wantErr:        true,
			wantEligible:   false,
		},
		{
			name:           "service unavailable",
			req:            &CheckEligibilityRequest{AccountID: "ACC-DOWN"},
			mockResponse:   ``,
			mockStatusCode: http.StatusServiceUnavailable,
			wantErr:        true,
			wantEligible:   false,
		},
		{
			name:           "not found",
			req:            &CheckEligibilityRequest{AccountID: "ACC-NOTFOUND"},
			mockResponse:   `{"error": "account not found"}`,
			mockStatusCode: http.StatusNotFound,
			wantErr:        true,
			wantEligible:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/eligibility/"+tt.req.AccountID, r.URL.Path)

				w.WriteHeader(tt.mockStatusCode)
				_, _ = w.Write([]byte(tt.mockResponse))
			}))
			defer server.Close()

			client := NewEligibilityClient(server.URL, http.DefaultClient)
			resp, err := client.CheckEligibility(context.Background(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)
			require.Equal(t, tt.wantEligible, resp.Eligible)
			if tt.wantReason != "" {
				require.Equal(t, tt.wantReason, resp.Reason)
			}
		})
	}
}
