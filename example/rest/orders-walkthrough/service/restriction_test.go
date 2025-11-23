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
		accountID        string
		mockResponse     string
		mockStatusCode   int
		wantErr          bool
		wantRestrictions int
	}{
		{
			name:      "no restrictions",
			accountID: "ACC-001",
			mockResponse: `{
				"account_id": "ACC-001",
				"restrictions": []
			}`,
			mockStatusCode:   http.StatusOK,
			wantErr:          false,
			wantRestrictions: 0,
		},
		{
			name:      "single fraud restriction",
			accountID: "ACC-FRAUD",
			mockResponse: `{
				"account_id": "ACC-FRAUD",
				"restrictions": [
					{"code": "FRAUD", "description": "Account flagged for fraudulent activity"}
				]
			}`,
			mockStatusCode:   http.StatusOK,
			wantErr:          false,
			wantRestrictions: 1,
		},
		{
			name:      "multiple restrictions",
			accountID: "ACC-BLOCKED",
			mockResponse: `{
				"account_id": "ACC-BLOCKED",
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
			accountID:        "ACC-ERROR",
			mockResponse:     ``,
			mockStatusCode:   http.StatusInternalServerError,
			wantErr:          true,
			wantRestrictions: 0,
		},
		{
			name:             "malformed JSON response",
			accountID:        "ACC-BAD",
			mockResponse:     `{"restrictions": [invalid json`,
			mockStatusCode:   http.StatusOK,
			wantErr:          true,
			wantRestrictions: 0,
		},
		{
			name:             "not found",
			accountID:        "ACC-NOTFOUND",
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
				require.Equal(t, "/restrictions/"+tt.accountID, r.URL.Path)

				w.WriteHeader(tt.mockStatusCode)
				_, _ = w.Write([]byte(tt.mockResponse))
			}))
			defer server.Close()

			client := NewRestrictionClient(server.URL, http.DefaultClient)
			restrictions, err := client.CheckRestrictions(context.Background(), tt.accountID)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Len(t, restrictions, tt.wantRestrictions)

			// Validate restriction structure for cases with restrictions
			if tt.wantRestrictions > 0 {
				for _, r := range restrictions {
					require.NotEmpty(t, r.Code)
					require.NotEmpty(t, r.Description)
				}
			}
		})
	}
}
