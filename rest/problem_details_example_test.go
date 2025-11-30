// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/z5labs/humus/rest"
)

// ExampleNewProblemDetailsErrorHandler demonstrates basic usage of Problem Details error handling
func ExampleNewProblemDetailsErrorHandler() {
	handler := rest.HandlerFunc[rest.EmptyRequest, rest.EmptyResponse](func(ctx context.Context, req *rest.EmptyRequest) (*rest.EmptyResponse, error) {
		return nil, errors.New("something went wrong")
	})

	api := rest.NewApi(
		"Example API",
		"v1.0.0",
		rest.Operation(
			http.MethodPost,
			rest.BasePath("/example"),
			rest.HandleJson(handler),
			rest.OnError(rest.NewProblemDetailsErrorHandler()),
		),
	)

	// Create test server
	server := httptest.NewServer(api)
	defer server.Close()

	// Send request that will trigger an error
	resp, err := http.Post(server.URL+"/example", "application/json", strings.NewReader(`{}`))
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println("Status:", resp.Status)
	fmt.Println("Response:", string(body))
	// Output:
	// Status: 500 Internal Server Error
	// Response: {"type":"about:blank","title":"Internal Server Error","status":500,"detail":"An internal server error occurred."}
}

// ExampleProblemDetail demonstrates creating a custom error with extension fields
func ExampleProblemDetail() {
	// Define custom error with extension fields
	type ValidationError struct {
		rest.ProblemDetail
		InvalidFields []string `json:"invalid_fields"`
	}

	handler := rest.HandlerFunc[rest.EmptyRequest, rest.EmptyResponse](func(ctx context.Context, req *rest.EmptyRequest) (*rest.EmptyResponse, error) {
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

	api := rest.NewApi(
		"Example API",
		"v1.0.0",
		rest.Operation(
			http.MethodPost,
			rest.BasePath("/validate"),
			rest.HandleJson(handler),
			rest.OnError(rest.NewProblemDetailsErrorHandler()),
		),
	)

	// Create test server
	server := httptest.NewServer(api)
	defer server.Close()

	// Send request that will trigger validation error
	resp, err := http.Post(server.URL+"/validate", "application/json", strings.NewReader(`{}`))
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println("Status:", resp.Status)
	fmt.Println("Response:", string(body))
	// Output:
	// Status: 400 Bad Request
	// Response: {"type":"https://api.example.com/problems/validation","title":"Validation Failed","status":400,"detail":"One or more fields failed validation","invalid_fields":["email","age"]}
}

// ExampleNewProblemDetailsErrorHandler_secureByDefault demonstrates that generic errors are secure by default
func ExampleNewProblemDetailsErrorHandler_secureByDefault() {
	// Generic errors are automatically secured with hardcoded detail messages
	handler := rest.HandlerFunc[rest.EmptyRequest, rest.EmptyResponse](func(ctx context.Context, req *rest.EmptyRequest) (*rest.EmptyResponse, error) {
		return nil, errors.New("internal database connection failed with password: secret123")
	})

	api := rest.NewApi(
		"Production API",
		"v1.0.0",
		rest.Operation(
			http.MethodPost,
			rest.BasePath("/example"),
			rest.HandleJson(handler),
			rest.OnError(rest.NewProblemDetailsErrorHandler(
				rest.WithDefaultType("https://api.example.com/problems/"),
			)),
		),
	)

	// Create test server
	server := httptest.NewServer(api)
	defer server.Close()

	// Send request that will trigger a generic error
	resp, err := http.Post(server.URL+"/example", "application/json", strings.NewReader(`{}`))
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println("Status:", resp.Status)
	fmt.Println("Response:", string(body))
	// Output:
	// Status: 500 Internal Server Error
	// Response: {"type":"https://api.example.com/problems/","title":"Internal Server Error","status":500,"detail":"An internal server error occurred."}
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

	handler := rest.HandlerFunc[rest.EmptyRequest, rest.EmptyResponse](func(ctx context.Context, req *rest.EmptyRequest) (*rest.EmptyResponse, error) {
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

	api := rest.NewApi(
		"Example API",
		"v1.0.0",
		rest.Operation(
			http.MethodPost,
			rest.BasePath("/users"),
			rest.HandleJson(handler),
			rest.OnError(rest.NewProblemDetailsErrorHandler()),
		),
	)

	// Create test server
	server := httptest.NewServer(api)
	defer server.Close()

	// Send request that will trigger validation error
	resp, err := http.Post(server.URL+"/users", "application/json", strings.NewReader(`{}`))
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println("Status:", resp.Status)
	fmt.Println("Response:", string(body))
	// Output:
	// Status: 400 Bad Request
	// Response: {"type":"https://api.example.com/problems/validation","title":"Validation Failed","status":400,"detail":"Multiple fields failed validation","invalid_fields":[{"field":"email","message":"invalid email format"},{"field":"age","message":"must be 18 or older"}],"error_count":2}
}

// ExampleProblemDetail_notFoundWithContext demonstrates a not found error with context
func ExampleProblemDetail_notFoundWithContext() {
	type NotFoundError struct {
		rest.ProblemDetail
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}

	handler := rest.ProducerFunc[rest.EmptyResponse](func(ctx context.Context) (*rest.EmptyResponse, error) {
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

	api := rest.NewApi(
		"Example API",
		"v1.0.0",
		rest.Operation(
			http.MethodGet,
			rest.BasePath("/users").Param("id"),
			rest.ProduceJson(handler),
			rest.OnError(rest.NewProblemDetailsErrorHandler()),
		),
	)

	// Create test server
	server := httptest.NewServer(api)
	defer server.Close()

	// Send request that will trigger not found error
	resp, err := http.Get(server.URL + "/users/12345")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println("Status:", resp.Status)
	fmt.Println("Response:", string(body))
	// Output:
	// Status: 404 Not Found
	// Response: {"type":"https://api.example.com/problems/not-found","title":"Resource Not Found","status":404,"detail":"The requested user does not exist","instance":"/users/12345","resource_type":"user","resource_id":"12345"}
}

// ExampleProblemDetail_overrideErrorMethod demonstrates overriding the Error() method
func ExampleProblemDetail_overrideErrorMethod() {
	type ValidationError struct {
		rest.ProblemDetail
		InvalidFields []string `json:"invalid_fields"`
	}

	handler := rest.HandlerFunc[rest.EmptyRequest, rest.EmptyResponse](func(ctx context.Context, req *rest.EmptyRequest) (*rest.EmptyResponse, error) {
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

	api := rest.NewApi(
		"Example API",
		"v1.0.0",
		rest.Operation(
			http.MethodPost,
			rest.BasePath("/login"),
			rest.HandleJson(handler),
			rest.OnError(rest.NewProblemDetailsErrorHandler()),
		),
	)

	// Create test server
	server := httptest.NewServer(api)
	defer server.Close()

	// Send request that will trigger validation error
	resp, err := http.Post(server.URL+"/login", "application/json", strings.NewReader(`{}`))
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println("Status:", resp.Status)
	fmt.Println("Response:", string(body))
	// Output:
	// Status: 400 Bad Request
	// Response: {"type":"https://api.example.com/problems/validation","title":"Validation Failed","status":400,"detail":"Email and password fields are invalid","invalid_fields":["email","password"]}
}
