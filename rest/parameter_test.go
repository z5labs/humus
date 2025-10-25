// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMissingRequiredParameterError_Error(t *testing.T) {
	t.Run("formats error with parameter name and location", func(t *testing.T) {
		err := MissingRequiredParameterError{
			Parameter: "userId",
			In:        "query",
		}

		expected := "missing required request parameter in query: userId"
		assert.Equal(t, expected, err.Error())
	})

	t.Run("handles header parameter", func(t *testing.T) {
		err := MissingRequiredParameterError{
			Parameter: "Authorization",
			In:        "header",
		}

		expected := "missing required request parameter in header: Authorization"
		assert.Equal(t, expected, err.Error())
	})

	t.Run("handles cookie parameter", func(t *testing.T) {
		err := MissingRequiredParameterError{
			Parameter: "session",
			In:        "cookie",
		}

		expected := "missing required request parameter in cookie: session"
		assert.Equal(t, expected, err.Error())
	})
}

func TestInvalidParameterValueError_Error(t *testing.T) {
	t.Run("formats error with parameter name and location", func(t *testing.T) {
		err := InvalidParameterValueError{
			Parameter: "page",
			In:        "query",
		}

		expected := "invalid parameter value in query: page"
		assert.Equal(t, expected, err.Error())
	})

	t.Run("handles header parameter", func(t *testing.T) {
		err := InvalidParameterValueError{
			Parameter: "X-Request-ID",
			In:        "header",
		}

		expected := "invalid parameter value in header: X-Request-ID"
		assert.Equal(t, expected, err.Error())
	})
}

func TestCookie(t *testing.T) {
	t.Run("creates an operation option", func(t *testing.T) {
		opt := Cookie("session")
		assert.NotNil(t, opt)
	})

	t.Run("accepts parameter options", func(t *testing.T) {
		opt := Cookie("session", Required())
		assert.NotNil(t, opt)
	})
}

func TestHeader(t *testing.T) {
	t.Run("creates an operation option", func(t *testing.T) {
		opt := Header("Authorization")
		assert.NotNil(t, opt)
	})

	t.Run("accepts parameter options", func(t *testing.T) {
		opt := Header("Authorization", Required(), JWTAuth("jwt"))
		assert.NotNil(t, opt)
	})
}

func TestQueryParam(t *testing.T) {
	t.Run("creates an operation option", func(t *testing.T) {
		opt := QueryParam("page")
		assert.NotNil(t, opt)
	})

	t.Run("accepts parameter options", func(t *testing.T) {
		opt := QueryParam("page", Required())
		assert.NotNil(t, opt)
	})
}

func TestRequired(t *testing.T) {
	t.Run("creates a parameter option", func(t *testing.T) {
		opt := Required()
		assert.NotNil(t, opt)
	})
}

func TestAPIKey(t *testing.T) {
	t.Run("creates a parameter option for API key auth", func(t *testing.T) {
		opt := APIKey()
		assert.NotNil(t, opt)
	})
}

func TestBasicAuth(t *testing.T) {
	t.Run("creates a parameter option for basic auth", func(t *testing.T) {
		opt := BasicAuth("basic")
		assert.NotNil(t, opt)
	})

	t.Run("accepts custom scheme name", func(t *testing.T) {
		opt := BasicAuth("custom-basic")
		assert.NotNil(t, opt)
	})
}

func TestJWTAuth(t *testing.T) {
	t.Run("creates a parameter option for JWT auth", func(t *testing.T) {
		opt := JWTAuth("jwt")
		assert.NotNil(t, opt)
	})

	t.Run("accepts custom scheme name", func(t *testing.T) {
		opt := JWTAuth("my-jwt")
		assert.NotNil(t, opt)
	})
}

func TestOAuth2(t *testing.T) {
	t.Run("creates a parameter option for OAuth2", func(t *testing.T) {
		opt := OAuth2("oauth2")
		assert.NotNil(t, opt)
	})

	t.Run("accepts custom scheme name", func(t *testing.T) {
		opt := OAuth2("my-oauth")
		assert.NotNil(t, opt)
	})
}

func TestOpenIDConnect(t *testing.T) {
	t.Run("creates a parameter option for OpenID Connect", func(t *testing.T) {
		opt := OpenIDConnect("oidc", "https://example.com/.well-known/openid-configuration")
		assert.NotNil(t, opt)
	})

	t.Run("accepts custom URLs", func(t *testing.T) {
		opt := OpenIDConnect("myoidc", "https://auth.mycompany.com/.well-known/openid-configuration")
		assert.NotNil(t, opt)
	})
}

func TestMutualTLS(t *testing.T) {
	t.Run("panics because it's not supported", func(t *testing.T) {
		defer func() {
			r := recover()
			assert.NotNil(t, r, "Expected panic from MutualTLS")
			assert.Contains(t, r, "swaggest/openapi3 does not support this")
		}()

		opt := MutualTLS("mtls")
		// Apply the option to trigger the panic
		po := &ParameterOptions{
			operationOptions: &OperationOptions{},
		}
		opt(po)
	})
}
