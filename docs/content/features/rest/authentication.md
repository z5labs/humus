---
title: Authentication
description: JWT, API keys, and security
weight: 35
type: docs
---


Humus REST services provide built-in support for multiple authentication schemes with automatic OpenAPI security specification generation.

## Overview

Authentication in Humus is handled through parameter validation options that you apply to headers, query parameters, or cookies. The framework:

- **Extracts** authentication credentials from requests
- **Validates** credentials using your custom logic
- **Injects** verified data into the request context
- **Generates** OpenAPI security schemes automatically
- **Returns** appropriate error responses (401 Unauthorized, 400 Bad Request)

## Parameter Validation Basics

Before diving into authentication, understand parameter validation:

### Required Parameters

Mark parameters as required to ensure they're present:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/protected"),
    handler,
    rest.Header("Authorization", rest.Required()),
)
```

Missing required parameters return `400 Bad Request` with a descriptive error.

### Regular Expression Validation

Validate parameter format with regex:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/v1/users"),
    handler,
    rest.QueryParam("api_version", rest.Regex(regexp.MustCompile(`^v\d+$`))),
)
```

Invalid formats return `400 Bad Request`.

### Combining Validators

Chain multiple validators together:

```go
rest.Header(
    "X-API-Key",
    rest.Required(),
    rest.Regex(regexp.MustCompile(`^[a-f0-9]{32}$`)),
    rest.APIKey("api-key"),
)
```

Validators run in order. The first failure stops validation and returns an error.

## Authentication Schemes

Humus supports five authentication schemes, each adding appropriate OpenAPI security documentation.

### API Key Authentication

API keys can be passed in headers, query parameters, or cookies:

```go
// Header-based API key
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/data"),
    handler,
    rest.Header("X-API-Key", rest.Required(), rest.APIKey("api-key")),
)

// Query parameter API key
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/data"),
    handler,
    rest.QueryParam("key", rest.Required(), rest.APIKey("api-key")),
)

// Cookie-based API key
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/data"),
    handler,
    rest.Cookie("api_key", rest.Required(), rest.APIKey("api-key")),
)
```

Access the API key in your handler:

```go
handler := rest.ProducerFunc[Response](func(ctx context.Context) (*Response, error) {
    apiKey := rest.HeaderValue(ctx, "X-API-Key")
    // Validate API key against your database
    if !isValidAPIKey(apiKey[0]) {
        return nil, fmt.Errorf("invalid API key")
    }
    return processRequest(ctx)
})
```

### Basic Authentication

HTTP Basic authentication (username:password encoded in Base64):

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/admin"),
    handler,
    rest.Header("Authorization", rest.Required(), rest.BasicAuth("basic")),
)
```

Parse Basic auth credentials in your handler:

```go
handler := rest.ProducerFunc[Response](func(ctx context.Context) (*Response, error) {
    authHeader := rest.HeaderValue(ctx, "Authorization")[0]

    // Parse "Basic <base64>" format
    if !strings.HasPrefix(authHeader, "Basic ") {
        return nil, fmt.Errorf("invalid authorization header")
    }

    encoded := strings.TrimPrefix(authHeader, "Basic ")
    decoded, err := base64.StdEncoding.DecodeString(encoded)
    if err != nil {
        return nil, fmt.Errorf("invalid base64 encoding")
    }

    // Split username:password
    credentials := strings.SplitN(string(decoded), ":", 2)
    if len(credentials) != 2 {
        return nil, fmt.Errorf("invalid credentials format")
    }

    username, password := credentials[0], credentials[1]
    if !validateCredentials(username, password) {
        return nil, fmt.Errorf("invalid credentials")
    }

    return processRequest(ctx)
})
```

### JWT Authentication

JWT (JSON Web Token) Bearer authentication provides the most comprehensive solution with automatic token extraction and verification.

#### How It Works

The framework:
1. Extracts the `Authorization` header
2. Validates the "Bearer <token>" format
3. Strips the "Bearer " prefix
4. Calls your `JWTVerifier.Verify()` method with the clean token
5. Returns `401 Unauthorized` if verification fails
6. Continues processing with the updated context if successful

#### JWTVerifier Interface

Implement the `JWTVerifier` interface to handle token verification:

```go
type JWTVerifier interface {
    Verify(ctx context.Context, token string) (context.Context, error)
}
```

The `token` parameter is the JWT without the "Bearer " prefix. Your implementation should:
1. Verify the token's signature
2. Validate claims (expiration, issuer, audience, etc.)
3. Extract relevant claims
4. Inject claims into the context
5. Return error if verification fails

#### Complete Example with golang-jwt/jwt

```go
package main

import (
    "context"
    "crypto/rsa"
    "fmt"
    "time"

    "github.com/golang-jwt/jwt/v5"
    "github.com/z5labs/humus/rest"
)

// Custom claims structure
type Claims struct {
    UserID string   `json:"user_id"`
    Email  string   `json:"email"`
    Roles  []string `json:"roles"`
    jwt.RegisteredClaims
}

// JWTVerifier implementation
type MyJWTVerifier struct {
    publicKey *rsa.PublicKey
}

func NewJWTVerifier(publicKey *rsa.PublicKey) *MyJWTVerifier {
    return &MyJWTVerifier{publicKey: publicKey}
}

func (v *MyJWTVerifier) Verify(ctx context.Context, tokenString string) (context.Context, error) {
    // Parse and verify the token
    token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
        // Verify signing method
        if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
            return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
        }
        return v.publicKey, nil
    })

    if err != nil {
        return nil, fmt.Errorf("failed to parse token: %w", err)
    }

    // Extract claims
    claims, ok := token.Claims.(*Claims)
    if !ok || !token.Valid {
        return nil, fmt.Errorf("invalid token claims")
    }

    // Additional validation
    if claims.ExpiresAt.Before(time.Now()) {
        return nil, fmt.Errorf("token expired")
    }

    // Inject claims into context
    ctx = context.WithValue(ctx, "user_id", claims.UserID)
    ctx = context.WithValue(ctx, "email", claims.Email)
    ctx = context.WithValue(ctx, "roles", claims.Roles)

    return ctx, nil
}

// Context keys for type-safe access
type contextKey string

const (
    userIDKey contextKey = "user_id"
    emailKey  contextKey = "email"
    rolesKey  contextKey = "roles"
)

// Helper functions to extract claims
func GetUserID(ctx context.Context) (string, bool) {
    userID, ok := ctx.Value(userIDKey).(string)
    return userID, ok
}

func GetEmail(ctx context.Context) (string, bool) {
    email, ok := ctx.Value(emailKey).(string)
    return email, ok
}

func GetRoles(ctx context.Context) ([]string, bool) {
    roles, ok := ctx.Value(rolesKey).([]string)
    return roles, ok
}
```

#### Registering with JWT Auth

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    api := rest.NewApi("Secure API", "1.0.0")

    // Load your public key (example)
    publicKey, err := loadPublicKey("public.pem")
    if err != nil {
        return nil, err
    }

    verifier := NewJWTVerifier(publicKey)

    // Protected endpoint
    handler := rest.ProducerFunc[UserProfile](func(ctx context.Context) (*UserProfile, error) {
        // Extract user info from context
        userID, ok := GetUserID(ctx)
        if !ok {
            return nil, fmt.Errorf("user ID not found in context")
        }

        email, _ := GetEmail(ctx)
        roles, _ := GetRoles(ctx)

        return &UserProfile{
            ID:    userID,
            Email: email,
            Roles: roles,
        }, nil
    })

    rest.Handle(
        http.MethodGet,
        rest.BasePath("/profile"),
        rest.ProduceJson(handler),
        rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", verifier)),
    )

    return api, nil
}
```

#### Testing JWT Authentication

```go
func TestJWTAuthentication(t *testing.T) {
    // Create test verifier
    privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)
    verifier := NewJWTVerifier(&privateKey.PublicKey)

    // Create valid token
    claims := &Claims{
        UserID: "user-123",
        Email:  "user@example.com",
        Roles:  []string{"admin"},
        RegisteredClaims: jwt.RegisteredClaims{
            ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
        },
    }
    token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
    tokenString, _ := token.SignedString(privateKey)

    // Test request
    req := httptest.NewRequest(http.MethodGet, "/profile", nil)
    req.Header.Set("Authorization", "Bearer "+tokenString)

    // Make request and verify
    // ... (standard HTTP testing)
}
```

#### Error Handling

JWT authentication returns different HTTP status codes depending on the type of error:

**400 Bad Request** - Token extraction failures (malformed request):
- Missing Authorization header (when JWTAuth used without `Required()`)
- Malformed header (not "Bearer <token>" format)
- Empty token after "Bearer " prefix

**401 Unauthorized** - Token verification failures (authentication failed):
- Invalid JWT signature
- Expired token
- Invalid claims (issuer, audience, etc.)
- Any error returned by your `JWTVerifier.Verify()` method

Example error scenarios:

```bash
# Missing header - 400 Bad Request
curl http://localhost:8080/profile
# Returns: 400 Bad Request

# Malformed header (missing "Bearer") - 400 Bad Request
curl -H "Authorization: invalid-token" http://localhost:8080/profile
# Returns: 400 Bad Request

# Empty token - 400 Bad Request
curl -H "Authorization: Bearer " http://localhost:8080/profile
# Returns: 400 Bad Request

# Invalid token (verification fails) - 401 Unauthorized
curl -H "Authorization: Bearer invalid.jwt.token" http://localhost:8080/profile
# Returns: 401 Unauthorized

# Expired token (verification fails) - 401 Unauthorized
curl -H "Authorization: Bearer expired.jwt.token" http://localhost:8080/profile
# Returns: 401 Unauthorized
```

**Note:** When combined with `Required()`, missing headers return `400 Bad Request` from the `Required()` validator, which runs before JWT verification.

### OAuth 2.0

OAuth 2.0 authentication scheme:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/data"),
    handler,
    rest.Header("Authorization", rest.Required(), rest.OAuth2("oauth2")),
)
```

**Note:** OAuth 2.0 flows are not yet fully configured in the OpenAPI spec. You'll need to implement the OAuth flow manually in your handler.

### OpenID Connect

OpenID Connect authentication with discovery URL:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/data"),
    handler,
    rest.Header(
        "Authorization",
        rest.Required(),
        rest.OpenIDConnect("oidc", "https://accounts.example.com/.well-known/openid-configuration"),
    ),
)
```

The discovery URL should point to your OpenID Connect provider's configuration endpoint.

## OpenAPI Security Schemes

All authentication options automatically add security schemes to your OpenAPI specification:

```go
api := rest.NewApi(
    "Secure API",
    "1.0.0",
    rest.Handle(
        http.MethodPost,
        rest.BasePath("/users"),
        createUserHandler,
        rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", verifier)),
    ),
)
```

The generated `/openapi.json` includes:

```json
{
  "components": {
    "securitySchemes": {
      "jwt": {
        "type": "http",
        "scheme": "bearer",
        "bearerFormat": "JWT"
      }
    }
  },
  "paths": {
    "/users": {
      "post": {
        "security": [
          {"jwt": []}
        ]
      }
    }
  }
}
```

This integrates with Swagger UI, Postman, and other OpenAPI tools.

## Security Best Practices

### 1. Always Use HTTPS in Production

Never transmit authentication credentials over unencrypted HTTP:

```go
// In production configuration
rest.Config{
    Port: 8443,
    TLS: &rest.TLSConfig{
        CertFile: "/path/to/cert.pem",
        KeyFile:  "/path/to/key.pem",
    },
}
```

### 2. Validate Token Expiration

Always check token expiration in your verifier:

```go
if claims.ExpiresAt.Before(time.Now()) {
    return nil, fmt.Errorf("token expired")
}
```

### 3. Use Strong Signing Algorithms

Prefer RS256 (RSA) or ES256 (ECDSA) over HS256 (HMAC):

```go
// Verify signing method
if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
    return nil, fmt.Errorf("unexpected signing method")
}
```

### 4. Implement Rate Limiting

Protect authentication endpoints from brute force attacks (use middleware or external service).

### 5. Rotate Keys Regularly

Implement key rotation for JWT signing keys and API keys.

### 6. Use Context Keys with Types

Avoid string collisions by using typed context keys:

```go
type contextKey string

const userIDKey contextKey = "user_id"

// Set
ctx = context.WithValue(ctx, userIDKey, "user-123")

// Get with type safety
userID, ok := ctx.Value(userIDKey).(string)
```

### 7. Validate All Claims

Check audience, issuer, and other relevant claims:

```go
if claims.Issuer != "https://auth.yourservice.com" {
    return nil, fmt.Errorf("invalid issuer")
}

if !claims.VerifyAudience("your-service", true) {
    return nil, fmt.Errorf("invalid audience")
}
```

### 8. Log Authentication Failures

Monitor for suspicious activity:

```go
func (v *MyJWTVerifier) Verify(ctx context.Context, token string) (context.Context, error) {
    // ... verification logic
    if err != nil {
        logger.WarnContext(ctx, "JWT verification failed", "error", err)
        return nil, err
    }
    return ctx, nil
}
```

## Common Patterns

### Role-Based Access Control

Combine JWT authentication with role checking:

```go
func requireRole(requiredRole string) rest.OperationOption {
    return func(oo *rest.OperationOptions) {
        oo.transforms = append(oo.transforms, func(r *http.Request) (*http.Request, error) {
            roles, ok := GetRoles(r.Context())
            if !ok {
                return nil, fmt.Errorf("roles not found in context")
            }

            hasRole := false
            for _, role := range roles {
                if role == requiredRole {
                    hasRole = true
                    break
                }
            }

            if !hasRole {
                return nil, fmt.Errorf("insufficient permissions")
            }

            return r, nil
        })
    }
}

// Usage
rest.Handle(
    http.MethodDelete,
    rest.BasePath("/users").Param("id"),
    deleteHandler,
    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", verifier)),
    requireRole("admin"),
)
```

### Optional Authentication

Make authentication optional by omitting `Required()`:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/public-or-private"),
    handler,
    rest.Header("Authorization", rest.JWTAuth("jwt", verifier)), // No Required()
)
```

Check in handler:

```go
handler := rest.ProducerFunc[Response](func(ctx context.Context) (*Response, error) {
    if userID, ok := GetUserID(ctx); ok {
        // Authenticated user - return personalized response
        return getPersonalizedResponse(ctx, userID)
    }

    // Anonymous user - return generic response
    return getPublicResponse(ctx)
})
```

### Multiple Authentication Schemes

Support multiple authentication methods:

```go
// Create verifier that handles multiple schemes
type MultiAuthVerifier struct {
    jwtVerifier *JWTVerifier
    apiKeyStore map[string]string
}

func (v *MultiAuthVerifier) Verify(ctx context.Context, token string) (context.Context, error) {
    // Try JWT first
    ctx, err := v.jwtVerifier.Verify(ctx, token)
    if err == nil {
        return ctx, nil
    }

    // Fall back to API key
    if userID, ok := v.apiKeyStore[token]; ok {
        return context.WithValue(ctx, userIDKey, userID), nil
    }

    return nil, fmt.Errorf("authentication failed")
}
```

## Next Steps

- Learn about [Interceptors]({{< ref "interceptors" >}}) for custom authentication logic
- Learn about [Error Handling]({{< ref "error-handling" >}}) for custom authentication error responses
- Explore [OpenAPI]({{< ref "openapi" >}}) to customize security documentation
- Read [Handler Helpers]({{< ref "handler-helpers" >}}) for implementing authenticated handlers
- See [Testing]({{< ref "/advanced/testing" >}}) for authentication test patterns
