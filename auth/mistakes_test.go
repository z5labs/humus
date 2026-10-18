// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/telemetry"
)

// The verify functions of the declarations below are never called.
func verifyToken(context.Context, string) (user, error) { return user{}, nil }

func verifyBasic(context.Context, string, string) (user, error) { return user{}, nil }

func verifyChains(context.Context, [][]*x509.Certificate) (user, error) { return user{}, nil }

// mustURL parses s.
func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	require.NoError(t, err)
	return u
}

// setup is a Setup whose telemetry discards everything.
func setup() *humus.Setup { return &humus.Setup{Telemetry: telemetry.Noop()} }

func TestNewHandler_reports_each_mistake(t *testing.T) {
	ok := http.NotFoundHandler()
	b := auth.Bearer("bearer", verifyToken)
	tokenURL := mustURL(t, "https://idp.example.com/token")
	authorizeURL := mustURL(t, "https://idp.example.com/authorize")
	deviceURL := mustURL(t, "https://idp.example.com/device")
	scopes := map[string]string{"carts:read": "Read carts."}

	for _, tc := range []struct {
		name    string
		s       *humus.Setup
		h       http.Handler
		answers auth.Answers
		reqs    []auth.Requirement
		want    string
	}{
		{name: "a nil s", h: ok, answers: answers, reqs: []auth.Requirement{auth.Require(b)}, want: "s is nil"},
		{name: "a nil h", s: setup(), answers: answers, reqs: []auth.Requirement{auth.Require(b)}, want: "h is nil"},
		{
			name: "a nil Unauthenticated", s: setup(), h: ok, reqs: []auth.Requirement{auth.Require(b)},
			answers: auth.Answers{Forbidden: ok, Failed: ok}, want: "Answers.Unauthenticated is nil",
		},
		{
			name: "a nil Forbidden", s: setup(), h: ok, reqs: []auth.Requirement{auth.Require(b)},
			answers: auth.Answers{Unauthenticated: ok, Failed: ok}, want: "Answers.Forbidden is nil",
		},
		{
			name: "a nil Failed", s: setup(), h: ok, reqs: []auth.Requirement{auth.Require(b)},
			answers: auth.Answers{Unauthenticated: ok, Forbidden: ok}, want: "Answers.Failed is nil",
		},
		{name: "no requirement", want: "no requirement"},
		{
			name: "Disabled",
			reqs: []auth.Requirement{auth.Disabled()},
			want: "reqs[0] is Disabled, which authenticates no one: serve h itself",
		},
		{name: "the zero Requirement", reqs: []auth.Requirement{{}}, want: "reqs[0] is the zero Requirement"},
		{name: "the zero Scheme", reqs: []auth.Requirement{auth.Require(auth.Scheme[user]{})}, want: "reqs[0] names the zero Scheme"},
		{
			name: "a nil verify function",
			reqs: []auth.Requirement{auth.Require(auth.APIKeyHeader[user]("apiKey", "X-API-Key", nil))},
			want: `reqs[0]: the APIKeyHeader scheme "apiKey" has a nil verify function`,
		},
		{
			name: "two different schemes with one name",
			reqs: []auth.Requirement{auth.Require(b), auth.Require(auth.Bearer("bearer", verifyToken))},
			want: `reqs[1]: two different schemes are named "bearer"`,
		},
		{
			name: "one scheme, described two ways, under one name",
			reqs: []auth.Requirement{auth.Require(b), auth.Require(b.Description("A token."))},
			want: `reqs[1]: two different schemes are named "bearer"`,
		},
		{
			name: "an empty name",
			reqs: []auth.Requirement{auth.Require(auth.Bearer("", verifyToken))},
			want: "reqs[0]: a Bearer scheme has an empty name",
		},
		{
			name: "a name with a space",
			reqs: []auth.Requirement{auth.Require(auth.Bearer("my bearer", verifyToken))},
			want: `reqs[0]: the Bearer scheme "my bearer" has a name with a character other than letters, digits, '.', '-' and '_'`,
		},
		{
			name: "one scheme twice in one All",
			reqs: []auth.Requirement{auth.All(auth.Require(b), auth.RequireScopes(b, "carts:read"))},
			want: `reqs[0]: All names the scheme "bearer" twice`,
		},
		{
			name: "one scheme twice in nested Alls",
			reqs: []auth.Requirement{auth.All(auth.All(auth.Require(b)), auth.Require(b))},
			want: `reqs[0]: All names the scheme "bearer" twice`,
		},
		{
			name: "an All of no requirement",
			reqs: []auth.Requirement{auth.All()},
			want: "reqs[0] is All with no requirement",
		},
		{
			name: "the zero Requirement inside an All",
			reqs: []auth.Requirement{auth.All(auth.Require(b), auth.Requirement{})},
			want: "reqs[0]: All's reqs[1] is the zero Requirement",
		},
		{
			name: "Disabled with another requirement",
			reqs: []auth.Requirement{auth.Require(b), auth.Disabled()},
			want: "reqs[1] is Disabled, which stands alone, but the list holds 2 requirements",
		},
		{
			name: "Disabled inside an All",
			reqs: []auth.Requirement{auth.All(auth.Require(b), auth.Disabled())},
			want: "reqs[0]: All's reqs[1] is Disabled, which stands alone, never inside an All",
		},
		{
			name: "Format on a scheme that isn't Bearer",
			reqs: []auth.Requirement{auth.Require(auth.APIKeyHeader("apiKey", "X-API-Key", verifyToken).Format("JWT"))},
			want: `reqs[0]: the APIKeyHeader scheme "apiKey" has a Format, which is for Bearer alone`,
		},
		{
			name: "Format on OAuth2",
			reqs: []auth.Requirement{auth.Require(auth.OAuth2("oauth", auth.Flows{
				ClientCredentials: &auth.ClientCredentialsFlow{TokenURL: tokenURL},
			}, verifyToken).Format("JWT"))},
			want: `reqs[0]: the OAuth2 scheme "oauth" has a Format, which is for Bearer alone`,
		},
		{
			name: "an empty Basic realm",
			reqs: []auth.Requirement{auth.Require(auth.Basic("basic", "", verifyBasic))},
			want: `reqs[0]: the Basic scheme "basic" has an empty realm`,
		},
		{
			name: "a Basic realm that holds a quote",
			reqs: []auth.Requirement{auth.Require(auth.Basic("basic", `the "admin" realm`, verifyBasic))},
			want: `reqs[0]: the Basic scheme "basic" has a realm that holds a quote`,
		},
		{
			name: "an API key read from an empty name",
			reqs: []auth.Requirement{auth.Require(auth.APIKeyCookie("apiKey", "", verifyToken))},
			want: `reqs[0]: the APIKeyCookie scheme "apiKey" reads its key from an empty name`,
		},
		{
			name: "an OAuth2 with no flow",
			reqs: []auth.Requirement{auth.Require(auth.OAuth2("oauth", auth.Flows{}, verifyToken))},
			want: `reqs[0]: the OAuth2 scheme "oauth" has no flow`,
		},
		{
			name: "an authorization code flow without its token URL",
			reqs: []auth.Requirement{auth.Require(auth.OAuth2("oauth", auth.Flows{
				AuthorizationCode: &auth.AuthorizationCodeFlow{AuthorizationURL: authorizeURL},
			}, verifyToken))},
			want: `reqs[0]: the OAuth2 scheme "oauth": its AuthorizationCode flow has no TokenURL`,
		},
		{
			name: "an authorization code flow without its authorization URL",
			reqs: []auth.Requirement{auth.Require(auth.OAuth2("oauth", auth.Flows{
				AuthorizationCode: &auth.AuthorizationCodeFlow{TokenURL: tokenURL},
			}, verifyToken))},
			want: `reqs[0]: the OAuth2 scheme "oauth": its AuthorizationCode flow has no AuthorizationURL`,
		},
		{
			name: "a client credentials flow without its token URL",
			reqs: []auth.Requirement{auth.Require(auth.OAuth2("oauth", auth.Flows{
				ClientCredentials: &auth.ClientCredentialsFlow{},
			}, verifyToken))},
			want: `reqs[0]: the OAuth2 scheme "oauth": its ClientCredentials flow has no TokenURL`,
		},
		{
			name: "a device authorization flow without its device authorization URL",
			reqs: []auth.Requirement{auth.Require(auth.OAuth2("oauth", auth.Flows{
				DeviceAuthorization: &auth.DeviceAuthorizationFlow{TokenURL: tokenURL},
			}, verifyToken))},
			want: `reqs[0]: the OAuth2 scheme "oauth": its DeviceAuthorization flow has no DeviceAuthorizationURL`,
		},
		{
			name: "a device authorization flow without its token URL",
			reqs: []auth.Requirement{auth.Require(auth.OAuth2("oauth", auth.Flows{
				DeviceAuthorization: &auth.DeviceAuthorizationFlow{DeviceAuthorizationURL: deviceURL},
			}, verifyToken))},
			want: `reqs[0]: the OAuth2 scheme "oauth": its DeviceAuthorization flow has no TokenURL`,
		},
		{
			name: "a scope no flow declares",
			reqs: []auth.Requirement{auth.RequireScopes(auth.OAuth2("oauth", auth.Flows{
				ClientCredentials: &auth.ClientCredentialsFlow{TokenURL: tokenURL, Scopes: scopes},
			}, verifyToken), "carts:read", "carts:write")},
			want: `reqs[0]: the scope "carts:write" is declared by no flow of the OAuth2 scheme "oauth"`,
		},
		{
			name: "an OpenIDConnect with no discovery URL",
			reqs: []auth.Requirement{auth.Require(auth.OpenIDConnect("oidc", nil, verifyToken))},
			want: `reqs[0]: the OpenIDConnect scheme "oidc" has a discovery URL that isn't absolute`,
		},
		{
			name: "an OpenIDConnect whose discovery URL is relative",
			reqs: []auth.Requirement{auth.Require(auth.OpenIDConnect("oidc", mustURL(t, "/.well-known/openid-configuration"), verifyToken))},
			want: `reqs[0]: the OpenIDConnect scheme "oidc" has a discovery URL that isn't absolute`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.s == nil && tc.want != "s is nil" {
				tc.s = setup()
			}
			if tc.h == nil && tc.want != "h is nil" {
				tc.h = ok
			}
			if tc.answers == (auth.Answers{}) {
				tc.answers = answers
			}
			h, err := auth.NewHandler(tc.s, tc.h, tc.answers, tc.reqs...)
			require.Nil(t, h)
			require.ErrorContains(t, err, "auth: NewHandler: ")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestNewHandler_reports_two_mistakes_in_one_error(t *testing.T) {
	h, err := auth.NewHandler(setup(), nil, answers,
		auth.Require(auth.Basic("basic", "", verifyBasic)),
		auth.Require(auth.MutualTLS[user]("mtls", nil)),
	)
	require.Nil(t, h)
	require.ErrorContains(t, err, "h is nil")
	require.ErrorContains(t, err, `reqs[0]: the Basic scheme "basic" has an empty realm`)
	require.ErrorContains(t, err, `reqs[1]: the MutualTLS scheme "mtls" has a nil verify function`)
}

func TestNewHandler_takes_every_kind_of_scheme(t *testing.T) {
	tokenURL := mustURL(t, "https://idp.example.com/token")
	scopes := map[string]string{"carts:read": "Read carts."}
	b := auth.Bearer("bearer", verifyToken).Format("JWT").Description("A JWT from the IdP.")
	h, err := auth.NewHandler(setup(), http.NotFoundHandler(), answers,
		auth.RequireScopes(b, "carts:read"),
		auth.Require(b),
		auth.All(auth.Require(auth.Basic("basic", "carts", verifyBasic)), auth.Require(auth.MutualTLS("mtls", verifyChains))),
		auth.Require(auth.APIKeyHeader("header", "X-API-Key", verifyToken)),
		auth.Require(auth.APIKeyQuery("query", "api_key", verifyToken)),
		auth.Require(auth.APIKeyCookie("cookie", "session", verifyToken)),
		auth.RequireScopes(auth.OAuth2("oauth", auth.Flows{
			AuthorizationCode: &auth.AuthorizationCodeFlow{
				AuthorizationURL: mustURL(t, "https://idp.example.com/authorize"),
				TokenURL:         tokenURL,
				Scopes:           map[string]string{"carts:write": "Change carts."},
			},
			ClientCredentials: &auth.ClientCredentialsFlow{TokenURL: tokenURL, Scopes: scopes},
			DeviceAuthorization: &auth.DeviceAuthorizationFlow{
				DeviceAuthorizationURL: mustURL(t, "https://idp.example.com/device"),
				TokenURL:               tokenURL,
			},
		}, verifyToken), "carts:read", "carts:write"),
		auth.Require(auth.OpenIDConnect("oidc.v1", mustURL(t, "https://idp.example.com/.well-known/openid-configuration"), verifyToken)),
		auth.All(auth.Require(auth.APIKeyHeader("tenant", "X-Tenant", verifyToken)), auth.Anonymous()),
		auth.Anonymous(),
	)
	require.NoError(t, err)
	require.NotNil(t, h)
}
