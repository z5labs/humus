// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth

import (
	"slices"

	"github.com/z5labs/humus/internal/authn"
)

// Requirement is what a request must have to be served: every scheme of
// it, each with its scopes. Given a list of requirements, a request must
// meet one.
//
// A Requirement is built with Require, RequireScopes, All, Anonymous and
// Disabled. The zero Requirement is a mistake that NewHandler reports.
type Requirement = authn.Requirement

// Scoped is a principal that has scopes. A role is a scope: OpenAPI 3.1
// and later let a requirement of any scheme list roles.
type Scoped interface {
	HasScope(scope string) bool
}

// Require is the requirement that s authenticates the request.
func Require[P any](s Scheme[P]) Requirement {
	return authn.Require(s.s, nil, nil)
}

// RequireScopes is the requirement that s authenticates the request with
// a principal that has every one of scopes, as its HasScope reports. P
// must be Scoped, so a scope on a principal that has none doesn't compile.
func RequireScopes[P Scoped](s Scheme[P], scopes ...string) Requirement {
	return authn.Require(s.s, slices.Clone(scopes), func(principal any, scope string) bool {
		p, ok := principal.(P)
		return ok && p.HasScope(scope)
	})
}

// All is the requirement that every one of reqs is met: OpenAPI's
// requirement object with more than one scheme. A scheme named twice in
// one All, an All of no requirement, and Disabled inside an All are
// mistakes. Anonymous inside an All adds nothing to it.
func All(reqs ...Requirement) Requirement {
	return authn.All(slices.Clone(reqs))
}

// Anonymous is the requirement a request meets without credentials, for
// an operation whose authentication is optional: OpenAPI's empty
// requirement object. Credentials that a scheme of the list rejects are
// still refused, never served anonymously: to know that, Anonymous
// verifies every credential the request carries. A request served by it
// carries no principal, so From of every scheme returns false.
func Anonymous() Requirement {
	return authn.Anonymous()
}

// Disabled is the requirement every request meets, with no credential
// read: it turns authentication off. It is how an operation, the document
// or a method is made public when the API or Service requires
// authentication. It stands alone: with another requirement in the list,
// or in All, it is a mistake. Unlike Anonymous, it verifies nothing, so
// From of every scheme returns false, even for a request that carries a
// credential.
//
// NewHandler reports Disabled as a mistake: a handler that authenticates
// no one is h itself.
func Disabled() Requirement {
	return authn.Disabled()
}
