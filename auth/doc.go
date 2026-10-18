// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package auth authenticates and authorizes the requests a humus service
// serves.
//
// # Schemes and principals
//
// A [Scheme] is a typed value: a way to authenticate a request, whose
// verify function turns the request's credential into the end user's own
// principal type, such as their User or Claims. A handler reads the
// principal back with the scheme's [Scheme.From], typed, with no context
// key of the end user's and no type assertion:
//
//	bearer := auth.Bearer("bearer", verifyToken) // verifyToken returns a Claims
//	...
//	claims, ok := bearer.From(r.Context())
//
// There is one constructor for each of OpenAPI's security scheme types:
// [Bearer], [Basic], [APIKeyHeader], [APIKeyQuery], [APIKeyCookie],
// [MutualTLS], [OAuth2] and [OpenIDConnect]. Each one reads its credential
// as the table below says, and every one verifies: there is no scheme that
// only documents.
//
//	Bearer, OAuth2, OpenIDConnect  Authorization: Bearer <token>
//	Basic                          Authorization: Basic <base64 of username:password>
//	APIKeyHeader                   the header
//	APIKeyQuery                    the query parameter
//	APIKeyCookie                   the cookie
//	MutualTLS                      r.TLS.VerifiedChains
//
// The scheme name in Authorization matches whatever its case. MutualTLS
// reads only the chains the TLS handshake verified, so the server's
// Settings.TLS sets ClientCAs and a ClientAuth of
// tls.VerifyClientCertIfGiven, to serve requests that authenticate
// otherwise too, or tls.RequireAndVerifyClientCert. A certificate that was
// only requested, not verified, is no credential.
//
// A verify function returns an error that wraps [ErrUnauthenticated] when
// the credential authenticates no one: a token that is expired or forged,
// an API key that doesn't exist, a wrong password. Any other error is the
// server's, such as a directory that is down, and is never answered with a
// 401, which would tell a client to drop a valid credential.
//
// A scheme is declared once and shared. Its methods return copies, which
// are the same scheme; two schemes declared with one name are a mistake.
//
// # Requirements and scopes
//
// A [Requirement] is what a request must have: [Require] a scheme,
// [RequireScopes] for a principal that has scopes, [All] for several
// schemes at once, [Anonymous] for optional authentication, and [Disabled]
// for none at all. A list of requirements is met by any one of them, as
// OpenAPI's security is:
//
//  1. The requirements are tried in order, and the first one met wins.
//  2. A scheme is verified only when the request carries its credential,
//     and at most once per request, however many requirements name it.
//  3. A malformed credential is a rejected one: two Authorization
//     headers, Bearer with no token, Basic credentials that aren't base64
//     or have no colon, an API key given twice or empty.
//  4. A verifier's other error ends the request at once, as the server's,
//     whatever the requirements after it.
//  5. When none is met, the request is forbidden if some requirement had
//     every scheme authenticated and only lacked scopes, and
//     unauthenticated otherwise.
//
// The context of a request that is served carries the principals of the
// requirement it met, and only those: From of any other scheme returns
// false.
//
// RequireScopes is constrained to a [Scoped] principal, whose HasScope
// decides: auth never guesses where scopes live in a token. A role is a
// scope.
//
// # Anonymous and Disabled
//
// [Anonymous] is optional authentication. A request without credentials
// meets it, and is served with no principal; one whose credential is valid
// meets an earlier requirement, if the list names its scheme before
// Anonymous. But a request whose credential a scheme of the list rejects
// never meets Anonymous: a client with a bad token is told so, and not
// served as though it had sent none. To know that, Anonymous verifies
// every credential the request carries.
//
// [Disabled] turns authentication off. It reads no credential and
// verifies nothing, so From of every scheme returns false even for a
// request that carries one. It is how openapi and grpcauth make an
// operation, a document or a method public, and it stands alone in its
// list. [NewHandler] doesn't take it: a handler that authenticates no one
// is the handler itself.
//
// # Answers and challenges
//
// [NewHandler] serves a plain http.Handler, such as the end user's mux,
// only to the requests that meet one of its requirements. Paths beside it
// on the mux, such as health probes, are the end user's, and stay open.
// Every answer is the end user's, given in [Answers]: auth writes no body
// of its own, and no verifier's text reaches the client.
//
//   - Answers.Unauthenticated answers a request no requirement
//     authenticated: a 401.
//   - Answers.Forbidden answers a request that lacked a scope: a 403.
//   - Answers.Failed answers a request a verifier failed on with an error
//     of the server's, after the error is recorded.
//
// Before Unauthenticated is called, WWW-Authenticate holds one challenge
// for each scheme of the list that HTTP authentication defines, in order
// and without repeats, as RFC 9110 has it:
//
//   - Bearer, for Bearer, OAuth2 and OpenIDConnect together, or
//     Bearer error="invalid_token" when one of them rejected the request's
//     token, as RFC 6750 has it;
//   - Basic realm="<realm>", charset="UTF-8", as RFC 7617 has it.
//
// Before Forbidden is called, WWW-Authenticate holds
// Bearer error="insufficient_scope", scope="<the scopes>" when a bearer
// scheme lacked them. An API key and mutual TLS have no HTTP challenge, so
// they add none. Failed gets no challenge.
//
// # Records
//
// A 401 or 403 isn't recorded: it is the client's mistake, and otelhttp
// already records its status on the request's span and in
// http.server.request.duration. A verifier's other error is recorded
// once, before Answers.Failed:
//
//   - as an exception event on the request's span;
//   - as one ERROR record, "authentication failed", in the request's
//     context, with humus.auth.scheme, the scheme's name, error.type,
//     exception.message and http.request.method.
//
// When the client has gone away, the error goes on the span alone. No
// principal, subject or key is recorded, since that is personal data in
// most services; a verify function that wants it on the span adds it. auth
// creates no instrument, logs through the Setup's LoggerProvider with the
// logger name "github.com/z5labs/humus/auth", and reads and sets no
// OpenTelemetry global.
//
// auth recovers nothing. A verifier runs on the request's goroutine,
// before the handler, so its panic passes to httpserver, which records
// "handler panicked" and aborts the response.
package auth
