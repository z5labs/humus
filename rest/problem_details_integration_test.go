// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProblemDetails_Integration_CustomErrorWithExtensions tests full HTTP lifecycle
// with custom errors that have extension fields
func TestProblemDetails_Integration_CustomErrorWithExtensions(t *testing.T) {
	type NotFoundError struct {
		ProblemDetail
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}

	type Request struct {
		ID string `json:"id"`
	}

	type Response struct {
		Data string `json:"data"`
	}

	handler := HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) {
		return nil, NotFoundError{
			ProblemDetail: ProblemDetail{
				Type:     "https://api.example.com/problems/not-found",
				Title:    "Resource Not Found",
				Status:   http.StatusNotFound,
				Detail:   "The requested resource does not exist",
				Instance: "/users/123",
			},
			ResourceType: "user",
			ResourceID:   "123",
		}
	})

	api := NewApi(
		"Test API",
		"v1.0.0",
		Operation(
			http.MethodPost,
			BasePath("/test"),
			HandleJson(handler),
			OnError(NewProblemDetailsErrorHandler()),
		),
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	// Make request
	body := bytes.NewReader([]byte(`{"id":"123"}`))
	resp, err := http.Post(srv.URL+"/test", "application/json", body)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Verify response
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

	// Decode and verify Problem Details with extensions
	var problem struct {
		Type         string `json:"type"`
		Title        string `json:"title"`
		Status       int    `json:"status"`
		Detail       string `json:"detail"`
		Instance     string `json:"instance"`
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(bodyBytes, &problem))
	require.Equal(t, "https://api.example.com/problems/not-found", problem.Type)
	require.Equal(t, "Resource Not Found", problem.Title)
	require.Equal(t, http.StatusNotFound, problem.Status)
	require.Equal(t, "The requested resource does not exist", problem.Detail)
	require.Equal(t, "/users/123", problem.Instance)
	require.Equal(t, "user", problem.ResourceType)
	require.Equal(t, "123", problem.ResourceID)
}

// TestProblemDetails_Integration_ValidationErrorWithMultipleFields tests complex extension fields
func TestProblemDetails_Integration_ValidationErrorWithMultipleFields(t *testing.T) {
	type FieldError struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	}

	type ValidationError struct {
		ProblemDetail
		InvalidFields []FieldError `json:"invalid_fields"`
	}

	type Request struct {
		Email string `json:"email"`
		Age   int    `json:"age"`
	}

	type Response struct {
		Success bool `json:"success"`
	}

	handler := HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) {
		return nil, ValidationError{
			ProblemDetail: ProblemDetail{
				Type:   "https://api.example.com/problems/validation",
				Title:  "Validation Failed",
				Status: http.StatusBadRequest,
				Detail: "One or more fields failed validation",
			},
			InvalidFields: []FieldError{
				{Field: "email", Message: "invalid email format"},
				{Field: "age", Message: "must be 18 or older"},
			},
		}
	})

	api := NewApi(
		"Test API",
		"v1.0.0",
		Operation(
			http.MethodPost,
			BasePath("/validate"),
			HandleJson(handler),
			OnError(NewProblemDetailsErrorHandler()),
		),
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	// Make request
	body := bytes.NewReader([]byte(`{"email":"invalid","age":15}`))
	resp, err := http.Post(srv.URL+"/validate", "application/json", body)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Verify response
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

	// Decode and verify Problem Details
	var problem struct {
		Type          string `json:"type"`
		Title         string `json:"title"`
		Status        int    `json:"status"`
		Detail        string `json:"detail"`
		InvalidFields []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"invalid_fields"`
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(bodyBytes, &problem))
	require.Equal(t, "https://api.example.com/problems/validation", problem.Type)
	require.Equal(t, "Validation Failed", problem.Title)
	require.Equal(t, http.StatusBadRequest, problem.Status)
	require.Len(t, problem.InvalidFields, 2)
	require.Equal(t, "email", problem.InvalidFields[0].Field)
	require.Equal(t, "invalid email format", problem.InvalidFields[0].Message)
	require.Equal(t, "age", problem.InvalidFields[1].Field)
	require.Equal(t, "must be 18 or older", problem.InvalidFields[1].Message)
}

// TestProblemDetails_Integration_ExistingFrameworkErrors tests that existing framework errors
// are properly converted to Problem Details
func TestProblemDetails_Integration_ExistingFrameworkErrors(t *testing.T) {
	type Request struct {
		Data string `json:"data"`
	}

	type Response struct {
		Result string `json:"result"`
	}

	t.Run("missing required parameter", func(t *testing.T) {
		handler := HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) {
			// This would normally be caught by parameter validation, but we simulate it
			return nil, BadRequestError{
				Cause: MissingRequiredParameterError{
					Parameter: "user_id",
					In:        "query",
				},
			}
		})

		api := NewApi(
			"Test API",
			"v1.0.0",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
				OnError(NewProblemDetailsErrorHandler()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := bytes.NewReader([]byte(`{"data":"test"}`))
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

		var problem ProblemDetail
		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(bodyBytes, &problem))
		require.Equal(t, "Missing Required Parameter", problem.Title)
		require.Equal(t, http.StatusBadRequest, problem.Status)
	})

	t.Run("unauthorized error", func(t *testing.T) {
		handler := HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) {
			return nil, UnauthorizedError{
				Cause: InvalidJWTError{
					Parameter: "Authorization",
					In:        "header",
					Cause:     errors.New("token expired"),
				},
			}
		})

		api := NewApi(
			"Test API",
			"v1.0.0",
			Operation(
				http.MethodPost,
				BasePath("/test"),
				HandleJson(handler),
				OnError(NewProblemDetailsErrorHandler()),
			),
		)

		srv := httptest.NewServer(api)
		defer srv.Close()

		body := bytes.NewReader([]byte(`{"data":"test"}`))
		resp, err := http.Post(srv.URL+"/test", "application/json", body)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

		var problem ProblemDetail
		bodyBytes, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(bodyBytes, &problem))
		require.Equal(t, "Invalid JWT Token", problem.Title)
		require.Equal(t, http.StatusUnauthorized, problem.Status)
	})
}

// TestProblemDetails_Integration_GenericError tests that generic errors are converted
// to 500 Internal Server Error with hardcoded detail message for security
func TestProblemDetails_Integration_GenericError(t *testing.T) {
	type Request struct {
		ID string `json:"id"`
	}

	type Response struct {
		Data string `json:"data"`
	}

	handler := HandlerFunc[Request, Response](func(ctx context.Context, req *Request) (*Response, error) {
		return nil, errors.New("database connection failed with password: secret123")
	})

	api := NewApi(
		"Test API",
		"v1.0.0",
		Operation(
			http.MethodPost,
			BasePath("/test"),
			HandleJson(handler),
			OnError(NewProblemDetailsErrorHandler()),
		),
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	body := bytes.NewReader([]byte(`{"id":"123"}`))
	resp, err := http.Post(srv.URL+"/test", "application/json", body)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

	var problem ProblemDetail
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(bodyBytes, &problem))
	require.Equal(t, "about:blank", problem.Type)
	require.Equal(t, "Internal Server Error", problem.Title)
	require.Equal(t, http.StatusInternalServerError, problem.Status)
	require.Equal(t, "An internal server error occurred.", problem.Detail, "Generic errors should use hardcoded detail message for security")
}
