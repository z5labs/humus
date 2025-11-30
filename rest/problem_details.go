// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/z5labs/humus"
)

// ProblemDetail represents an RFC 7807 Problem Details error response.
//
// RFC 7807 defines a standard format for HTTP API error responses.
// Embed this struct in your custom error types to add extension fields.
//
// Example:
//
//	type ValidationError struct {
//	    rest.ProblemDetail
//	    InvalidFields []string `json:"invalid_fields"`
//	}
//
//	return nil, ValidationError{
//	    ProblemDetail: rest.ProblemDetail{
//	        Type:   "https://api.example.com/problems/validation",
//	        Title:  "Validation Failed",
//	        Status: http.StatusBadRequest,
//	        Detail: "One or more fields failed validation",
//	    },
//	    InvalidFields: []string{"email", "age"},
//	}
//
// Reference: https://www.rfc-editor.org/rfc/rfc7807
type ProblemDetail struct {
	// Type is a URI reference that identifies the problem type.
	// When dereferenced, it should provide human-readable documentation.
	// Defaults to "about:blank" when the problem has no specific type.
	Type string `json:"type"`

	// Title is a short, human-readable summary of the problem type.
	// It SHOULD NOT change from occurrence to occurrence of the problem,
	// except for purposes of localization.
	Title string `json:"title"`

	// Status is the HTTP status code for this occurrence of the problem.
	Status int `json:"status"`

	// Detail is a human-readable explanation specific to this occurrence
	// of the problem. Unlike Title, Detail can vary for different occurrences.
	Detail string `json:"detail,omitempty"`

	// Instance is a URI reference that identifies the specific occurrence
	// of the problem. It may or may not yield further information if dereferenced.
	Instance string `json:"instance,omitempty"`
}

// Error implements the error interface.
// Returns the Detail field if present, otherwise returns the Title.
// This allows ProblemDetail to be used directly as a Go error.
func (p ProblemDetail) Error() string {
	if p.Detail != "" {
		return p.Detail
	}
	return p.Title
}

type problemDetailMarker interface {
	statusCode() int
}

func (p ProblemDetail) statusCode() int {
	return p.Status
}

// ProblemDetailsErrorHandler is an [ErrorHandler] that returns RFC 7807 Problem Details responses.
//
// It provides three-tier error detection:
//  1. Errors embedding [ProblemDetail] (detected via private interface) - marshaled directly with all extension fields
//  2. Errors implementing [HttpResponseWriter] (existing framework errors) - converted to standard Problem Details with hardcoded detail message
//  3. Generic errors - converted to 500 Internal Server Error with hardcoded detail message
//
// For security, only errors embedding [ProblemDetail] include the actual error details.
// All other errors use the hardcoded message "An internal server error occurred."
// to prevent leaking sensitive internal error information to API clients.
//
// All errors are logged before returning the response.
//
// Example:
//
//	handler := rest.NewProblemDetailsErrorHandler(
//	    rest.WithDefaultType("https://api.example.com/problems/"),
//	)
//
//	rest.Handle(
//	    http.MethodPost,
//	    rest.BasePath("/users"),
//	    userHandler,
//	    rest.OnError(handler),
//	)
type ProblemDetailsErrorHandler struct {
	config problemDetailsConfig
	log    *slog.Logger
}

// problemDetailsConfig configures the Problem Details error handler.
type problemDetailsConfig struct {
	// DefaultType is the default type URI for errors that don't specify one.
	// Defaults to "about:blank" per RFC 7807.
	DefaultType string
}

// ProblemDetailsOption configures a ProblemDetailsErrorHandler.
type ProblemDetailsOption func(*problemDetailsConfig)

// WithDefaultType sets the default type URI for errors that don't specify one.
// Defaults to "about:blank" per RFC 7807.
//
// For custom problem types, provide a base URI like:
// "https://api.example.com/problems/"
func WithDefaultType(uri string) ProblemDetailsOption {
	return func(c *problemDetailsConfig) {
		c.DefaultType = uri
	}
}

// NewProblemDetailsErrorHandler creates a new Problem Details error handler.
//
// Error details are only included in responses for errors embedding [ProblemDetail].
// For all other errors (framework errors and generic errors), a hardcoded message
// "An internal server error occurred." is used to prevent leaking sensitive information.
//
// Example:
//
//	handler := rest.NewProblemDetailsErrorHandler(
//	    rest.WithDefaultType("https://api.example.com/problems/"),
//	)
func NewProblemDetailsErrorHandler(opts ...ProblemDetailsOption) *ProblemDetailsErrorHandler {
	// Apply defaults
	config := problemDetailsConfig{
		DefaultType: "about:blank",
	}

	// Apply options
	for _, opt := range opts {
		opt(&config)
	}

	return &ProblemDetailsErrorHandler{
		config: config,
		log:    humus.Logger("rest"),
	}
}

// OnError implements the [ErrorHandler] interface.
// It detects the error type and returns an appropriate RFC 7807 Problem Details response.
func (h *ProblemDetailsErrorHandler) OnError(ctx context.Context, w http.ResponseWriter, err error) {
	h.log.ErrorContext(ctx, "sending error response", slog.Any("error", err))

	// Strategy 1: Check if error embeds ProblemDetail (via private marker interface)
	if pd, ok := err.(problemDetailMarker); ok {
		// Error embeds ProblemDetail - marshal the full struct with all extension fields
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(pd.statusCode())
		if encodeErr := json.NewEncoder(w).Encode(err); encodeErr != nil {
			h.log.ErrorContext(ctx, "failed to encode problem details", slog.Any("error", encodeErr))
		}
		return
	}

	// Strategy 2: Check if error implements HttpResponseWriter (existing framework errors)
	if _, ok := err.(HttpResponseWriter); ok {
		problemDetail := h.convertHttpResponseWriterToProblemDetail(err)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(problemDetail.Status)
		if encodeErr := json.NewEncoder(w).Encode(problemDetail); encodeErr != nil {
			h.log.ErrorContext(ctx, "failed to encode problem details", slog.Any("error", encodeErr))
		}
		return
	}

	// Strategy 3: Generic error - convert to basic ProblemDetail
	problemDetail := h.convertGenericErrorToProblemDetail(err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(problemDetail.Status)
	if encodeErr := json.NewEncoder(w).Encode(problemDetail); encodeErr != nil {
		h.log.ErrorContext(ctx, "failed to encode problem details", slog.Any("error", encodeErr))
	}
}

// convertHttpResponseWriterToProblemDetail converts existing framework errors
// to Problem Details format by unwrapping and checking for specific error types.
func (h *ProblemDetailsErrorHandler) convertHttpResponseWriterToProblemDetail(err error) ProblemDetail {
	var status int
	var typeURI string
	var title string

	// Check for BadRequestError and its causes
	var badRequest BadRequestError
	if errors.As(err, &badRequest) {
		status = http.StatusBadRequest
		typeURI = h.buildTypeURI("bad-request")
		title = "Bad Request"

		// Check for more specific error types in the cause chain
		var missingParam MissingRequiredParameterError
		if errors.As(badRequest.Cause, &missingParam) {
			typeURI = h.buildTypeURI("missing-required-parameter")
			title = "Missing Required Parameter"
		}

		var invalidParam InvalidParameterValueError
		if errors.As(badRequest.Cause, &invalidParam) {
			typeURI = h.buildTypeURI("invalid-parameter-value")
			title = "Invalid Parameter Value"
		}

		var invalidContentType InvalidContentTypeError
		if errors.As(badRequest.Cause, &invalidContentType) {
			typeURI = h.buildTypeURI("invalid-content-type")
			title = "Invalid Content Type"
		}

		var invalidJWT InvalidJWTError
		if errors.As(badRequest.Cause, &invalidJWT) {
			typeURI = h.buildTypeURI("invalid-jwt-format")
			title = "Invalid JWT Format"
		}
	}

	// Check for UnauthorizedError and its causes
	var unauthorized UnauthorizedError
	if errors.As(err, &unauthorized) {
		status = http.StatusUnauthorized
		typeURI = h.buildTypeURI("unauthorized")
		title = "Unauthorized"

		var invalidJWT InvalidJWTError
		if errors.As(unauthorized.Cause, &invalidJWT) {
			typeURI = h.buildTypeURI("invalid-jwt")
			title = "Invalid JWT Token"
		}
	}

	// Fallback if not recognized
	if status == 0 {
		status = http.StatusInternalServerError
		typeURI = h.buildTypeURI("internal-error")
		title = "Internal Server Error"
	}

	return ProblemDetail{
		Type:   typeURI,
		Title:  title,
		Status: status,
		Detail: h.getDetailMessage(err),
	}
}

// convertGenericErrorToProblemDetail converts any error to a basic ProblemDetail
// with a 500 Internal Server Error status.
func (h *ProblemDetailsErrorHandler) convertGenericErrorToProblemDetail(err error) ProblemDetail {
	return ProblemDetail{
		Type:   h.config.DefaultType,
		Title:  "Internal Server Error",
		Status: http.StatusInternalServerError,
		Detail: h.getDetailMessage(err),
	}
}

// buildTypeURI constructs a type URI from a problem type identifier.
// If DefaultType is "about:blank", returns "about:blank" for all types.
// Otherwise, appends the problem type to the base URI.
func (h *ProblemDetailsErrorHandler) buildTypeURI(problemType string) string {
	if h.config.DefaultType == "about:blank" {
		return "about:blank"
	}
	// If custom base URI, append problem type
	return h.config.DefaultType + problemType
}

// getDetailMessage returns a hardcoded security message.
// This prevents leaking sensitive internal error information to API clients.
func (h *ProblemDetailsErrorHandler) getDetailMessage(err error) string {
	return "An internal server error occurred."
}
