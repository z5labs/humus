package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRestrictionServiceClient_CheckRestrictions(t *testing.T) {
	tests := []struct {
		name             string
		req              *CheckRestrictionsRequest
		mockResponse     string
		mockStatusCode   int
		wantErr          bool
		wantRestrictions int
	}{
		{
			name: "no restrictions",
			req:  &CheckRestrictionsRequest{AccountID: "ACC-001"},
			mockResponse: `{
				"restrictions": []
			}`,
			mockStatusCode:   http.StatusOK,
			wantErr:          false,
			wantRestrictions: 0,
		},
		{
			name: "single fraud restriction",
			req:  &CheckRestrictionsRequest{AccountID: "ACC-FRAUD"},
			mockResponse: `{
				"restrictions": [
					{"code": "FRAUD", "description": "Account flagged for fraudulent activity"}
				]
			}`,
			mockStatusCode:   http.StatusOK,
			wantErr:          false,
			wantRestrictions: 1,
		},
		{
			name: "multiple restrictions",
			req:  &CheckRestrictionsRequest{AccountID: "ACC-BLOCKED"},
			mockResponse: `{
				"restrictions": [
					{"code": "FRAUD", "description": "Account flagged for fraudulent activity"},
					{"code": "COMPLIANCE", "description": "Compliance hold"},
					{"code": "DEBT", "description": "Outstanding debt"}
				]
			}`,
			mockStatusCode:   http.StatusOK,
			wantErr:          false,
			wantRestrictions: 3,
		},
		{
			name:             "non-200 status code",
			req:              &CheckRestrictionsRequest{AccountID: "ACC-ERROR"},
			mockResponse:     ``,
			mockStatusCode:   http.StatusInternalServerError,
			wantErr:          true,
			wantRestrictions: 0,
		},
		{
			name:             "malformed JSON response",
			req:              &CheckRestrictionsRequest{AccountID: "ACC-BAD"},
			mockResponse:     `{"restrictions": [invalid json`,
			mockStatusCode:   http.StatusOK,
			wantErr:          true,
			wantRestrictions: 0,
		},
		{
			name:             "not found",
			req:              &CheckRestrictionsRequest{AccountID: "ACC-NOTFOUND"},
			mockResponse:     `{"error": "account not found"}`,
			mockStatusCode:   http.StatusNotFound,
			wantErr:          true,
			wantRestrictions: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/restrictions/"+tt.req.AccountID, r.URL.Path)

				w.WriteHeader(tt.mockStatusCode)
				_, _ = w.Write([]byte(tt.mockResponse))
			}))
			defer server.Close()

			client := NewRestrictionClient(server.URL, http.DefaultClient)
			resp, err := client.CheckRestrictions(context.Background(), tt.req)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)
			require.Len(t, resp.Restrictions, tt.wantRestrictions)

			// Validate restriction structure for cases with restrictions
			if tt.wantRestrictions > 0 {
				for _, r := range resp.Restrictions {
					require.NotEmpty(t, r.Code)
					require.NotEmpty(t, r.Description)
				}
			}
		})
	}
}
