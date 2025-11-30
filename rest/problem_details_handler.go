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

// ProblemDetailsErrorHandler is an [ErrorHandler] that returns RFC 7807 Problem Details responses.
//
// It provides three-tier error detection:
//  1. Errors embedding [ProblemDetail] (detected via private interface) - marshaled directly with all extension fields
//  2. Errors implementing [HttpResponseWriter] (existing framework errors) - converted to standard Problem Details
//  3. Generic errors - converted to 500 Internal Server Error Problem Details
//
// All errors are logged before returning the response.
//
// Example:
//
//	handler := rest.NewProblemDetailsErrorHandler(rest.ProblemDetailsConfig{
//	    DefaultType: "https://api.example.com/problems/",
//	    IncludeDetails: false, // Production mode
//	})
//
//	rest.Handle(
//	    http.MethodPost,
//	    rest.BasePath("/users"),
//	    userHandler,
//	    rest.OnError(handler),
//	)
type ProblemDetailsErrorHandler struct {
	config ProblemDetailsConfig
	log    *slog.Logger
}

// ProblemDetailsConfig configures the Problem Details error handler.
type ProblemDetailsConfig struct {
	// DefaultType is the default type URI for errors that don't specify one.
	// Defaults to "about:blank" per RFC 7807.
	//
	// For custom problem types, provide a base URI like:
	// "https://api.example.com/problems/"
	DefaultType string

	// IncludeDetails controls whether error details are included in responses.
	// When true, the Detail field will contain the error's Error() message.
	// When false, the Detail field will be empty.
	//
	// Set to false in production to avoid leaking internal error messages.
	// Defaults to true if nil.
	IncludeDetails *bool

	// Logger is used to log errors before returning Problem Details.
	// If nil, uses humus.Logger("rest").
	Logger *slog.Logger
}

// NewProblemDetailsErrorHandler creates a new Problem Details error handler.
//
// Example:
//
//	handler := rest.NewProblemDetailsErrorHandler(rest.ProblemDetailsConfig{
//	    DefaultType: "https://api.example.com/problems/",
//	    IncludeDetails: false,
//	})
func NewProblemDetailsErrorHandler(config ProblemDetailsConfig) *ProblemDetailsErrorHandler {
	// Apply defaults
	if config.DefaultType == "" {
		config.DefaultType = "about:blank"
	}
	if config.Logger == nil {
		config.Logger = humus.Logger("rest")
	}
	if config.IncludeDetails == nil {
		// Default to true for development friendliness
		trueVal := true
		config.IncludeDetails = &trueVal
	}

	return &ProblemDetailsErrorHandler{
		config: config,
		log:    config.Logger,
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

// getDetailMessage returns the error detail message based on configuration.
// Returns empty string if IncludeDetails is false (for production security).
func (h *ProblemDetailsErrorHandler) getDetailMessage(err error) string {
	if h.config.IncludeDetails != nil && !(*h.config.IncludeDetails) {
		return ""
	}
	return err.Error()
}
