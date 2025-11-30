// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProblemDetail_Error verifies the Error() method implementation
func TestProblemDetail_Error(t *testing.T) {
	t.Run("returns detail when present", func(t *testing.T) {
		pd := ProblemDetail{
			Type:   "about:blank",
			Title:  "Error Title",
			Status: 500,
			Detail: "Specific error details",
		}

		require.Equal(t, "Specific error details", pd.Error())
	})

	t.Run("returns title when detail is empty", func(t *testing.T) {
		pd := ProblemDetail{
			Type:   "about:blank",
			Title:  "Error Title",
			Status: 500,
		}

		require.Equal(t, "Error Title", pd.Error())
	})
}

// TestProblemDetail_StatusCode verifies the private statusCode() method
func TestProblemDetail_StatusCode(t *testing.T) {
	pd := ProblemDetail{
		Type:   "about:blank",
		Title:  "Error",
		Status: http.StatusBadRequest,
	}

	// Verify it implements problemDetailMarker
	var marker problemDetailMarker = pd
	require.Equal(t, http.StatusBadRequest, marker.statusCode())
}

// TestProblemDetail_Embedding verifies that embedding works correctly
func TestProblemDetail_Embedding(t *testing.T) {
	type ValidationError struct {
		ProblemDetail
		InvalidFields []string `json:"invalid_fields"`
	}

	t.Run("implements error interface", func(t *testing.T) {
		err := ValidationError{
			ProblemDetail: ProblemDetail{
				Type:   "https://example.com/validation",
				Title:  "Validation Failed",
				Status: http.StatusBadRequest,
				Detail: "Fields are invalid",
			},
			InvalidFields: []string{"email", "age"},
		}

		// Should be usable as error
		var _ error = err
		require.Equal(t, "Fields are invalid", err.Error())
	})

	t.Run("implements problemDetailMarker", func(t *testing.T) {
		err := ValidationError{
			ProblemDetail: ProblemDetail{
				Type:   "https://example.com/validation",
				Title:  "Validation Failed",
				Status: http.StatusBadRequest,
			},
			InvalidFields: []string{"email"},
		}

		// Should implement the private marker interface
		marker, ok := any(err).(problemDetailMarker)
		require.True(t, ok)
		require.Equal(t, http.StatusBadRequest, marker.statusCode())
	})

	t.Run("json marshaling includes extension fields", func(t *testing.T) {
		err := ValidationError{
			ProblemDetail: ProblemDetail{
				Type:   "https://example.com/validation",
				Title:  "Validation Failed",
				Status: http.StatusBadRequest,
				Detail: "Fields are invalid",
			},
			InvalidFields: []string{"email", "age"},
		}

		data, jsonErr := json.Marshal(err)
		require.NoError(t, jsonErr)

		var result map[string]any
		require.NoError(t, json.Unmarshal(data, &result))

		// Verify all fields are present
		require.Equal(t, "https://example.com/validation", result["type"])
		require.Equal(t, "Validation Failed", result["title"])
		require.Equal(t, float64(400), result["status"])
		require.Equal(t, "Fields are invalid", result["detail"])

		// Verify extension field
		invalidFields, ok := result["invalid_fields"].([]any)
		require.True(t, ok)
		require.Len(t, invalidFields, 2)
		require.Equal(t, "email", invalidFields[0])
		require.Equal(t, "age", invalidFields[1])
	})
}

// TestProblemDetailsErrorHandler_OnError_EmbeddedProblemDetail tests detection of embedded ProblemDetail
func TestProblemDetailsErrorHandler_OnError_EmbeddedProblemDetail(t *testing.T) {
	type NotFoundError struct {
		ProblemDetail
		ResourceID string `json:"resource_id"`
	}

	handler := NewProblemDetailsErrorHandler()

	err := NotFoundError{
		ProblemDetail: ProblemDetail{
			Type:     "https://example.com/not-found",
			Title:    "Not Found",
			Status:   http.StatusNotFound,
			Detail:   "Resource does not exist",
			Instance: "/users/123",
		},
		ResourceID: "123",
	}

	rec := httptest.NewRecorder()
	handler.OnError(context.Background(), rec, err)

	// Verify response
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))

	var result map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.Equal(t, "https://example.com/not-found", result["type"])
	require.Equal(t, "Not Found", result["title"])
	require.Equal(t, float64(404), result["status"])
	require.Equal(t, "Resource does not exist", result["detail"])
	require.Equal(t, "/users/123", result["instance"])
	require.Equal(t, "123", result["resource_id"])
}

// TestProblemDetailsErrorHandler_OnError_BadRequestError tests conversion of BadRequestError
func TestProblemDetailsErrorHandler_OnError_BadRequestError(t *testing.T) {
	handler := NewProblemDetailsErrorHandler()

	t.Run("generic bad request", func(t *testing.T) {
		err := BadRequestError{
			Cause: errors.New("something is wrong"),
		}

		rec := httptest.NewRecorder()
		handler.OnError(context.Background(), rec, err)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))

		var result ProblemDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.Equal(t, "about:blank", result.Type)
		require.Equal(t, "Bad Request", result.Title)
		require.Equal(t, http.StatusBadRequest, result.Status)
	})

	t.Run("missing required parameter", func(t *testing.T) {
		err := BadRequestError{
			Cause: MissingRequiredParameterError{
				Parameter: "user_id",
				In:        "query",
			},
		}

		rec := httptest.NewRecorder()
		handler.OnError(context.Background(), rec, err)

		var result ProblemDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.Equal(t, "about:blank", result.Type)
		require.Equal(t, "Missing Required Parameter", result.Title)
		require.Equal(t, http.StatusBadRequest, result.Status)
	})

	t.Run("invalid parameter value", func(t *testing.T) {
		err := BadRequestError{
			Cause: InvalidParameterValueError{
				Parameter: "page",
				In:        "query",
			},
		}

		rec := httptest.NewRecorder()
		handler.OnError(context.Background(), rec, err)

		var result ProblemDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.Equal(t, "Invalid Parameter Value", result.Title)
	})
}

// TestProblemDetailsErrorHandler_OnError_UnauthorizedError tests conversion of UnauthorizedError
func TestProblemDetailsErrorHandler_OnError_UnauthorizedError(t *testing.T) {
	handler := NewProblemDetailsErrorHandler()

	t.Run("generic unauthorized", func(t *testing.T) {
		err := UnauthorizedError{
			Cause: errors.New("invalid credentials"),
		}

		rec := httptest.NewRecorder()
		handler.OnError(context.Background(), rec, err)

		require.Equal(t, http.StatusUnauthorized, rec.Code)

		var result ProblemDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.Equal(t, "Unauthorized", result.Title)
		require.Equal(t, http.StatusUnauthorized, result.Status)
	})

	t.Run("invalid jwt", func(t *testing.T) {
		err := UnauthorizedError{
			Cause: InvalidJWTError{
				Parameter: "Authorization",
				In:        "header",
				Cause:     errors.New("signature invalid"),
			},
		}

		rec := httptest.NewRecorder()
		handler.OnError(context.Background(), rec, err)

		var result ProblemDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.Equal(t, "Invalid JWT Token", result.Title)
	})
}

// TestProblemDetailsErrorHandler_OnError_GenericError tests conversion of generic errors
func TestProblemDetailsErrorHandler_OnError_GenericError(t *testing.T) {
	handler := NewProblemDetailsErrorHandler()

	err := errors.New("database connection failed")

	rec := httptest.NewRecorder()
	handler.OnError(context.Background(), rec, err)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))

	var result ProblemDetail
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.Equal(t, "about:blank", result.Type)
	require.Equal(t, "Internal Server Error", result.Title)
	require.Equal(t, http.StatusInternalServerError, result.Status)
	require.Equal(t, "An internal server error occurred.", result.Detail)
}

// TestProblemDetailsConfig tests configuration options
func TestProblemDetailsConfig(t *testing.T) {
	t.Run("default type is about:blank", func(t *testing.T) {
		handler := NewProblemDetailsErrorHandler()

		err := errors.New("test error")
		rec := httptest.NewRecorder()
		handler.OnError(context.Background(), rec, err)

		var result ProblemDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.Equal(t, "about:blank", result.Type)
	})

	t.Run("custom default type", func(t *testing.T) {
		handler := NewProblemDetailsErrorHandler(
			WithDefaultType("https://api.example.com/problems/"),
		)

		err := errors.New("test error")
		rec := httptest.NewRecorder()
		handler.OnError(context.Background(), rec, err)

		var result ProblemDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.Equal(t, "https://api.example.com/problems/", result.Type)
	})

	t.Run("custom default type with bad request", func(t *testing.T) {
		handler := NewProblemDetailsErrorHandler(
			WithDefaultType("https://api.example.com/problems/"),
		)

		err := BadRequestError{Cause: errors.New("bad")}
		rec := httptest.NewRecorder()
		handler.OnError(context.Background(), rec, err)

		var result ProblemDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.Equal(t, "https://api.example.com/problems/bad-request", result.Type)
	})

	t.Run("generic errors use hardcoded detail message", func(t *testing.T) {
		handler := NewProblemDetailsErrorHandler()

		err := errors.New("sensitive internal error message")
		rec := httptest.NewRecorder()
		handler.OnError(context.Background(), rec, err)

		var result ProblemDetail
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		require.Equal(t, "An internal server error occurred.", result.Detail)
	})
}
