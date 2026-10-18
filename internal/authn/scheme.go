// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package authn is the authentication engine that auth.NewHandler,
// openapi.Security and grpcauth share.
//
// It holds what the auth package declares, schemes and requirements, in a
// form the three can read, and authenticates a request against a list of
// requirements, as the authentication proposal, #870, has it:
//
//   - the requirements are tried in order, and the first one met wins;
//   - a scheme is verified only when the request carries its credential,
//     and at most once per request;
//   - a malformed credential is a rejected one;
//   - a verifier's error that doesn't wrap ErrUnauthenticated ends the
//     request at once, as the server's;
//   - Anonymous is met only when no scheme of the list rejects a credential
//     the request carries;
//   - a request that meets no requirement is forbidden when some
//     requirement only lacked scopes, and unauthenticated otherwise;
//   - a list that is Disabled serves the request, reads no credential and
//     puts no principal in its context.
//
// Requirement has no exported method, because auth.Requirement is an
// alias of it, and an end user must reach nothing of it but its zero value.
// The packages that read one do it with this package's functions.
package authn

import (
	"context"
	"crypto/x509"
	"errors"
	"maps"
	"net/url"
)

// ErrUnauthenticated is what a verifier's error wraps when the credential
// it was given authenticates no one. auth exports it.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrForbidden is the error of a request that a scheme authenticated, but
// whose principal lacks a scope its requirement needs. auth exports it.
var ErrForbidden = errors.New("forbidden")

// Kind is the kind of a scheme: one of OpenAPI's security scheme types,
// with the API key's three places told apart.
type Kind int

// The kinds of scheme.
const (
	Bearer Kind = iota + 1
	Basic
	APIKeyHeader
	APIKeyQuery
	APIKeyCookie
	MutualTLS
	OAuth2
	OpenIDConnect
)

// String returns the name of the auth constructor of k.
func (k Kind) String() string {
	switch k {
	case Bearer:
		return "Bearer"
	case Basic:
		return "Basic"
	case APIKeyHeader:
		return "APIKeyHeader"
	case APIKeyQuery:
		return "APIKeyQuery"
	case APIKeyCookie:
		return "APIKeyCookie"
	case MutualTLS:
		return "MutualTLS"
	case OAuth2:
		return "OAuth2"
	case OpenIDConnect:
		return "OpenIDConnect"
	}
	return "Kind(?)"
}

// ReadsBearer reports whether a scheme of kind k reads a bearer token from
// the Authorization header: Bearer, OAuth2 and OpenIDConnect.
func (k Kind) ReadsBearer() bool {
	return k == Bearer || k == OAuth2 || k == OpenIDConnect
}

// Credential is what a scheme read from a request, and hands its verify
// function. Only the fields of the scheme's kind are set.
type Credential struct {
	// Token is a bearer token: Bearer, OAuth2 and OpenIDConnect.
	Token string

	// Username and Password are Basic's.
	Username, Password string

	// Key is an API key's.
	Key string

	// Chains are MutualTLS's: the client certificate chains the TLS
	// handshake verified.
	Chains [][]*x509.Certificate
}

// Verify is a scheme's verify function, with its principal as an any.
type Verify func(ctx context.Context, c Credential) (any, error)

// Flow is one of an OAuth2 scheme's flows. Which URLs it needs depends on
// the flow.
type Flow struct {
	AuthorizationURL       *url.URL
	DeviceAuthorizationURL *url.URL
	TokenURL               *url.URL
	RefreshURL             *url.URL
	Scopes                 map[string]string
}

// Flows are an OAuth2 scheme's flows, each nil when the scheme lacks it.
type Flows struct {
	AuthorizationCode   *Flow
	ClientCredentials   *Flow
	DeviceAuthorization *Flow
}

// declares reports whether a flow of f declares scope.
func (f Flows) declares(scope string) bool {
	for _, flow := range []*Flow{f.AuthorizationCode, f.ClientCredentials, f.DeviceAuthorization} {
		if flow == nil {
			continue
		}
		if _, ok := flow.Scopes[scope]; ok {
			return true
		}
	}
	return false
}

// clone returns a copy of f whose flows and scopes are its own.
func (f Flows) clone() Flows {
	c := func(flow *Flow) *Flow {
		if flow == nil {
			return nil
		}
		cp := *flow
		cp.Scopes = maps.Clone(flow.Scopes)
		return &cp
	}
	return Flows{
		AuthorizationCode:   c(f.AuthorizationCode),
		ClientCredentials:   c(f.ClientCredentials),
		DeviceAuthorization: c(f.DeviceAuthorization),
	}
}

// Scheme is a way to authenticate a request. Its fields are fixed once
// NewScheme returns it; With returns a changed copy.
type Scheme struct {
	// Name is the scheme's name: its key in a document's securitySchemes.
	Name string

	// Kind is the scheme's kind.
	Kind Kind

	// Param is the header, query parameter or cookie an API key is read
	// from.
	Param string

	// Realm is Basic's realm.
	Realm string

	// Format is a bearer token's format, such as JWT, and Formatted
	// reports whether Format was set at all, which is a mistake on a
	// scheme that isn't Bearer.
	Format    string
	Formatted bool

	// Description is the scheme's description in a document, in
	// CommonMark.
	Description string

	// Flows are OAuth2's.
	Flows Flows

	// Discovery is OpenIDConnect's discovery document.
	Discovery *url.URL

	// Verify is the scheme's verify function, nil when the end user's was.
	Verify Verify

	// id is shared by every copy With makes, so that From finds a
	// principal through any of them.
	id *identity
}

// identity tells one declared scheme from another. It isn't zero-sized,
// so two of them never share an address.
type identity struct{ _ byte }

// NewScheme returns s, declared: a scheme of its own, which every copy
// With makes of it shares.
func NewScheme(s Scheme) *Scheme {
	s.Flows = s.Flows.clone()
	s.id = new(identity)
	return &s
}

// With returns a copy of s, changed by edit.
func (s *Scheme) With(edit func(*Scheme)) *Scheme {
	c := *s
	c.Flows = s.Flows.clone()
	edit(&c)
	return &c
}

// Same reports whether s and o are one scheme: copies of one declaration,
// documented alike.
func (s *Scheme) Same(o *Scheme) bool {
	return s.id == o.id && s.Format == o.Format && s.Formatted == o.Formatted && s.Description == o.Description
}

// principalsKey is the context key of a served request's principals.
type principalsKey struct{}

// principals are the principals of the requirement a request met, by the
// scheme that authenticated each.
type principals map[*identity]any

// Principal returns the principal s authenticated ctx's request with, and
// false when s didn't authenticate it.
func Principal(ctx context.Context, s *Scheme) (any, bool) {
	if s == nil {
		return nil, false
	}
	ps, _ := ctx.Value(principalsKey{}).(principals)
	p, ok := ps[s.id]
	return p, ok
}
