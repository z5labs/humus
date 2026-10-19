# Authentication and authorization

The [`auth`](https://pkg.go.dev/github.com/z5labs/humus/auth) package
authenticates and authorizes the requests a humus service serves, over HTTP
and gRPC alike. A **scheme** is a typed value whose verify function turns a
request's credential into the end user's own principal, which a handler reads
back with the scheme's `From`. A **requirement** is what a request must have:
a scheme, a scheme with scopes, several schemes, or nothing at all. Three
places enforce requirements, one for each way humus serves requests:
`openapi.Security` for an [OpenAPI API](/humus/pr-preview/pr-1096/openapi/),
`auth.NewHandler` for a plain `http.Handler` served by
[`httpserver`](/humus/pr-preview/pr-1096/http/), and
[`grpcauth`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpcauth)'s
`Interceptors` for a [`grpcserver`](/humus/pr-preview/pr-1096/grpc/) Service.

`auth` names no token format. The opt-in
[`auth/jwt`](https://pkg.go.dev/github.com/z5labs/humus/auth/jwt) package
verifies JWT bearer tokens, against an identity provider's JSON Web Key Set
or keys from anywhere else the end user keeps them, and its `Verifier.Verify`
is the verify function of a bearer scheme.

The design behind everything on this page is in
[Proposal: authentication and authorization](https://github.com/z5labs/humus/discussions/870),
with its [revision 1](https://github.com/z5labs/humus/discussions/870#discussioncomment-19020305).

## Schemes and principals

A [`Scheme`](https://pkg.go.dev/github.com/z5labs/humus/auth#Scheme) is
declared once, in the end user's `Init`, by the constructor of its type, with
a name and a verify function. The verify function's result is the
principal, of whatever type the end user chooses, such as their `Operator`,
`User` or `Claims`, and the scheme's type parameter is that type. Here an
admin API authenticates its operators with a client certificate or a
password:

```go
// A certificate is verified by the handshake, against ClientCAs:
// its common name is the operator, who has every role.
cert := auth.MutualTLS("cert", func(_ context.Context, chains [][]*x509.Certificate) (Operator, error) {
	return Operator{Name: chains[0][0].Subject.CommonName, Roles: []string{"admin"}}, nil
})
password := auth.Basic("password", "admin", func(ctx context.Context, name, pw string) (Operator, error) {
	want, roles, ok, err := dir.Password(ctx, name)
	if err != nil {
		return Operator{}, err // the directory is down: the server's error, never a 401
	}
	if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(want)) != 1 {
		return Operator{}, auth.ErrUnauthenticated
	}
	return Operator{Name: name, Roles: roles}, nil
})
```

A handler reads the principal back with the scheme's `From`, typed, with no
context key of the end user's and no type assertion. `From` returns false
when the request wasn't authenticated with that scheme:

```go
whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	op, ok := cert.From(r.Context())
	if !ok {
		op, _ = password.From(r.Context())
	}
	_, _ = io.WriteString(w, op.Name)
})
```

There is one constructor for each of OpenAPI's security scheme types, and
each one reads its credential from one place:

| Constructor | Over HTTP | Over gRPC |
|---|---|---|
| `auth.Bearer`, `auth.OAuth2`, `auth.OpenIDConnect` | `Authorization: Bearer <token>` | the `authorization` metadata, the same value |
| `auth.Basic` | `Authorization: Basic <base64 of username:password>` | the `authorization` metadata, the same value |
| `auth.APIKeyHeader` | the header | the metadata key, lowercased |
| `auth.APIKeyQuery`, `auth.APIKeyCookie` | the query parameter, the cookie | a mistake `Interceptors` reports |
| `auth.MutualTLS` | `r.TLS.VerifiedChains` | the peer's `credentials.TLSInfo` |

Every scheme verifies: there is no scheme that only documents. `OAuth2` and
`OpenIDConnect` read a bearer token, as `Bearer` does, and differ from it in
the OpenAPI document, where `OAuth2` lists its flows and `OpenIDConnect` its
discovery URL. A scheme's `Description` describes it in the document, and
`Format`, for a `Bearer` alone, is its `bearerFormat`, such as `JWT`.

`MutualTLS` reads only the chains the TLS handshake verified, so the server's
`Settings.TLS` sets `ClientCAs` and a `ClientAuth` of
`tls.VerifyClientCertIfGiven`, to serve requests that authenticate otherwise
too, or `tls.RequireAndVerifyClientCert`. A certificate that was only
requested, not verified, is no credential.

A verify function returns an error that wraps
[`auth.ErrUnauthenticated`](https://pkg.go.dev/github.com/z5labs/humus/auth#ErrUnauthenticated)
when the credential authenticates no one: a token that is expired or forged,
an API key that doesn't exist, a wrong password. **Any other error is the
server's,** such as a directory that is down, and is never answered with a
401, which would tell a client to drop a valid credential;
[The answers](#the-answers) says what it gets instead.

A scheme is declared once and shared: two schemes declared with one name are
a mistake, even when they are alike. Its methods return copies, which are the
same scheme, and `From` finds its principal through any of them.

## Requirements and scopes

A [`Requirement`](https://pkg.go.dev/github.com/z5labs/humus/auth#Requirement)
is what a request must have:

- `auth.Require(s)`: the scheme `s` authenticates the request;
- `auth.RequireScopes(s, scopes...)`: it does so with a principal that has
  every one of the scopes;
- `auth.All(reqs...)`: every one of `reqs` is met, for several schemes at
  once, which is OpenAPI's requirement object with more than one scheme;
- `auth.Anonymous()`: optional authentication;
- `auth.Disabled()`: none at all, as [the next section](#opening-a-path)
  explains.

The three places each take a list of requirements, and **a list is met by
any one of them,** as OpenAPI's `security` is. The admin API reads with
either scheme, and writes only with the `admin` role:

```go
reads, err := auth.NewHandler(s, whoami, answers, auth.Require(cert), auth.Require(password))
if err != nil {
	return nil, err
}
writes, err := auth.NewHandler(s, whoami, answers,
	auth.RequireScopes(cert, "admin"), auth.RequireScopes(password, "admin"),
)
```

Given a list, every one of the three places authenticates a request the same
way:

1. The requirements are tried in order, and the first one met wins.
2. A scheme is verified only when the request carries its credential, and at
   most once per request, however many requirements name it. A request with
   only an API key never reaches the bearer scheme's verifier.
3. A malformed credential is a rejected one: two `Authorization` headers,
   `Bearer` with no token, Basic credentials that aren't base64 or have no
   colon, an API key given twice or empty.
4. A verifier's other error ends the request at once, as the server's,
   whatever the requirements after it. The outcome never depends on which
   alternative happened to fail.
5. When none is met, the request is forbidden if some requirement had every
   scheme authenticated and only lacked scopes, and unauthenticated
   otherwise.

The context of a request that is served carries the principals of the
requirement it met, and only those: `From` of any other scheme returns false.

### Scopes

`RequireScopes` is constrained to an
[`auth.Scoped`](https://pkg.go.dev/github.com/z5labs/humus/auth#Scoped)
principal, whose `HasScope` decides, so a scope on a principal that has none
doesn't compile. `auth` never guesses where scopes live in a token. A role is
a scope: the admin API's `Operator` reports its roles,

```go
// HasScope reports whether the operator has role, so that RequireScopes
// can require a role.
func (o Operator) HasScope(role string) bool { return slices.Contains(o.Roles, role) }
```

and a JWT's claims report the token's `scope` claim:

```go
// Claims are the end user's principal: whatever their identity provider
// puts in a token, decoded by auth/jwt.
type Claims struct {
	Subject string `json:"sub"`
	Scope   string `json:"scope"` // space-separated, as RFC 9068 has it
}

// HasScope reports whether the token was granted scope. It makes Claims an
// auth.Scoped, so that RequireScopes can name a scope of it.
func (c Claims) HasScope(scope string) bool { return slices.Contains(strings.Fields(c.Scope), scope) }
```

### Anonymous

`auth.Anonymous()` is optional authentication, OpenAPI's empty requirement
object. A request without credentials meets it, and is served with no
principal. One whose credential is valid meets an earlier requirement, if
the list names its scheme before `Anonymous`. But **a request whose
credential a scheme of the list rejects never meets `Anonymous`:** a client
with a bad token is told so, and not served as though it had sent none. To
know that, `Anonymous` verifies every credential the request carries.

## Opening a path

`auth.Disabled()` turns authentication off. It reads no credential and
verifies nothing, so `From` of every scheme returns false, even for a request
that carries one. It is how an OpenAPI operation, the OpenAPI document or a
gRPC method is made public when the rest of the API or the Service requires
authentication, and it stands alone: beside another requirement in a list,
or inside an `All`, it is a mistake.

It differs from `Anonymous` in what it reads:

| | `auth.Anonymous()` | `auth.Disabled()` |
|---|---|---|
| a request without credentials | served, with no principal | served, with no principal |
| a request with a valid credential | served with its principal by an earlier requirement of the list, else with none | served, with no principal |
| a request with a credential the list rejects | refused: a 401 | served, with no principal |
| verifies credentials | every one the request carries | none |
| beside other requirements | yes; it is usually last | never: it stands alone |
| in the OpenAPI document | `{}` among the requirements | `security: []` |

An empty `openapi.Security()`, `grpcauth.Default()` or `grpcauth.Method(m)` is
a mistake rather than "public", so that every open path says
`auth.Disabled()` where a review sees it. `auth.NewHandler` doesn't take
`Disabled`: a handler that authenticates no one is the handler itself, served
without `NewHandler`.

## The three places

### `openapi.Security`

An API served with [`openapi/v3`](/humus/pr-preview/pr-1096/openapi/) takes its
requirements as `openapi.Security(reqs...)`. Given to `NewAPI`, it is every
operation's, and the document's. Given to an operation or to `Document`, it
replaces the API's, whatever the API's was. An API without `Security`
authenticates nothing.

This carts API requires a JWT bearer token of every operation, the scope
`carts:write` to delete a cart, and nothing of the greeting, whose
authentication is optional. Its document is public:

```go
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
```

Its handlers read the principal with `From`. On the greeting, `From` returns
false for a request served anonymously:

```go
func (c carts) getCart(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (cartBody, error) {
	claims, _ := c.bearer.From(ctx) // always there: the API's Security requires bearer
	return cartBody{Owner: claims.Subject}, nil
}
```

```go
func (c carts) greet(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (greetingBody, error) {
	claims, ok := c.bearer.From(ctx) // false for a request served anonymously
	if !ok {
		return greetingBody{Message: "hello, stranger"}, nil
	}
	return greetingBody{Message: "hello, " + claims.Subject}, nil
}
```

An operation with requirements authenticates the request before anything
else: before it decodes a parameter, checks the `Content-Type` or reads the
body. So a client that isn't authenticated gets a 401, never a 400 that says
which parameter it got wrong, and its body is never read. A request that
`Unmatched` answers matched no operation, and isn't authenticated.

The document lists what the API requires. The `Security` given to `NewAPI` is
its top-level `security`, an operation's own `Security` is the operation's
`security`, `[]` for `auth.Disabled()`, and `components.securitySchemes`
holds every scheme, keyed by its name. Each operation with requirements lists
`RequestErrors`' answer at 401, and at 403 when one of them names a scope.
[Serving an API with OpenAPI](/humus/pr-preview/pr-1096/openapi/#the-document)
describes the rest of the document.

The document is secured like the API: a request for it must meet the API's
`Security`, unless `Document` is given its own. A team that keeps an
internal API's document open for its tooling says so in one line,
`openapi.Security(auth.Disabled())`, as the carts API does. `NewAPI` reports
every mistake in a requirement in its one error, the ones `auth.NewHandler`
reports included.

### `auth.NewHandler`

[`auth.NewHandler`](https://pkg.go.dev/github.com/z5labs/humus/auth#NewHandler)
serves a plain `http.Handler`, such as the end user's mux or one route of it,
only to the requests that meet one of its requirements, and answers every
other with the end user's
[`Answers`](https://pkg.go.dev/github.com/z5labs/humus/auth#Answers). The
admin API above wraps one handler twice, once per method, and mounts both on
its mux:

```go
answers := auth.Answers{
	Unauthenticated: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sign in", http.StatusUnauthorized)
	}),
	Forbidden: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "you need the admin role", http.StatusForbidden)
	}),
	Failed: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "try again later", http.StatusServiceUnavailable)
	}),
}
```

```go
mux := http.NewServeMux()
mux.Handle("GET /admin/whoami", reads)
mux.Handle("POST /admin/whoami", writes)
mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "ok")
})

settings := httpserver.DefaultSettings()
settings.Addr = ":8443"
settings.TLS = tlsConfig // with ClientAuth tls.VerifyClientCertIfGiven, and ClientCAs
svc, err := httpserver.NewService(s, settings, mux)
```

`NewHandler` returns one error that names every mistake: a nil handler or
answer, no requirement, `Disabled`, the zero `Scheme` or `Requirement`, a nil
verify function, two different schemes with one name, and the rest its
documentation lists.

### `grpcauth.Interceptors`

The `grpc` module's
[`grpcauth`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpcauth)
package gives each method of a `grpcserver.Service` its requirements, by the
method's full name, `/package.Service/Method`, for which protoc-gen-go-grpc
generates a constant. `grpcauth.Interceptors` returns a unary and a stream
interceptor, which the end user passes to `grpcserver.NewService`:

```go
bearer := auth.Bearer("bearer", tokens.Verify).Format("JWT")

unary, stream, err := grpcauth.Interceptors(s,
	grpcauth.Default(auth.Require(bearer)),
	grpcauth.Method(testpb.TestService_StreamingOutputCall_FullMethodName, auth.RequireScopes(bearer, "greetings:write")),
	grpcauth.Method(healthpb.Health_Check_FullMethodName, auth.Disabled()), // the kubelet presents no credentials
	grpcauth.Method(healthpb.Health_Watch_FullMethodName, auth.Disabled()),
	grpcauth.Method(healthpb.Health_List_FullMethodName, auth.Disabled()),
)
if err != nil {
	return nil, err
}
svc, err := grpcserver.NewService(s, grpcserver.Settings{Addr: ":9090", TLS: tlsConfig}, unary, stream)
```

- `grpcauth.Default` is the requirement of every method no `Method` names,
  and it is required, because there is no document to show which methods are
  open. A method added to the Service later, and server reflection if the
  end user registers it, is covered by it.
  `grpcauth.Default(auth.Disabled())` makes "public unless listed" an
  explicit choice, with each secured method listed by `Method`.
- `grpcauth.Method` is one method's requirement, which replaces the
  Default's.

gRPC has no server option that is a unary and a stream interceptor at once,
so `Interceptors` returns both, and dropping one is a visible `_` rather than
a streaming method left open. They run inside `grpcserver`'s own
interceptors: after `otelgrpc` has started the RPC's span, and inside its
panic recovery. A handler reads its principal with `From`, from the RPC's
context:

```go
func (g greeterService) UnaryCall(ctx context.Context, _ *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	claims, _ := g.bearer.From(ctx) // always there: the Default requires bearer
	return &testpb.SimpleResponse{Username: claims.Subject}, nil
}
```

## The answers

Every answer's body is the end user's: `auth` writes no body of its own, and
**no verifier's text reaches the client.** A 401 doesn't say "token expired
at 10:04" or "no user alice@example.com". A verifier's reason is the end
user's to log, or to put on the span, in its verify function.

| Outcome | `openapi.Security` | `auth.NewHandler` | `grpcauth` |
|---|---|---|---|
| no requirement authenticated the request | `RequestErrors`, with a `RequestError` of `Status` 401 and `Err` `auth.ErrUnauthenticated` | `Answers.Unauthenticated` | `codes.Unauthenticated`, "unauthenticated" |
| authenticated without a scope it needs | `RequestErrors`, with a `RequestError` of `Status` 403 and `Err` `auth.ErrForbidden` | `Answers.Forbidden` | `codes.PermissionDenied`, "forbidden" |
| a verifier's other error | the first `Error` that matches it, else the catch-all | `Answers.Failed` | the error's own gRPC status, else `codes.Internal`, "authentication failed" |
| a verifier's panic | the catch-all, recorded as `operation panicked` | passes to `httpserver`, which records it and aborts the response | `grpcserver`'s recovery: `codes.Internal` |

With `problem.FromRequestError` as its wrap, `RequestErrors` answers a 401 as
a problem detail whose detail is the sentinel's text:

```json
{"type":"about:blank","title":"Unauthorized","status":401,"detail":"unauthenticated"}
```

### A verifier's other error is the server's

A verifier's error that doesn't wrap `auth.ErrUnauthenticated`, such as a
directory that is down or a key set that can't be fetched, is never answered
as a 401: a 401 tells a client its credential is bad, and a client that
believes it drops a valid token, or signs its user out. It is the server's
error, and answered as one:

- **In `openapi`,** as a handler's error is: by the first `Error` that
  matches it, with `errors.As`, such as an `Error` at 503, which clients
  retry, else by the catch-all, which records it as `request failed`. An
  `Error` at 401, or at 403 on an operation whose requirements name a scope,
  is a `NewAPI` error.
- **In `auth.NewHandler`,** it is recorded, then answered by
  `Answers.Failed`.
- **In `grpcauth`,** it is recorded, and the RPC ends with the error's own
  gRPC status if it has one, so a verifier that returns
  `status.Error(codes.Unavailable, …)` gets `Unavailable`, which clients
  retry. Otherwise it ends with `codes.Internal`, "authentication failed".

### `WWW-Authenticate`

A 401 carries one challenge for each scheme of the list that HTTP
authentication defines, in order and without repeats, as RFC 9110 has it:

- `Bearer`, for `Bearer`, `OAuth2` and `OpenIDConnect` together, or
  `Bearer error="invalid_token"` when one of them rejected the request's
  token, as RFC 6750 has it;
- `Basic realm="<realm>", charset="UTF-8"`, as RFC 7617 has it.

A 403 carries `Bearer error="insufficient_scope", scope="<the scopes>"` when a
bearer scheme lacked them, and nothing otherwise. An API key and mutual TLS
have no HTTP challenge, so they add none, and a 401 for a list of only those
carries none. A failed verifier gets no challenge. `openapi` and
`auth.NewHandler` set the header before the end user's answer runs, as
`openapi` sets `Allow` before `MethodNotAllowed`, so the answer can read it.

## Probes and other open paths

Kubernetes' probes present no credentials, so the path they reach must stay
open. How it stays open depends on where the probes are served:

- **A probes server,** `httphealth.NewService` or `grpchealth.NewService` on a
  port of its own, has no authentication, and needs nothing.
- **HTTP probes attached to the end user's mux** are open, because nothing
  wraps them. `openapi.Security` covers the API's operations, and
  `auth.NewHandler` only the handler it is given; the mux beside them, and
  `httphealth.NewHandler` on it, are the end user's. The carts API mounts
  the API and its probes side by side:

  ```go
  // The probes are beside the API on the mux, and nothing wraps
  // them, so they stay open for the kubelet.
  hs := health.NewService()
  mux := http.NewServeMux()
  mux.Handle("/", api)
  mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))
  ```

  Wrapping the whole mux in `auth.NewHandler` would close the probes too.
  Wrap the API's routes, as the admin API does, and leave the probes beside
  them.
- **gRPC health registered on the end user's Service,** with
  `grpchealth.NewServer`, passes through `grpcauth` like every RPC.
  `grpcauth` exempts no method on its own, because an exemption the end user
  can't see is a hole. So the end user opens Health's `Check`, `Watch` and
  `List` by name, with `auth.Disabled()`, as the rules in
  [`grpcauth.Interceptors`](#grpcauthinterceptors) do. If they don't, the
  kubelet's probes get `Unauthenticated`, and the pod never turns ready: that
  fails at the first deploy, never silently.

[Health](/humus/pr-preview/pr-1096/health/#exposing-the-probes) shows both wirings of
each protocol, and which fits the pod.

## Verifying JWTs

[`auth/jwt`](https://pkg.go.dev/github.com/z5labs/humus/auth/jwt) verifies
JWT bearer tokens and decodes each one's claims into the end user's own type.
A [`Verifier`](https://pkg.go.dev/github.com/z5labs/humus/auth/jwt#Verifier)
gets its keys from a
[`KeySet`](https://pkg.go.dev/github.com/z5labs/humus/auth/jwt#KeySet), and
its `Verify` is the verify function of a `Bearer`, `OAuth2` or
`OpenIDConnect` scheme. For each token, `Verify`:

1. parses it as a compact JWS signed with one of `Settings.Algorithms`, so
   `none` and HMAC are rejected;
2. requires a `kid` in its header, which the keys held must have;
3. verifies its signature, and decodes its payload into the registered
   claims and into the end user's type, which the package reads nothing
   from;
4. requires `iss` to be `Settings.Issuer`, `aud` to hold `Settings.Audience`,
   `exp` to be present and not past, and `nbf` and `iat` not to be in the
   future, all within `Settings.Leeway` of the clock.

Every rejection wraps `auth.ErrUnauthenticated`, so it is a 401.
[`jwt.Settings`](https://pkg.go.dev/github.com/z5labs/humus/auth/jwt#Settings)
are plain values the end user sources, such as with `config.Struct`, starting
from `jwt.DefaultSettings()`. `Issuer` and `Audience` have no default, and
each field has a `Check` function for its chain, as
[Configuring a humus service](/humus/pr-preview/pr-1096/configuration/) describes.

### On a `RemoteKeySet`

[`jwt.RemoteKeySet`](https://pkg.go.dev/github.com/z5labs/humus/auth/jwt#RemoteKeySet)
is the standard `KeySet`. It fetches the JSON Web Key Set an identity
provider publishes at its `jwks_uri`, with the end user's
[`httpclient`](/humus/pr-preview/pr-1096/http-client/) client, built for the key set's
origin. The carts API's `Init` builds all three, and nothing is fetched
until the first token:

```go
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
```

The URL must be https, with a host: a key set fetched in plain text would let
anyone on the path add a key and sign their own tokens, which the service
would accept. The client trusts the provider through its `TLS.RootCAs`, or
the system's roots when `RootCAs` is nil. Each fetch is a span of the
client's, measured by its metrics, bounded by its `Timeout` and retried as
its `Settings` say.

The Verifier, not the `KeySet`, decides when keys are got:

- with the first token, never at startup, so an identity provider that is
  down when the pod starts doesn't fail `Init`, and an API whose tokens
  can't be verified still serves its public operations;
- when a token names a `kid` the held keys lack, which is how a provider's
  key rotation reaches the service;
- when the held keys are older than `MaxKeyAge`, so a key the provider
  withdrew stops verifying.

The calls are at most one per `MinRefreshInterval`, whatever the tokens, so a
client that sends tokens with made-up `kid`s can't make the service call its
`KeySet` on every request. Concurrent requests share one call. When a call
fails while keys are held, the Verifier goes on verifying with them, past
`MaxKeyAge`, and logs `keys refresh failed`: a short outage of the key source
doesn't log every user out.

### On a `KeySet` of the end user's

Not every key infrastructure serves a JSON Web Key Set: keys come from secret
stores, files and providers' own APIs. A `KeySet` is one method, which
returns every [`jwt.Key`](https://pkg.go.dev/github.com/z5labs/humus/auth/jwt#Key)
the end user has, from wherever they keep them. This one reads PEM public
keys from the end user's secret store:

```go
// StoreKeys is a KeySet of the end user's, over their SecretStore. It is a
// fetch and nothing more: the Verifier holds the keys, and decides when to
// call it again.
type StoreKeys struct {
	Store SecretStore
}

// Keys returns every key in the store. A key that isn't PEM, or doesn't
// parse, is left out, so one bad entry doesn't fail the set.
func (k StoreKeys) Keys(ctx context.Context) ([]jwt.Key, error) {
	pems, err := k.Store.PublicKeys(ctx)
	if err != nil {
		return nil, err
	}
	var keys []jwt.Key
	for kid, b := range pems {
		block, _ := pem.Decode(b)
		if block == nil {
			continue
		}
		public, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			continue
		}
		keys = append(keys, jwt.Key{ID: kid, Public: public})
	}
	return keys, nil
}
```

A `KeySet` is a fetch and nothing more. The Verifier holds the keys and
decides when to call it, so a `KeySet` needs no cache of its own, and every
rule above holds whatever the source. A key's `Public` is an
`*rsa.PublicKey`, an `*ecdsa.PublicKey` or an `ed25519.PublicKey`; a key
whose ID is empty, or whose `Public` is anything else, a private key among
them, is skipped.

**A `KeySet` decides which tokens the service accepts:** whoever can add a key
to it can sign tokens the service serves. So it is the end user's to secure,
as a key set fetched over https is secured by its TLS: its source is reached
over an authenticated channel, and holds only the identity provider's public
keys.

### Answering a `*jwt.KeySetError`

When the Verifier has no key to verify a token with because its `KeySet`
failed, `Verify`'s error is a
[`*jwt.KeySetError`](https://pkg.go.dev/github.com/z5labs/humus/auth/jwt#KeySetError),
which wraps the `KeySet`'s error. That happens when no keys have ever been
got and the call fails, and when a token names a `kid` the held keys lack and
the call for it fails. The `kid` may be a key the provider rotated in, which
the service couldn't get, so **the token may be valid:** a `*KeySetError`
never wraps `auth.ErrUnauthenticated`, and is the server's error.

The end user answers it, such as with a 503, so clients retry. In `openapi`,
an `Error` answers it, with no wrapping in a verify function, as the carts
API's `NewAPI` does:

```go
// A token the Verifier had no keys for, because the key set is
// unreachable, is the server's outage, which clients retry.
openapi.Error[*jwt.KeySetError](http.StatusServiceUnavailable, keysUnavailable),
```

Its encoder answers every one with the same problem detail, which keeps the
`KeySet`'s error, and with it the provider's address, out of the body:

```go
func (e unavailable) Encode(*jwt.KeySetError) (io.WriterTo, error) {
	return e.Encoder.Encode(problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusServiceUnavailable),
		Status: http.StatusServiceUnavailable,
		Detail: "The server can't verify tokens right now. Try again later.",
	})
}
```

With `auth.NewHandler`, `Answers.Failed` answers it, as any verifier's other
error. Over gRPC, a verify function that turns it into
`status.Error(codes.Unavailable, …)`, found with `errors.As`, gets
`Unavailable`, which `grpcclient`'s default classification retries. Without
any of these, it is answered as a verifier's other error always is: the
catch-all's 500, `Answers.Failed`, or `Internal`.

## Telemetry

None of the packages reads or sets OpenTelemetry's globals, and none creates
an instrument of its own.

**A refusal isn't recorded.** A 401 or 403, `Unauthenticated` or
`PermissionDenied`, is the client's mistake. `otelhttp` and `otelgrpc` already
record its status on the request's span and in `http.server.request.duration`
or `rpc.server.call.duration`, so a credential-stuffing attempt shows as a
rise in 401s, with no log line per attempt.

**A verifier's other error is recorded once,** as an exception event on the
request's span and as one `ERROR` record in the request's context, so the
record carries the span's trace and span IDs:

| Where | Record | Logger | Attributes |
|---|---|---|---|
| `auth.NewHandler` | `authentication failed` | `github.com/z5labs/humus/auth` | `humus.auth.scheme`, `error.type`, `exception.message`, `http.request.method` |
| `grpcauth` | `authentication failed` | `github.com/z5labs/humus/grpc/grpcauth` | `humus.auth.scheme`, `error.type`, `exception.message`, `rpc.method` |
| `openapi.Security` | the catch-all's `request failed`; an `Error`'s answer isn't logged | `github.com/z5labs/humus/openapi/v3` | as for any handler's error, on [the OpenAPI page](/humus/pr-preview/pr-1096/openapi/#telemetry) |

`humus.auth.scheme` is the name of the scheme whose verifier failed. It
starts with `humus.`, as
[every name humus defines](/humus/pr-preview/pr-1096/telemetry/#the-humus-prefix)
does. When the client has gone away, the error goes on the span alone.

`auth/jwt` logs one record, `keys refresh failed`, at `WARN`, under the
logger name `github.com/z5labs/humus/auth/jwt`, with `error.type` and
`exception.message`, for a refresh that failed while keys were held. A
`*KeySetError` it returns is recorded by whichever of the three places
answers it, as any verifier's other error is.

**No principal is recorded.** humus puts no `user.id`, subject or key on a
span or a record, since that is personal data in most services. A verify
function that wants it on the span adds it, from
`trace.SpanFromContext(ctx)`.

## Testing

### A verify function

A test of a verify function gives the Verifier a `KeySet` of its own, in
memory, with no identity provider to serve. It generates a key with
`crypto/rand`, signs its tokens with it, and runs in a `testing/synctest`
bubble, so token times, `MinRefreshInterval` and `MaxKeyAge` take no real
time. `auth/jwt`'s package example puts the public half of a key in an
in-memory secret store, behind the `StoreKeys` above:

```go
// The identity provider's key pair: the store holds the public half.
public, private, err := ed25519.GenerateKey(rand.Reader)
if err != nil {
	panic(err)
}
der, err := x509.MarshalPKIXPublicKey(public)
if err != nil {
	panic(err)
}
store := memoryStore{"2026-10": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})}
```

and signs a token with the private half, under the same `kid`:

```go
// A token the identity provider signed.
signer, err := jose.NewSigner(jose.SigningKey{
	Algorithm: jose.EdDSA,
	Key:       jose.JSONWebKey{Key: private, KeyID: "2026-10"},
}, (&jose.SignerOptions{}).WithType("JWT"))
if err != nil {
	panic(err)
}
now := time.Now()
token, err := josejwt.Signed(signer).Claims(josejwt.Claims{
	Issuer:   "https://idp.example.com",
	Audience: josejwt.Audience{"carts"},
	Subject:  "alice",
	IssuedAt: josejwt.NewNumericDate(now),
	Expiry:   josejwt.NewNumericDate(now.Add(5 * time.Minute)),
}).Claims(map[string]any{"scope": "carts:read"}).Serialize()
if err != nil {
	panic(err)
}
```

A store that fails, with no keys held, shows a `*KeySetError`, and a `KeySet`
that counts its calls shows when the Verifier got keys.

### An App on a key set served on a humustest Host

A test of the end user's whole App runs it with `humustest`, as
[Testing a humus App](/humus/pr-preview/pr-1096/testing/) describes, and is its
identity provider too. It serves a JSON Web Key Set from a handler of its
own, with `httpserver`, on the same
[humustest Host](/humus/pr-preview/pr-1096/testing/#the-network), over TLS with a
certificate from a `humustest.CA`, and the App reaches the key set through its
own settings, as it does in production, trusting the CA through `RootCAs`.
The handler serves the public half of a key the test generates:

```go
func (p *identityProvider) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	down := p.down
	p.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: p.key.Public(), KeyID: p.kid, Algorithm: string(jose.ES256), Use: "sig"},
	}}
	w.Header().Set("Content-Type", "application/jwk-set+json")
	_ = json.NewEncoder(w).Encode(set)
}
```

and is served on the Host as an App of the test's own:

```go
cert := ca.Issue(t, "idp.internal")
proc, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings := httpserver.DefaultSettings()
	settings.Addr = ":443"
	settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
	svc, err := httpserver.NewService(s, settings, p)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
select {
case <-h.Listening("idp.internal:443"):
case st := <-status:
	t.Fatalf("the identity provider exited before it listened: %v", st.Cause())
}
```

The end user's App is started on the same Host, with the provider's key set
URL and the CA's pool:

```go
tokens := jwt.DefaultSettings()
tokens.Issuer = testIssuer
tokens.Audience = testAudience
keySet, err := url.Parse("https://idp.internal/.well-known/jwks.json")
require.NoError(t, err)
idp := IdP{KeySet: keySet, Roots: ca.Pool(), Tokens: tokens}
tlsConfig := &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}
```

The test then sends the tokens its provider signs, with the scopes each case
needs, and checks each answer: the 401 and its challenge, the 403 and its
scope, the anonymous greeting, the open document and probes. Inside the
bubble, an outage of the provider and the `MinRefreshInterval` after it take
no real time:

```go
// With the key set down and no keys held, a token may be valid: a
// 503, never a 401.
r := s.get("/carts/mine", bearer(reader))
require.Equal(t, http.StatusServiceUnavailable, r.status)
require.Empty(t, r.challenges())
require.JSONEq(t, `{"type":"about:blank","title":"Service Unavailable","status":503,"detail":"The server can't verify tokens right now. Try again later."}`, r.body)

// The key set is back, and called again once MinRefreshInterval
// has passed.
idp.setDown(false)
time.Sleep(time.Minute)
```

```go
r = s.get("/carts/mine", bearer(reader), method(http.MethodDelete))
require.Equal(t, http.StatusForbidden, r.status)
require.Equal(t, []string{`Bearer error="insufficient_scope", scope="carts:write"`}, r.challenges())
```

The shoppingcart at `example/shoppingcart` does the same, on the humustest
Host in its unit tests and on the OS Host beside its Postgres container in
its container test; [Serving an API with OpenAPI](/humus/pr-preview/pr-1096/openapi/#the-shoppingcart)
describes it. A test of an `auth.NewHandler` or a `grpcauth` Service needs no
identity provider when its schemes aren't JWTs: the admin API's test keeps
its directory in memory, with passwords from `crypto/rand`, and presents a
client certificate issued by its `humustest.CA`.

## Migration from REST security

`auth` replaces the security of `rest.Run` from before the reset, which the
reset deleted.

| Before the reset | With `auth` |
|---|---|
| `rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", verifier))` | `bearer := auth.Bearer("jwt", verify).Format("JWT")`, and `openapi.Security(auth.Require(bearer))` |
| `JWTVerifier.Verify(ctx, token) (context.Context, error)`, claims put in the context | `verify(ctx, token) (P, error)`, read with `bearer.From(ctx)` |
| a JWT verifier written by hand, on golang-jwt and a hand-loaded public key | `jwt.NewVerifier[Claims](s, settings, keys)`, with keys from a `jwt.RemoteKeySet` or a `KeySet` of the end user's, held and rotated |
| `rest.APIKey("api-key")` on a header, query parameter or cookie, checked in the handler | `auth.APIKeyHeader`, `APIKeyQuery` or `APIKeyCookie`, verified before the handler |
| `rest.BasicAuth("basic")`, documented as an API key, decoded in the handler | `auth.Basic(name, realm, verify)`, documented as `http`/`basic` |
| `rest.OAuth2("oauth2")`, with no flows | `auth.OAuth2(name, auth.Flows{…}, verify)` |
| `rest.OpenIDConnect(name, url)`, always named `openid-connect` | `auth.OpenIDConnect(name, url, verify)` |
| `rest.MutualTLS`, which panicked | `auth.MutualTLS(name, verify)` |
| a `requireRole` transform | `auth.RequireScopes(bearer, "admin")`, with the principal's `HasScope` |
| `JWTAuth` without `Required()`, for optional authentication | `openapi.Security(auth.Require(bearer), auth.Anonymous())` |
| a missing or malformed header: 400 | 401, with a challenge |
| the verifier's error: 401 | wrapping `auth.ErrUnauthenticated`: 401; any other: the server's |
| a gRPC interceptor of the end user's own | `grpcauth.Interceptors` |
