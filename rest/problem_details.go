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
	config ProblemDetailsConfig
	log    *slog.Logger
}

// ProblemDetailsConfig configures the Problem Details error handler.
type ProblemDetailsConfig struct {
	// defaultType is the default type URI for errors that don't specify one.
	// Defaults to "about:blank" per RFC 7807.
	defaultType string
}

// ProblemDetailsOption configures a ProblemDetailsErrorHandler.
type ProblemDetailsOption func(*ProblemDetailsConfig)

// WithDefaultType sets the default type URI for errors that don't specify one.
// Defaults to "about:blank" per RFC 7807.
//
// For custom problem types, provide a base URI like:
// "https://api.example.com/problems/"
func WithDefaultType(uri string) ProblemDetailsOption {
	return func(c *ProblemDetailsConfig) {
		c.defaultType = uri
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
	config := ProblemDetailsConfig{
		defaultType: "about:blank",
	}

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

	w.Header().Set("Content-Type", "application/problem+json")

	pd := mapErrorToProblemDetail(h.config.defaultType, err)
	w.WriteHeader(pd.statusCode())

	enc := json.NewEncoder(w)
	err = enc.Encode(pd)
	if err != nil {
		h.log.ErrorContext(ctx, "failed to encode problem details", slog.Any("error", err))
	}
}

func mapErrorToProblemDetail(defaultType string, err error) problemDetailMarker {
	mappers := []func(error) (problemDetailMarker, bool){
		mapProblemDetailMarker,
		mapBadRequestError,
		mapUnauthorizedError,
	}

	for _, mapper := range mappers {
		if pd, ok := mapper(err); ok {
			return pd
		}
	}

	return ProblemDetail{
		Type:   defaultType,
		Title:  "Internal Server Error",
		Status: http.StatusInternalServerError,
		Detail: "An internal server error occurred.",
	}
}

func mapProblemDetailMarker(err error) (problemDetailMarker, bool) {
	pd, ok := err.(problemDetailMarker)
	return pd, ok
}

func mapBadRequestError(err error) (problemDetailMarker, bool) {
	var bre BadRequestError
	if !errors.As(err, &bre) {
		return nil, false
	}

	pd := ProblemDetail{
		Type:   "about:blank",
		Title:  "Bad Request",
		Status: http.StatusBadRequest,
		Detail: "A bad request was sent to the API",
	}
	return pd, true
}

func mapUnauthorizedError(err error) (problemDetailMarker, bool) {
	var ue UnauthorizedError
	if !errors.As(err, &ue) {
		return nil, false
	}

	pd := ProblemDetail{
		Type:   "about:blank",
		Title:  "Unauthorized",
		Status: http.StatusUnauthorized,
		Detail: "An unauthorized request was sent to the API",
	}
	return pd, true
}
