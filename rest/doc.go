// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package rest provides a framework for building OpenAPI-compliant RESTful HTTP applications.
//
// # Overview
//
// The rest package simplifies building REST APIs by providing:
//   - Automatic OpenAPI 3.0 schema generation
//   - Type-safe request/response handling
//   - Built-in parameter validation (headers, query params, cookies, path params)
//   - Standardized error handling
//   - Health check endpoints (liveness/readiness)
//   - OpenTelemetry instrumentation
//   - Authentication schemes (JWT, API Key, OAuth2, etc.)
//
// # Quick Start
//
// Creating a basic API:
//
//	api := rest.NewApi("My API", "v1.0.0")
//	http.ListenAndServe(":8080", api)
//
// The API automatically provides:
//   - OpenAPI schema at GET /openapi.json
//   - Health endpoints at GET /health/liveness and GET /health/readiness
//
// # Adding Operations
//
// Use the Handle function to register HTTP operations:
//
//	getBook := rest.Handle(
//	    http.MethodGet,
//	    rest.BasePath("/books").Param("id"),
//	    myHandler,
//	    rest.QueryParam("format", rest.Required()),
//	)
//	api := rest.NewApi("Bookstore", "v1.0.0", getBook)
//
// # Parameter Validation
//
// The package supports various parameter types with validation:
//
//	rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt"))
//	rest.QueryParam("page", rest.Required(), rest.Regex(regexp.MustCompile(`^\d+$`)))
//	rest.Cookie("session", rest.Required())
//
// # Path Building
//
// Build type-safe paths with segments and parameters:
//
//	path := rest.BasePath("/api/v1").Segment("users").Param("userId")
//	// Results in: /api/v1/users/{userId}
//
// # Error Handling
//
// Customize error handling for operations:
//
//	handler := rest.Handle(
//	    http.MethodPost,
//	    rest.BasePath("/users"),
//	    myHandler,
//	    rest.OnError(customErrorHandler),
//	)
//
// Errors implementing HttpResponseWriter can control the HTTP response:
//
//	type CustomError struct{}
//	func (e CustomError) Error() string { return "custom error" }
//	func (e CustomError) WriteHttpResponse(ctx context.Context, w http.ResponseWriter) {
//	    w.WriteHeader(http.StatusBadRequest)
//	    w.Write([]byte(`{"error": "custom error"}`))
//	}
//
// # Configuration and Running
//
// Use the Builder and Run functions for production deployments:
//
//	rest.Run(configReader, func(ctx context.Context, cfg rest.Config) (*rest.Api, error) {
//	    return rest.NewApi("My API", "v1.0.0", operations...), nil
//	})
//
// This provides automatic configuration loading, graceful shutdown,
// OpenTelemetry setup, and signal handling.
package rest
