// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package jwt verifies JWT bearer tokens, against keys from a [KeySet], and
// decodes each one's claims into the end user's own type. It is opt-in: auth
// names no token format, and a service whose clients bring JWTs from an
// identity provider uses [Verifier.Verify] as the verify function of a
// Bearer, OAuth2 or OpenIDConnect scheme:
//
//	tokens, err := jwt.NewVerifier[Claims](s, settings, keys)
//	if err != nil {
//		return nil, err
//	}
//	bearer := auth.Bearer("bearer", tokens.Verify).Format("JWT")
//
// # Settings
//
// [Settings] are plain values, sourced by the end user, such as with
// config.Struct; the package reads no environment variable. Start from
// [DefaultSettings]: Issuer and Audience have no default.
//
//	Field               Default                      Valid                                        Check
//	Issuer              none                         not empty                                    CheckIssuer
//	Audience            none                         not empty                                    CheckAudience
//	Algorithms          RS256, PS256, ES256, EdDSA   not empty, each one of the constants,        CheckAlgorithms
//	                                                 none twice
//	Leeway              1m                           0 to 5m                                      CheckLeeway
//	MinRefreshInterval  1m                           positive                                     CheckMinRefreshInterval
//	MaxKeyAge           1h                           positive, and at least MinRefreshInterval    CheckMaxKeyAge; the order in NewVerifier
//
// Each Check function rejects an out-of-range value without naming the
// field, so whoever sourced it names the field or the variable.
// [NewVerifier] runs every check, names each bad field, and reports them
// together, with a nil KeySet among them.
//
// # Verifying a token
//
// For each token, [Verifier.Verify]:
//
//  1. parses it as a compact JWS signed with one of Algorithms, so none
//     and HMAC are rejected;
//  2. requires a kid in its header, which the keys held must have;
//  3. verifies its signature, and decodes its payload into the registered
//     claims and into a C, the end user's type, which the package reads
//     nothing from;
//  4. requires iss to be Issuer, aud to hold Audience, exp to be present
//     and not past, and nbf and iat not to be in the future, all within
//     Leeway of time.Now().
//
// Every rejection wraps auth.ErrUnauthenticated, so auth answers it with a
// 401.
//
// # An identity provider's JSON Web Key Set
//
// The keys come from a [KeySet]. [RemoteKeySet] is the standard one: it
// fetches the JSON Web Key Set an identity provider publishes at its
// jwks_uri, as RFC 7517 has it, with the end user's httpclient client,
// built for the key set's origin:
//
//	idpSettings := httpclient.DefaultSettings() // sourced with config.Struct
//	idpSettings.BaseURL = idpURL                // https://idp.example.com
//	idpSettings.TLS = &tls.Config{RootCAs: idpRoots}
//	idp, err := httpclient.NewClient(s, idpSettings)
//	if err != nil {
//		return nil, err
//	}
//	keys, err := jwt.NewRemoteKeySet(jwksURL, idp) // the provider's jwks_uri
//	if err != nil {
//		return nil, err
//	}
//	tokens, err := jwt.NewVerifier[Claims](s, settings, keys)
//
// The URL must be https, with a host: a key set fetched in plain text would
// let anyone on the path add a key and sign their own tokens, which the
// service would accept. The client trusts the provider through its
// TLS.RootCAs, or the system's roots when RootCAs is nil: httpclient
// refuses InsecureSkipVerify, and refuses a request to another origin, so
// the client is built for the key set's. [NewRemoteKeySet] names each
// mistake in one error: a nil client, and a URL that is relative, isn't
// https or has no host.
//
// Each call of [RemoteKeySet.Keys] is one fetch. It reads at most 1 MiB of
// the body, and fails on a larger one, on a status other than 200, and on
// a body that isn't a JSON Web Key Set, with an error that names the URL.
// It returns the set's signing keys, and skips a key without kid, with a
// use other than sig, with private parts, or of a type go-jose doesn't
// read, rather than failing the set. It keeps no keys, and records nothing
// of its own: the Verifier decides when it is called, and the fetch is a
// span of the client's, measured by its metrics, and bounded by its
// Timeout and retried as its Settings say.
//
// # A KeySet of the end user's
//
// A KeySet is one method, which returns every [Key] the end user has, from
// wherever they keep them. Not every key infrastructure serves a JSON Web
// Key Set: keys come from secret stores, files and providers' own APIs. A source that holds PEM keys turns each into a
// crypto.PublicKey with x509.ParsePKIXPublicKey. A key's Public is an
// *rsa.PublicKey, an *ecdsa.PublicKey or an ed25519.PublicKey; a key whose
// ID is empty, or whose Public is anything else, a private key among them,
// is skipped, and the others are kept.
//
// A KeySet is a fetch and nothing more. The Verifier holds the keys, and
// decides when to call it, so a KeySet needs no cache of its own, and every
// rule below holds whatever the source.
//
// A KeySet decides which tokens the service accepts: whoever can add a key
// to it can sign tokens the service serves. So it is the end user's to
// secure, as a key set fetched over https is secured by its TLS: its source
// is reached over an authenticated channel, and holds only the identity
// provider's public keys.
//
// # When keys are got
//
// Keys are got when they are needed, never at startup: an identity
// provider that is down when the pod starts doesn't fail Init, and an API
// whose tokens can't be verified still serves its public operations. The
// Verifier calls its KeySet:
//
//   - with the first token;
//   - when a token names a kid the held keys lack, which is how an identity
//     provider's key rotation reaches the service;
//   - when the held keys are older than MaxKeyAge, so a key the provider
//     withdrew stops verifying.
//
// The calls are at most one per MinRefreshInterval, whatever the tokens. So
// a client that sends tokens with made-up kids can't make the service call
// its KeySet on every request: within the interval, an unknown kid is
// rejected. Concurrent requests share one call. The call gets
// context.WithoutCancel of the request's context, so it carries the
// request's trace, and a client that goes away doesn't fail it for the
// requests waiting on it. A request stops waiting when its own context
// ends, and Verify returns the context's error. A KeySet bounds its own
// calls: a RemoteKeySet's are bounded by its client's Timeout.
//
// # Stale keys over none
//
// When a call fails and keys are held, the Verifier goes on verifying with
// them, past MaxKeyAge, and logs one WARN record, "keys refresh failed",
// with error.type and exception.message. A short outage of the key source
// doesn't log every user out. After MinRefreshInterval, the next token that
// needs keys calls the KeySet again.
//
// # KeySetError
//
// When the Verifier has no key to verify a token with because its KeySet
// failed, Verify's error is a [*KeySetError], which wraps the KeySet's
// error:
//
//   - when no keys have ever been got, and the call fails. Within
//     MinRefreshInterval of that call, the next token fails at once with
//     the same error, without calling again;
//   - when a token names a kid the held keys lack, and the call for it
//     fails, or the last call failed within MinRefreshInterval. The kid may
//     be a key the provider rotated in, which the service couldn't get.
//
// A kid that a call which succeeded still lacks is a 401, so made-up kids
// stay rejected.
//
// A *KeySetError never wraps auth.ErrUnauthenticated: a 401 would tell the
// client to drop a valid token. It is the server's error, and the end user
// answers it, such as with a 503, so clients retry. In openapi, an Error
// option answers it, with errors.As, and no wrapping in a verify function:
//
//	openapi.Error[*jwt.KeySetError](http.StatusServiceUnavailable, enc)
//
// Over gRPC, a verify function that turns it into
// status.Error(codes.Unavailable, ...) gets Unavailable. Without either, it
// is answered as any verifier's other error: the catch-all's 500,
// Answers.Failed, or Internal.
//
// # Telemetry
//
// A Verifier logs through the Setup's Telemetry.LoggerProvider, with the
// logger name "github.com/z5labs/humus/auth/jwt", and logs only "keys
// refresh failed". It records no principal, subject or key, creates no
// instrument, and reads and sets no OpenTelemetry global. A rejected token
// isn't recorded here: auth records a verifier's other error, and a 401 is
// the client's mistake.
//
// A KeySet is called on a goroutine of the Verifier's, so a panic in it
// would recover nowhere: it is the call's error instead.
//
// # Testing a verify function
//
// A test of a verify function gives the Verifier a KeySet of its own, in
// memory, with no identity provider to serve. It generates a key with
// crypto/rand, signs its tokens with it, and runs in a testing/synctest
// bubble, so token times, MinRefreshInterval and MaxKeyAge take no real
// time. A KeySet that counts its calls shows when the Verifier got keys,
// and one that fails shows a *KeySetError. The package's Example does so.
//
// A test of the end user's whole App, on a RemoteKeySet, serves the key set
// from a handler of its own, with httpserver on a humustest Host, over TLS
// with a certificate from humustest.NewCA, whose Pool the App's httpclient
// client trusts through RootCAs.
package jwt
