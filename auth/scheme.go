// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth

import (
	"context"
	"crypto/x509"
	"net/url"

	"github.com/z5labs/humus/internal/authn"
)

// ErrUnauthenticated is what a verifier's error wraps when the credential
// it was given authenticates no one: a token that is expired, forged or
// for another audience, an API key that doesn't exist, a wrong password.
// Any other error from a verifier is the server's, and is never answered
// with a 401.
var ErrUnauthenticated = authn.ErrUnauthenticated

// ErrForbidden is the error of a request that a scheme authenticated, but
// whose principal lacks a scope its requirement needs.
var ErrForbidden = authn.ErrForbidden

// Scheme is a way to authenticate a request, whose principal is a P: the
// value the scheme's verify function returns, such as the end user's User
// or Claims. Each method returns a copy, so a declaration can be shared.
//
// A scheme is declared once, by its constructor, and shared: two schemes
// declared with one name are a mistake, even when they are alike. Its
// copies, made by its methods, are the same scheme, and From finds its
// principal through any of them.
//
// The zero Scheme is a mistake that NewHandler reports.
type Scheme[P any] struct {
	s *authn.Scheme
}

// declare declares s, with verify as its verify function: nil when the
// end user's is nil, so that NewHandler reports it.
func declare[P any](s authn.Scheme, verify authn.Verify) Scheme[P] {
	s.Verify = verify
	return Scheme[P]{authn.NewScheme(s)}
}

// tokenVerify wraps a verify function of a bearer token.
func tokenVerify[P any](verify func(ctx context.Context, token string) (P, error)) authn.Verify {
	if verify == nil {
		return nil
	}
	return func(ctx context.Context, c authn.Credential) (any, error) {
		return verify(ctx, c.Token)
	}
}

// keyVerify wraps a verify function of an API key.
func keyVerify[P any](verify func(ctx context.Context, key string) (P, error)) authn.Verify {
	if verify == nil {
		return nil
	}
	return func(ctx context.Context, c authn.Credential) (any, error) {
		return verify(ctx, c.Key)
	}
}

// basicVerify wraps a verify function of a username and password.
func basicVerify[P any](verify func(ctx context.Context, username, password string) (P, error)) authn.Verify {
	if verify == nil {
		return nil
	}
	return func(ctx context.Context, c authn.Credential) (any, error) {
		return verify(ctx, c.Username, c.Password)
	}
}

// chainsVerify wraps a verify function of verified certificate chains.
func chainsVerify[P any](verify func(ctx context.Context, chains [][]*x509.Certificate) (P, error)) authn.Verify {
	if verify == nil {
		return nil
	}
	return func(ctx context.Context, c authn.Credential) (any, error) {
		return verify(ctx, c.Chains)
	}
}

// Bearer reads a token from the Authorization header's Bearer scheme,
// whatever its case, and from the authorization metadata of an RPC.
func Bearer[P any](name string, verify func(ctx context.Context, token string) (P, error)) Scheme[P] {
	return declare[P](authn.Scheme{Name: name, Kind: authn.Bearer}, tokenVerify(verify))
}

// Basic reads a username and password from the Authorization header's
// Basic scheme, whatever its case. realm is the realm of its challenge,
// which RFC 7617 requires.
func Basic[P any](name, realm string, verify func(ctx context.Context, username, password string) (P, error)) Scheme[P] {
	return declare[P](authn.Scheme{Name: name, Kind: authn.Basic, Realm: realm}, basicVerify(verify))
}

// APIKeyHeader reads an API key from the header header. It is the one API
// key that can be read from an RPC, as its metadata.
func APIKeyHeader[P any](name, header string, verify func(ctx context.Context, key string) (P, error)) Scheme[P] {
	return declare[P](authn.Scheme{Name: name, Kind: authn.APIKeyHeader, Param: header}, keyVerify(verify))
}

// APIKeyQuery reads an API key from the query parameter param.
func APIKeyQuery[P any](name, param string, verify func(ctx context.Context, key string) (P, error)) Scheme[P] {
	return declare[P](authn.Scheme{Name: name, Kind: authn.APIKeyQuery, Param: param}, keyVerify(verify))
}

// APIKeyCookie reads an API key from the cookie cookie.
func APIKeyCookie[P any](name, cookie string, verify func(ctx context.Context, key string) (P, error)) Scheme[P] {
	return declare[P](authn.Scheme{Name: name, Kind: authn.APIKeyCookie, Param: cookie}, keyVerify(verify))
}

// MutualTLS reads the client certificate chains the TLS handshake
// verified, against the server's ClientCAs. A certificate the handshake
// didn't verify is no credential: the server's ClientAuth must be
// tls.VerifyClientCertIfGiven or tls.RequireAndVerifyClientCert.
func MutualTLS[P any](name string, verify func(ctx context.Context, chains [][]*x509.Certificate) (P, error)) Scheme[P] {
	return declare[P](authn.Scheme{Name: name, Kind: authn.MutualTLS}, chainsVerify(verify))
}

// OAuth2 reads a bearer token, as Bearer does. It differs from Bearer in a
// document, where flows lists how a client gets a token.
func OAuth2[P any](name string, flows Flows, verify func(ctx context.Context, token string) (P, error)) Scheme[P] {
	return declare[P](authn.Scheme{Name: name, Kind: authn.OAuth2, Flows: flows.internal()}, tokenVerify(verify))
}

// OpenIDConnect reads a bearer token, as Bearer does. It differs from
// Bearer in a document, where discovery is the provider's OpenID Connect
// discovery document.
func OpenIDConnect[P any](name string, discovery *url.URL, verify func(ctx context.Context, token string) (P, error)) Scheme[P] {
	return declare[P](authn.Scheme{Name: name, Kind: authn.OpenIDConnect, Discovery: discovery}, tokenVerify(verify))
}

// Description returns s, described by d in a document, in CommonMark.
func (s Scheme[P]) Description(d string) Scheme[P] {
	if s.s == nil {
		return s
	}
	return Scheme[P]{s.s.With(func(c *authn.Scheme) { c.Description = d })}
}

// Format returns s, whose tokens are in format f, such as JWT: the
// document's bearerFormat. It is for Bearer alone; on another scheme it is
// a mistake.
func (s Scheme[P]) Format(f string) Scheme[P] {
	if s.s == nil {
		return s
	}
	return Scheme[P]{s.s.With(func(c *authn.Scheme) { c.Format, c.Formatted = f, true })}
}

// From returns the principal s authenticated ctx's request with, and false
// when the request wasn't authenticated with s: it met a requirement
// without s, Anonymous, or Disabled.
func (s Scheme[P]) From(ctx context.Context) (P, bool) {
	var zero P
	if s.s == nil {
		return zero, false
	}
	v, ok := authn.Principal(ctx, s.s)
	if !ok {
		return zero, false
	}
	p, _ := v.(P)
	return p, true
}

// Flows are an OAuth2 scheme's flows, as a document lists them. A scheme
// has one flow at least. Scopes map each scope to its description, and a
// scope a requirement names must be in one of them.
type Flows struct {
	AuthorizationCode   *AuthorizationCodeFlow
	ClientCredentials   *ClientCredentialsFlow
	DeviceAuthorization *DeviceAuthorizationFlow
}

// AuthorizationCodeFlow is OAuth2's authorization code flow.
// AuthorizationURL and TokenURL are required; RefreshURL is optional.
type AuthorizationCodeFlow struct {
	AuthorizationURL, TokenURL, RefreshURL *url.URL
	Scopes                                 map[string]string
}

// ClientCredentialsFlow is OAuth2's client credentials flow. TokenURL is
// required; RefreshURL is optional.
type ClientCredentialsFlow struct {
	TokenURL, RefreshURL *url.URL
	Scopes               map[string]string
}

// DeviceAuthorizationFlow is OAuth2's device authorization flow.
// DeviceAuthorizationURL and TokenURL are required; RefreshURL is
// optional.
type DeviceAuthorizationFlow struct {
	DeviceAuthorizationURL, TokenURL, RefreshURL *url.URL
	Scopes                                       map[string]string
}

// internal returns f as the engine holds it.
func (f Flows) internal() authn.Flows {
	var out authn.Flows
	if fl := f.AuthorizationCode; fl != nil {
		out.AuthorizationCode = &authn.Flow{AuthorizationURL: fl.AuthorizationURL, TokenURL: fl.TokenURL, RefreshURL: fl.RefreshURL, Scopes: fl.Scopes}
	}
	if fl := f.ClientCredentials; fl != nil {
		out.ClientCredentials = &authn.Flow{TokenURL: fl.TokenURL, RefreshURL: fl.RefreshURL, Scopes: fl.Scopes}
	}
	if fl := f.DeviceAuthorization; fl != nil {
		out.DeviceAuthorization = &authn.Flow{DeviceAuthorizationURL: fl.DeviceAuthorizationURL, TokenURL: fl.TokenURL, RefreshURL: fl.RefreshURL, Scopes: fl.Scopes}
	}
	return out
}
