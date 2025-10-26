// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/sdk-go/ptr"
)

// Cookie creates a parameter validator for an HTTP cookie.
// The validator extracts and validates cookies from incoming requests.
//
// Example:
//
//	rest.Handle(
//	    http.MethodGet,
//	    rest.BasePath("/dashboard"),
//	    handler,
//	    rest.Cookie("session", rest.Required()),
//	)
func Cookie(name string, opts ...ParameterOption) OperationOption {
	return param(name, openapi3.ParameterInCookie, opts...)
}

func CookieValue(ctx context.Context, name string) []*http.Cookie {
	return ctx.Value(paramCtxKey(name)).([]*http.Cookie)
}

// Header creates a parameter validator for an HTTP header.
// The validator extracts and validates headers from incoming requests.
//
// Example:
//
//	rest.Handle(
//	    http.MethodGet,
//	    rest.BasePath("/api/data"),
//	    handler,
//	    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt")),
//	    rest.Header("X-Request-ID", rest.Required()),
//	)
func Header(name string, opts ...ParameterOption) OperationOption {
	return param(name, openapi3.ParameterInHeader, opts...)
}

func HeaderValue(ctx context.Context, name string) []string {
	return ctx.Value(paramCtxKey(name)).([]string)
}

// QueryParam creates a parameter validator for a URL query parameter.
// The validator extracts and validates query parameters from incoming requests.
//
// Example:
//
//	rest.Handle(
//	    http.MethodGet,
//	    rest.BasePath("/search"),
//	    handler,
//	    rest.QueryParam("q", rest.Required()),
//	    rest.QueryParam("page", rest.Regex(regexp.MustCompile(`^\d+$`))),
//	)
func QueryParam(name string, opts ...ParameterOption) OperationOption {
	return param(name, openapi3.ParameterInQuery, opts...)
}

func QueryParamValue(ctx context.Context, name string) []string {
	return ctx.Value(paramCtxKey(name)).([]string)
}

type paramCtxKey string

func injectParam(name string, in openapi3.ParameterIn) func(*http.Request) (*http.Request, error) {
	ctxKey := paramCtxKey(name)

	return transformParam(
		name,
		in,
		func(ctx context.Context, c []*http.Cookie) (context.Context, error) {
			return context.WithValue(ctx, ctxKey, c), nil
		},
		func(ctx context.Context, s []string) (context.Context, error) {
			return context.WithValue(ctx, ctxKey, s), nil
		},
		func(ctx context.Context, s string) (context.Context, error) {
			return context.WithValue(ctx, ctxKey, s), nil
		},
		func(ctx context.Context, s []string) (context.Context, error) {
			return context.WithValue(ctx, ctxKey, s), nil
		},
	)
}

func param(name string, in openapi3.ParameterIn, opts ...ParameterOption) OperationOption {
	return func(oo *OperationOptions) {
		oo.transforms = append(oo.transforms, injectParam(name, in))

		po := &ParameterOptions{
			operationOptions: oo,
			def: &openapi3.Parameter{
				Name: name,
				In:   in,
			},
		}
		for _, opt := range opts {
			opt(po)
		}

		oo.parameters = append(oo.parameters, openapi3.ParameterOrRef{
			Parameter: po.def,
		})
	}
}

type contextTransformer[T any] func(context.Context, T) (context.Context, error)

func transformRequestContext[T any](f func(*http.Request) T, ct contextTransformer[T]) func(*http.Request) (*http.Request, error) {
	return func(r *http.Request) (*http.Request, error) {
		ctx, err := ct(r.Context(), f(r))
		if err != nil {
			return nil, err
		}

		return r.WithContext(ctx), nil
	}
}

func extractCookie(name string) func(*http.Request) []*http.Cookie {
	return func(r *http.Request) []*http.Cookie {
		return r.CookiesNamed(name)
	}
}

func extractHeader(name string) func(*http.Request) []string {
	return func(r *http.Request) []string {
		return r.Header.Values(name)
	}
}

func extractPathParam(name string) func(*http.Request) string {
	return func(r *http.Request) string {
		return chi.URLParam(r, name)
	}
}

func extractQueryParam(name string) func(*http.Request) []string {
	return func(r *http.Request) []string {
		return r.URL.Query()[name]
	}
}

// extractBearerToken extracts a JWT token from Authorization header values.
// It expects the header value to be in the format "Bearer <token>".
// Returns an error if no header values exist or if the "Bearer " prefix is missing.
func extractBearerToken(headerValues []string) (string, error) {
	if len(headerValues) == 0 {
		return "", fmt.Errorf("missing Authorization header")
	}

	// Check the first value (standard practice is to send only one Authorization header)
	authHeader := headerValues[0]
	const bearerPrefix = "Bearer "

	if len(authHeader) < len(bearerPrefix) {
		return "", fmt.Errorf("malformed Authorization header: missing Bearer prefix")
	}

	// Case-sensitive check for "Bearer " prefix (RFC 6750 Section 2.1)
	if authHeader[:len(bearerPrefix)] != bearerPrefix {
		return "", fmt.Errorf("malformed Authorization header: expected Bearer scheme")
	}

	token := authHeader[len(bearerPrefix):]
	if len(token) == 0 {
		return "", fmt.Errorf("malformed Authorization header: empty token")
	}

	return token, nil
}

func transformParam(
	name string,
	in openapi3.ParameterIn,
	transformCookie contextTransformer[[]*http.Cookie],
	transformHeader contextTransformer[[]string],
	transformPathParam contextTransformer[string],
	transformQueryParam contextTransformer[[]string],
) func(*http.Request) (*http.Request, error) {
	switch in {
	case openapi3.ParameterInCookie:
		return transformRequestContext(
			extractCookie(name),
			transformCookie,
		)
	case openapi3.ParameterInHeader:
		return transformRequestContext(
			extractHeader(name),
			transformHeader,
		)
	case openapi3.ParameterInPath:
		return transformRequestContext(
			extractPathParam(name),
			transformPathParam,
		)
	case openapi3.ParameterInQuery:
		return transformRequestContext(
			extractQueryParam(name),
			transformQueryParam,
		)
	default:
		panic("unsupported parameter type: " + in)
	}
}

// ParameterOptions holds configuration for a parameter being added to an operation.
// This includes the OpenAPI parameter definition and a reference to the parent
// operation options for registering validators.
type ParameterOptions struct {
	operationOptions *OperationOptions
	def              *openapi3.Parameter
}

// ParameterOption configures a parameter created by [Cookie], [Header], [QueryParam], or [PathParam].
// Common implementations include [Required], [Regex], and authentication options like [JWTAuth].
type ParameterOption func(*ParameterOptions)

// MissingRequiredParameterError is returned when a required parameter is missing
// from an HTTP request. This error is wrapped in a [BadRequestError] and results
// in a 400 Bad Request response.
type MissingRequiredParameterError struct {
	Parameter string
	In        string
}

func (e MissingRequiredParameterError) Error() string {
	return fmt.Sprintf("missing required request parameter in %s: %s", e.In, e.Parameter)
}

// Required marks a parameter as required.
// If the parameter is not present in the request, the operation returns a 400 Bad Request
// with a [MissingRequiredParameterError].
//
// Example:
//
//	rest.Header("Authorization", rest.Required())
//	rest.QueryParam("id", rest.Required())
func Required() ParameterOption {
	return func(po *ParameterOptions) {
		po.def.Required = ptr.Ref(true)

		po.operationOptions.transforms = append(
			po.operationOptions.transforms,
			transformParam(
				po.def.Name,
				po.def.In,
				func(ctx context.Context, c []*http.Cookie) (context.Context, error) {
					if len(c) == 0 {
						return nil, BadRequestError{
							Cause: MissingRequiredParameterError{
								Parameter: po.def.Name,
								In:        string(po.def.In),
							},
						}
					}

					return ctx, nil
				},
				func(ctx context.Context, ss []string) (context.Context, error) {
					if len(ss) == 0 {
						return nil, BadRequestError{
							Cause: MissingRequiredParameterError{
								Parameter: po.def.Name,
								In:        string(po.def.In),
							},
						}
					}

					return ctx, nil
				},
				func(ctx context.Context, s string) (context.Context, error) {
					if len(s) == 0 {
						return nil, BadRequestError{
							Cause: MissingRequiredParameterError{
								Parameter: po.def.Name,
								In:        string(po.def.In),
							},
						}
					}

					return ctx, nil
				},
				func(ctx context.Context, ss []string) (context.Context, error) {
					if len(ss) == 0 {
						return nil, BadRequestError{
							Cause: MissingRequiredParameterError{
								Parameter: po.def.Name,
								In:        string(po.def.In),
							},
						}
					}

					return ctx, nil
				},
			),
		)
	}
}

// InvalidParameterValueError is returned when a parameter's value doesn't match
// the expected format or constraints. This error is wrapped in a [BadRequestError]
// and results in a 400 Bad Request response.
type InvalidParameterValueError struct {
	Parameter string
	In        string
}

func (e InvalidParameterValueError) Error() string {
	return fmt.Sprintf("invalid parameter value in %s: %s", e.In, e.Parameter)
}

// JWTVerifier defines the interface for verifying JWT tokens and injecting claims into the request context.
// Implementations should:
//  1. Verify the JWT token's signature and validity
//  2. Extract claims from the verified token
//  3. Inject the claims into the context for use by downstream handlers
//
// The token parameter contains the JWT without the "Bearer " prefix.
// Return an error if verification fails - this will result in a 401 Unauthorized response.
//
// Example implementation:
//
//	type MyVerifier struct {
//	    publicKey *rsa.PublicKey
//	}
//
//	func (v *MyVerifier) Verify(ctx context.Context, token string) (context.Context, error) {
//	    claims, err := jwt.Parse(token, v.publicKey)
//	    if err != nil {
//	        return nil, err
//	    }
//	    return context.WithValue(ctx, "claims", claims), nil
//	}
type JWTVerifier interface {
	Verify(ctx context.Context, token string) (context.Context, error)
}

// InvalidJWTError is returned when a JWT token is missing, malformed, or fails verification.
// This error is wrapped in an [UnauthorizedError] and results in a 401 Unauthorized response.
type InvalidJWTError struct {
	Parameter string
	In        string
	Cause     error
}

func (e InvalidJWTError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("invalid JWT in %s %s: %v", e.In, e.Parameter, e.Cause)
	}
	return fmt.Sprintf("invalid JWT in %s: %s", e.In, e.Parameter)
}

func (e InvalidJWTError) Unwrap() error {
	return e.Cause
}

// Regex validates that a parameter value matches the provided regular expression.
// If the parameter value doesn't match, the operation returns a 400 Bad Request
// with an [InvalidParameterValueError].
//
// Example:
//
//	rest.QueryParam("page", rest.Regex(regexp.MustCompile(`^\d+$`)))
//	rest.Header("X-Trace-ID", rest.Regex(regexp.MustCompile(`^[a-f0-9]{32}$`)))
func Regex(re *regexp.Regexp) ParameterOption {
	return func(po *ParameterOptions) {
		if po.def.Schema == nil {
			po.def.Schema = &openapi3.SchemaOrRef{
				Schema: &openapi3.Schema{},
			}
		}

		po.operationOptions.transforms = append(
			po.operationOptions.transforms,
			transformParam(
				po.def.Name,
				po.def.In,
				func(ctx context.Context, cs []*http.Cookie) (context.Context, error) {
					matches := func(c *http.Cookie) bool {
						return re.MatchString(c.Value)
					}

					if slices.ContainsFunc(cs, matches) {
						return ctx, nil
					}

					return nil, BadRequestError{
						Cause: InvalidParameterValueError{
							Parameter: po.def.Name,
							In:        string(po.def.In),
						},
					}
				},
				func(ctx context.Context, ss []string) (context.Context, error) {
					if slices.ContainsFunc(ss, re.MatchString) {
						return ctx, nil
					}

					return nil, BadRequestError{
						Cause: InvalidParameterValueError{
							Parameter: po.def.Name,
							In:        string(po.def.In),
						},
					}
				},
				func(ctx context.Context, s string) (context.Context, error) {
					if re.MatchString(s) {
						return ctx, nil
					}

					return nil, BadRequestError{
						Cause: InvalidParameterValueError{
							Parameter: po.def.Name,
							In:        string(po.def.In),
						},
					}
				},
				func(ctx context.Context, ss []string) (context.Context, error) {
					if slices.ContainsFunc(ss, re.MatchString) {
						return ctx, nil
					}

					return nil, BadRequestError{
						Cause: InvalidParameterValueError{
							Parameter: po.def.Name,
							In:        string(po.def.In),
						},
					}
				},
			),
		)
	}
}

// APIKey configures API key authentication for a parameter.
// This adds an API key security scheme to the OpenAPI specification.
// The parameter (typically a header or cookie) contains the API key.
//
// Example:
//
//	rest.Header("X-API-Key", rest.Required(), rest.APIKey())
func APIKey(schemeName string) ParameterOption {
	return func(po *ParameterOptions) {
		po.operationOptions.securityScheme = &securityScheme{
			name: schemeName,
			scheme: openapi3.SecurityScheme{
				APIKeySecurityScheme: &openapi3.APIKeySecurityScheme{
					Name: po.def.Name,
					In:   openapi3.APIKeySecuritySchemeIn(po.def.In),
				},
			},
		}
	}
}

// BasicAuth configures HTTP Basic authentication for a parameter.
// The schemeName identifies the security scheme in the OpenAPI specification.
//
// Example:
//
//	rest.Header("Authorization", rest.Required(), rest.BasicAuth("basic"))
func BasicAuth(schemeName string) ParameterOption {
	return func(po *ParameterOptions) {
		po.operationOptions.securityScheme = &securityScheme{
			name: schemeName,
			scheme: openapi3.SecurityScheme{
				APIKeySecurityScheme: &openapi3.APIKeySecurityScheme{
					Name: po.def.Name,
					In:   openapi3.APIKeySecuritySchemeIn(po.def.In),
				},
			},
		}
	}
}

// JWTAuth configures JWT Bearer token authentication for a parameter.
// The parameter (typically an Authorization header) should contain a JWT token in the format "Bearer <token>".
// The schemeName identifies the security scheme in the OpenAPI specification.
// The verifier is called to verify the JWT and inject claims into the request context.
//
// The verifier receives the extracted JWT token (without the "Bearer " prefix) and should:
//  1. Verify the token's signature and validity
//  2. Extract claims from the token
//  3. Return a new context with the claims injected
//  4. Return an error if verification fails
//
// If token extraction or verification fails, the operation returns a 401 Unauthorized response.
//
// Example:
//
//	type MyVerifier struct{}
//
//	func (v *MyVerifier) Verify(ctx context.Context, token string) (context.Context, error) {
//	    // Parse and verify the JWT token
//	    claims, err := jwt.Parse(token)
//	    if err != nil {
//	        return nil, err
//	    }
//	    // Inject claims into context
//	    return context.WithValue(ctx, "user_id", claims.UserID), nil
//	}
//
//	rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", &MyVerifier{}))
func JWTAuth(schemeName string, verifier JWTVerifier) ParameterOption {
	return func(po *ParameterOptions) {
		po.operationOptions.securityScheme = &securityScheme{
			name: schemeName,
			scheme: openapi3.SecurityScheme{
				HTTPSecurityScheme: &openapi3.HTTPSecurityScheme{
					Scheme:       "bearer",
					BearerFormat: ptr.Ref("JWT"),
				},
			},
		}

		// Add transformer to extract and verify JWT token
		po.operationOptions.transforms = append(
			po.operationOptions.transforms,
			func(r *http.Request) (*http.Request, error) {
				ctx := r.Context()

				// Extract header values from context (injected by injectParam)
				headerValues := HeaderValue(ctx, po.def.Name)

				// Extract Bearer token from Authorization header
				token, err := extractBearerToken(headerValues)
				if err != nil {
					return nil, UnauthorizedError{
						Cause: InvalidJWTError{
							Parameter: po.def.Name,
							In:        string(po.def.In),
							Cause:     err,
						},
					}
				}

				// Verify JWT and inject claims into context
				newCtx, err := verifier.Verify(ctx, token)
				if err != nil {
					return nil, UnauthorizedError{
						Cause: InvalidJWTError{
							Parameter: po.def.Name,
							In:        string(po.def.In),
							Cause:     err,
						},
					}
				}

				return r.WithContext(newCtx), nil
			},
		)
	}
}

// MutualTLS configures mutual TLS authentication.
// Note: This is currently not supported by the underlying OpenAPI library and will panic.
func MutualTLS(schemeName string) ParameterOption {
	return func(po *ParameterOptions) {
		panic("swaggest/openapi3 does not support this... damn")
	}
}

// OAuth2 configures OAuth 2.0 authentication for a parameter.
// The schemeName identifies the security scheme in the OpenAPI specification.
//
// Note: OAuth2 flows are not yet configured and need to be set manually.
//
// Example:
//
//	rest.Header("Authorization", rest.Required(), rest.OAuth2("oauth2"))
func OAuth2(schemeName string) ParameterOption {
	return func(po *ParameterOptions) {
		po.operationOptions.securityScheme = &securityScheme{
			name: schemeName,
			scheme: openapi3.SecurityScheme{
				OAuth2SecurityScheme: &openapi3.OAuth2SecurityScheme{
					// todo: set flows
				},
			},
		}
	}
}

// OpenIDConnect configures OpenID Connect authentication for a parameter.
// The wellKnownURL should point to the OpenID Connect discovery document.
//
// Example:
//
//	rest.Header("Authorization", rest.Required(),
//	    rest.OpenIDConnect("oidc", "https://accounts.example.com/.well-known/openid-configuration"))
func OpenIDConnect(schemeName string, wellKnownURL string) ParameterOption {
	return func(po *ParameterOptions) {
		po.operationOptions.securityScheme = &securityScheme{
			name: "openid-connect",
			scheme: openapi3.SecurityScheme{
				OpenIDConnectSecurityScheme: &openapi3.OpenIDConnectSecurityScheme{
					OpenIDConnectURL: wellKnownURL,
				},
			},
		}
	}
}
