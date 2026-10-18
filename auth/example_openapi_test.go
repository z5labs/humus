// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
)

// The "Authentication and authorization" page of the docs site quotes the
// code in this file, and TestCartsInit in example_openapi_app_test.go checks
// what the page says about it. TestDocs_code_samples checks that every quote
// still matches it.

// Claims are the end user's principal: whatever their identity provider
// puts in a token, decoded by auth/jwt.
type Claims struct {
	Subject string `json:"sub"`
	Scope   string `json:"scope"` // space-separated, as RFC 9068 has it
}

// HasScope reports whether the token was granted scope. It makes Claims an
// auth.Scoped, so that RequireScopes can name a scope of it.
func (c Claims) HasScope(scope string) bool { return slices.Contains(strings.Fields(c.Scope), scope) }

// IdP is the identity provider the service's tokens come from, as the end
// user's Init sources it, such as with config.Struct.
type IdP struct {
	// KeySet is the provider's jwks_uri: an https URL.
	KeySet *url.URL

	// Roots are the CAs the provider's server is trusted through, or nil
	// for the system's.
	Roots *x509.CertPool

	// Tokens are what every token is checked against: its iss, its aud,
	// its algorithms and the clock's leeway.
	Tokens jwt.Settings
}

// cart is the one cart path: /carts/mine.
var cart = openapi.Root().Lit("carts").Lit("mine")

// cartBody is a cart, as the API answers it.
type cartBody struct {
	Owner string `json:"owner"`
}

// greetingBody is a greeting, as the API answers it.
type greetingBody struct {
	Message string `json:"message"`
}

// carts serves the API's operations. Every handler reads its principal
// from bearer.
type carts struct {
	bearer auth.Scheme[Claims]
}

func (c carts) getCart(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (cartBody, error) {
	claims, _ := c.bearer.From(ctx) // always there: the API's Security requires bearer
	return cartBody{Owner: claims.Subject}, nil
}

func (c carts) deleteCart(context.Context, openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
	return openapi.EmptyBody{}, nil
}

func (c carts) greet(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (greetingBody, error) {
	claims, ok := c.bearer.From(ctx) // false for a request served anonymously
	if !ok {
		return greetingBody{Message: "hello, stranger"}, nil
	}
	return greetingBody{Message: "hello, " + claims.Subject}, nil
}

// CartsInit serves a carts API that requires a JWT bearer token from idp,
// and carts:write to delete a cart, with a greeting whose authentication
// is optional, its document public, and its health probes open beside it.
func CartsInit(idp IdP, tlsConfig *tls.Config) func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		// The key set's client is for its origin alone, and fetches
		// nothing until the Verifier's first token.
		clientSettings := httpclient.DefaultSettings()
		clientSettings.BaseURL = &url.URL{Scheme: idp.KeySet.Scheme, Host: idp.KeySet.Host}
		clientSettings.TLS = &tls.Config{RootCAs: idp.Roots}
		client, err := httpclient.NewClient(s, clientSettings)
		if err != nil {
			return nil, err
		}
		keys, err := jwt.NewRemoteKeySet(idp.KeySet, client)
		if err != nil {
			return nil, err
		}
		tokens, err := jwt.NewVerifier[Claims](s, idp.Tokens, keys)
		if err != nil {
			return nil, err
		}
		bearer := auth.Bearer("bearer", tokens.Verify).Format("JWT")
		c := carts{bearer: bearer}

		api, err := openapi.NewAPI(s,
			openapi.Info{Title: "Carts", Version: "1.0.0"},
			openapi.Unmatched{
				NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
				MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
			},
			openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem), // answers 401 and 403 too
			openapi.CatchAll(http.StatusInternalServerError, internalError, jsoncodec.Problem),
			[]openapi.Operation{
				openapi.Get(cart, c.getCart, jsoncodec.Encoder),
				openapi.Delete(cart, c.deleteCart, openapi.EmptyEncoder,
					openapi.Status(http.StatusNoContent),
					openapi.Security(auth.RequireScopes(bearer, "carts:write")),
				),
				openapi.Get(openapi.Root().Lit("greeting"), c.greet, jsoncodec.Encoder,
					openapi.Security(auth.Require(bearer), auth.Anonymous()), // optional
				),
				openapi.Document(openapi.Root().Lit("openapi.json"),
					openapi.Security(auth.Disabled()), // public
				),
			},
			openapi.Security(auth.Require(bearer)),
			// A token the Verifier had no keys for, because the key set is
			// unreachable, is the server's outage, which clients retry.
			openapi.Error[*jwt.KeySetError](http.StatusServiceUnavailable, keysUnavailable),
		)
		if err != nil {
			return nil, err
		}

		// The probes are beside the API on the mux, and nothing wraps
		// them, so they stay open for the kubelet.
		hs := health.NewService()
		mux := http.NewServeMux()
		mux.Handle("/", api)
		mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))

		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = tlsConfig
		svc, err := httpserver.NewService(s, settings, mux)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc, hs}, nil
	}
}

// internalError is the catch-all's wrap. Its answer is fixed, and never
// carries err's text, which the API records instead.
func internalError(error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
	}
}

// keysUnavailable is the encoder of the answer to a *jwt.KeySetError: a
// 503, as a problem detail, which says to try again. The KeySet's error
// isn't in the body, which would hand the client the identity provider's
// address and how it failed.
func keysUnavailable() openapi.Encoder[*jwt.KeySetError] {
	return unavailable{jsoncodec.Problem[problem.Details]()}
}

// unavailable answers every *jwt.KeySetError with one problem detail,
// written by a problem.Details encoder, whose media type and schema it
// keeps.
type unavailable struct {
	openapi.Encoder[problem.Details]
}

func (e unavailable) Encode(*jwt.KeySetError) (io.WriterTo, error) {
	return e.Encoder.Encode(problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusServiceUnavailable),
		Status: http.StatusServiceUnavailable,
		Detail: "The server can't verify tokens right now. Try again later.",
	})
}

// A carts API whose operations require a JWT bearer token, verified
// against an identity provider's JSON Web Key Set. This example has no
// Output comment, so it is compiled but never run: running it would exit
// the test binary.
func ExampleBearer() {
	keySet, _ := url.Parse("https://idp.example.com/.well-known/jwks.json")
	tokens := jwt.DefaultSettings() // sourced with config.Struct
	tokens.Issuer = "https://idp.example.com"
	tokens.Audience = "carts"
	httpserver.Main(CartsInit(IdP{KeySet: keySet, Tokens: tokens}, &tls.Config{}))
}
