// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"fmt"
	"net/http"

	"github.com/z5labs/humus/rest"
)

// Error type URIs
const (
	ErrTypeValidation = "https://api.example.com/errors/validation"
	ErrTypeNotFound   = "https://api.example.com/errors/not-found"
	ErrTypeConflict   = "https://api.example.com/errors/conflict"
)

// ValidationError represents validation failures with field-specific errors
type ValidationError struct {
	rest.ProblemDetail
	Errors map[string][]string `json:"errors"`
}

func (e ValidationError) Error() string {
	return e.Detail
}

func newValidationError(errors map[string][]string) ValidationError {
	return ValidationError{
		ProblemDetail: rest.ProblemDetail{
			Type:   ErrTypeValidation,
			Title:  "Validation Failed",
			Status: http.StatusBadRequest,
			Detail: "One or more validation errors occurred",
		},
		Errors: errors,
	}
}

// NotFoundError represents resource not found errors
type NotFoundError struct {
	rest.ProblemDetail
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
}

func (e NotFoundError) Error() string {
	return e.Detail
}

func newNotFoundError(resourceType, resourceID string) NotFoundError {
	return NotFoundError{
		ProblemDetail: rest.ProblemDetail{
			Type:   ErrTypeNotFound,
			Title:  "Resource Not Found",
			Status: http.StatusNotFound,
			Detail: fmt.Sprintf("%s with ID %s not found", resourceType, resourceID),
		},
		ResourceType: resourceType,
		ResourceID:   resourceID,
	}
}

// ConflictError represents resource conflict errors
type ConflictError struct {
	rest.ProblemDetail
	ConflictingField string `json:"conflicting_field"`
	ExistingValue    string `json:"existing_value"`
}

func (e ConflictError) Error() string {
	return e.Detail
}

func newConflictError(field, value string) ConflictError {
	return ConflictError{
		ProblemDetail: rest.ProblemDetail{
			Type:   ErrTypeConflict,
			Title:  "Resource Conflict",
			Status: http.StatusConflict,
			Detail: fmt.Sprintf("A resource with %s=%s already exists", field, value),
		},
		ConflictingField: field,
		ExistingValue:    value,
	}
}
