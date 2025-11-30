// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest_test

import (
	"context"
	"errors"
	"net/http"

	"github.com/z5labs/humus/rest"
)

type SimpleRequest struct {
	Name string `json:"name"`
}

type SimpleResponse struct {
	Message string `json:"message"`
}

// ExampleNewProblemDetailsErrorHandler demonstrates basic usage of Problem Details error handling
func ExampleNewProblemDetailsErrorHandler() {
	handler := rest.HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
		return nil, errors.New("something went wrong")
	})

	api := rest.NewApi(
		"Example API",
		"v1.0.0",
		rest.Operation(
			http.MethodPost,
			rest.BasePath("/example"),
			rest.HandleJson(handler),
			rest.OnError(rest.NewProblemDetailsErrorHandler(rest.ProblemDetailsConfig{})),
		),
	)

	// Error responses will now be RFC 7807 compliant
	_ = api
}

// ExampleProblemDetail demonstrates creating a custom error with extension fields
func ExampleProblemDetail() {
	// Define custom error with extension fields
	type ValidationError struct {
		rest.ProblemDetail
		InvalidFields []string `json:"invalid_fields"`
	}

	handler := rest.HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
		return nil, ValidationError{
			ProblemDetail: rest.ProblemDetail{
				Type:   "https://api.example.com/problems/validation",
				Title:  "Validation Failed",
				Status: http.StatusBadRequest,
				Detail: "One or more fields failed validation",
			},
			InvalidFields: []string{"email", "age"},
		}
	})

	_ = handler
}

// ExampleProblemDetailsConfig_productionMode demonstrates production configuration
func ExampleProblemDetailsConfig_productionMode() {
	// Production configuration - don't leak error details
	includeDetails := false

	handler := rest.HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
		return nil, errors.New("internal database connection failed")
	})

	api := rest.NewApi(
		"Production API",
		"v1.0.0",
		rest.Operation(
			http.MethodPost,
			rest.BasePath("/example"),
			rest.HandleJson(handler),
			rest.OnError(rest.NewProblemDetailsErrorHandler(rest.ProblemDetailsConfig{
				DefaultType:    "https://api.example.com/problems/",
				IncludeDetails: &includeDetails, // Security: hide internal errors
			})),
		),
	)

	_ = api
}

// ExampleProblemDetail_complexExtensions demonstrates complex extension fields
func ExampleProblemDetail_complexExtensions() {
	type FieldError struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	}

	type ValidationError struct {
		rest.ProblemDetail
		InvalidFields []FieldError `json:"invalid_fields"`
		ErrorCount    int          `json:"error_count"`
	}

	handler := rest.HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
		invalidFields := []FieldError{
			{Field: "email", Message: "invalid email format"},
			{Field: "age", Message: "must be 18 or older"},
		}

		return nil, ValidationError{
			ProblemDetail: rest.ProblemDetail{
				Type:   "https://api.example.com/problems/validation",
				Title:  "Validation Failed",
				Status: http.StatusBadRequest,
				Detail: "Multiple fields failed validation",
			},
			InvalidFields: invalidFields,
			ErrorCount:    len(invalidFields),
		}
	})

	_ = handler

	// Response will include all extension fields:
	// {
	//   "type": "https://api.example.com/problems/validation",
	//   "title": "Validation Failed",
	//   "status": 400,
	//   "detail": "Multiple fields failed validation",
	//   "invalid_fields": [
	//     {"field": "email", "message": "invalid email format"},
	//     {"field": "age", "message": "must be 18 or older"}
	//   ],
	//   "error_count": 2
	// }
}

// ExampleProblemDetail_notFoundWithContext demonstrates a not found error with context
func ExampleProblemDetail_notFoundWithContext() {
	type NotFoundError struct {
		rest.ProblemDetail
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}

	handler := rest.HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
		return nil, NotFoundError{
			ProblemDetail: rest.ProblemDetail{
				Type:     "https://api.example.com/problems/not-found",
				Title:    "Resource Not Found",
				Status:   http.StatusNotFound,
				Detail:   "The requested user does not exist",
				Instance: "/users/12345",
			},
			ResourceType: "user",
			ResourceID:   "12345",
		}
	})

	_ = handler

	// Response:
	// {
	//   "type": "https://api.example.com/problems/not-found",
	//   "title": "Resource Not Found",
	//   "status": 404,
	//   "detail": "The requested user does not exist",
	//   "instance": "/users/12345",
	//   "resource_type": "user",
	//   "resource_id": "12345"
	// }
}

// ExampleProblemDetail_overrideErrorMethod demonstrates overriding the Error() method
func ExampleProblemDetail_overrideErrorMethod() {
	type ValidationError struct {
		rest.ProblemDetail
		InvalidFields []string `json:"invalid_fields"`
	}

	handler := rest.HandlerFunc[SimpleRequest, SimpleResponse](func(ctx context.Context, req *SimpleRequest) (*SimpleResponse, error) {
		// ValidationError can override Error() method if desired
		// The ProblemDetail.Error() method returns Detail if present, otherwise Title
		return nil, ValidationError{
			ProblemDetail: rest.ProblemDetail{
				Type:   "https://api.example.com/problems/validation",
				Title:  "Validation Failed",
				Status: http.StatusBadRequest,
				Detail: "Email and password fields are invalid",
			},
			InvalidFields: []string{"email", "password"},
		}
	})

	_ = handler
}
