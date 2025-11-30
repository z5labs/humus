// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

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

// problemDetailMarker is a private interface that only ProblemDetail implements.
// This allows the ProblemDetailsErrorHandler to detect errors that embed ProblemDetail
// without using reflection. Types that embed ProblemDetail will automatically
// implement this interface via method promotion.
type problemDetailMarker interface {
	statusCode() int
}

// statusCode implements the private problemDetailMarker interface.
// This method is intentionally unexported to prevent external implementation.
func (p ProblemDetail) statusCode() int {
	return p.Status
}
