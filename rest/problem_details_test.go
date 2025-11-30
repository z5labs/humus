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

func TestProblemDetailsErrorHandler(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		handler        HandlerFunc[EmptyRequest, EmptyResponse]
		errorHandler   ErrorHandler
		assertResponse func(*testing.T, *http.Response)
	}{
		{
			name: "custom error embedding ProblemDetail with extension fields",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				type ValidationError struct {
					ProblemDetail
					InvalidFields []string `json:"invalid_fields"`
				}
				return nil, ValidationError{
					ProblemDetail: ProblemDetail{
						Type:     "https://api.example.com/problems/validation",
						Title:    "Validation Failed",
						Status:   http.StatusBadRequest,
						Detail:   "Request validation failed",
						Instance: "/test",
					},
					InvalidFields: []string{"email", "age"},
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusBadRequest, resp.StatusCode)
				require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

				var result map[string]any
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "https://api.example.com/problems/validation", result["type"])
				require.Equal(t, "Validation Failed", result["title"])
				require.Equal(t, float64(400), result["status"])
				require.Equal(t, "Request validation failed", result["detail"])
				require.Equal(t, "/test", result["instance"])
				require.Contains(t, result, "invalid_fields")
			},
		},
		{
			name: "custom error embedding ProblemDetail without detail field",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, ProblemDetail{
					Type:   "https://api.example.com/problems/not-found",
					Title:  "Resource Not Found",
					Status: http.StatusNotFound,
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusNotFound, resp.StatusCode)
				require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

				var result map[string]any
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "https://api.example.com/problems/not-found", result["type"])
				require.Equal(t, "Resource Not Found", result["title"])
				require.Equal(t, float64(404), result["status"])
				require.NotContains(t, result, "detail")
			},
		},
		{
			name: "BadRequestError with MissingRequiredParameterError cause",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, BadRequestError{
					Cause: MissingRequiredParameterError{
						Parameter: "user_id",
						In:        "query",
					},
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusBadRequest, resp.StatusCode)
				require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "about:blank", result.Type)
				require.Equal(t, "Missing Required Parameter", result.Title)
				require.Equal(t, http.StatusBadRequest, result.Status)
				require.Equal(t, "An internal server error occurred.", result.Detail)
			},
		},
		{
			name: "BadRequestError with InvalidParameterValueError cause",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, BadRequestError{
					Cause: InvalidParameterValueError{
						Parameter: "page",
						In:        "query",
					},
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusBadRequest, resp.StatusCode)

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "about:blank", result.Type)
				require.Equal(t, "Invalid Parameter Value", result.Title)
				require.Equal(t, http.StatusBadRequest, result.Status)
				require.Equal(t, "An internal server error occurred.", result.Detail)
			},
		},
		{
			name: "BadRequestError with InvalidContentTypeError cause",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, BadRequestError{
					Cause: InvalidContentTypeError{
						ContentType: "text/plain",
					},
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusBadRequest, resp.StatusCode)

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "about:blank", result.Type)
				require.Equal(t, "Invalid Content Type", result.Title)
				require.Equal(t, http.StatusBadRequest, result.Status)
			},
		},
		{
			name: "BadRequestError with InvalidJWTError cause",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, BadRequestError{
					Cause: InvalidJWTError{
						Parameter: "Authorization",
						In:        "header",
					},
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusBadRequest, resp.StatusCode)

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "about:blank", result.Type)
				require.Equal(t, "Invalid JWT Format", result.Title)
				require.Equal(t, http.StatusBadRequest, result.Status)
			},
		},
		{
			name: "generic BadRequestError without specific cause",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, BadRequestError{
					Cause: errors.New("some validation error"),
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusBadRequest, resp.StatusCode)

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "about:blank", result.Type)
				require.Equal(t, "Bad Request", result.Title)
				require.Equal(t, http.StatusBadRequest, result.Status)
			},
		},
		{
			name: "UnauthorizedError with InvalidJWTError cause",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, UnauthorizedError{
					Cause: InvalidJWTError{
						Parameter: "Authorization",
						In:        "header",
					},
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "about:blank", result.Type)
				require.Equal(t, "Invalid JWT Token", result.Title)
				require.Equal(t, http.StatusUnauthorized, result.Status)
			},
		},
		{
			name: "generic UnauthorizedError without specific cause",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, UnauthorizedError{
					Cause: errors.New("token verification failed"),
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "about:blank", result.Type)
				require.Equal(t, "Unauthorized", result.Title)
				require.Equal(t, http.StatusUnauthorized, result.Status)
			},
		},
		{
			name: "generic error converted to 500 Internal Server Error",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, errors.New("database connection failed")
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
				require.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "about:blank", result.Type)
				require.Equal(t, "Internal Server Error", result.Title)
				require.Equal(t, http.StatusInternalServerError, result.Status)
				require.Equal(t, "An internal server error occurred.", result.Detail)
			},
		},
		{
			name: "security: generic error does not leak sensitive information",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, errors.New("database password: secret123")
			}),
			errorHandler: NewProblemDetailsErrorHandler(),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				// Verify hardcoded message is used, not the actual error message
				require.Equal(t, "An internal server error occurred.", result.Detail)
				require.NotContains(t, result.Detail, "secret123")
			},
		},
		{
			name: "WithDefaultType option with custom URI",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, BadRequestError{
					Cause: MissingRequiredParameterError{
						Parameter: "id",
						In:        "query",
					},
				}
			}),
			errorHandler: NewProblemDetailsErrorHandler(
				WithDefaultType("https://api.example.com/problems/"),
			),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusBadRequest, resp.StatusCode)

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "https://api.example.com/problems/missing-required-parameter", result.Type)
			},
		},
		{
			name: "WithDefaultType affects generic errors",
			handler: HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return nil, errors.New("generic error")
			}),
			errorHandler: NewProblemDetailsErrorHandler(
				WithDefaultType("https://api.example.com/problems/"),
			),
			assertResponse: func(t *testing.T, resp *http.Response) {
				require.Equal(t, http.StatusInternalServerError, resp.StatusCode)

				var result ProblemDetail
				require.Nil(t, json.NewDecoder(resp.Body).Decode(&result))

				require.Equal(t, "https://api.example.com/problems/", result.Type)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := NewApi(
				tc.name,
				"v0.0.0",
				Operation(
					http.MethodGet,
					BasePath("/"),
					tc.handler,
					OnError(tc.errorHandler),
				),
			)

			srv := httptest.NewServer(api)
			t.Cleanup(srv.Close)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			require.Nil(t, err)

			resp, err := srv.Client().Do(req)
			require.Nil(t, err)

			defer func() {
				require.Nil(t, resp.Body.Close())
			}()

			tc.assertResponse(t, resp)
		})
	}
}
