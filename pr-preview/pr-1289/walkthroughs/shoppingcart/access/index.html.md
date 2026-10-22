# Who may do what

Every operation needs a JWT bearer token from the identity provider.
Changing a cart needs the scope `carts:write` too, and a cart is only ever
its owner's.

## The claims

The claims the cart reads from a token are a type of its own. `HasScope`
is what `auth.RequireScopes` asks:

```go
const ScopeWrite = "carts:write"

type Claims struct {
	Subject string `json:"sub"`
	Scope   string `json:"scope"`
}

func (c Claims) HasScope(scope string) bool {
	return slices.Contains(strings.Fields(c.Scope), scope)
}
```

## Verifying tokens

`Init` builds an `httpclient` client for the key set's origin, a
`jwt.RemoteKeySet` that fetches the key set with it, and a `Verifier` of
`Claims`:

```go
idp, err := httpclient.NewClient(s, settings.KeySet.Client)
if err != nil {
	return nil, err
}
keys, err := jwt.NewRemoteKeySet(settings.KeySet.URL, idp)
if err != nil {
	return nil, err
}
tokens, err := jwt.NewVerifier[endpoint.Claims](s, settings.JWT, keys)
if err != nil {
	return nil, err
}
```

The client's base URL is the key set URL's origin:

```go
read.KeySet.Client.BaseURL = &url.URL{Scheme: read.KeySet.URL.Scheme, Host: read.KeySet.URL.Host}
```

The `Verifier` checks the signature, the issuer, the audience, the
algorithm and the times, as `JWT_*` sets them.

## A token with no subject

A cart is its owner's, so a token with no `sub` is a 401:

```go
func withSubject(verify func(context.Context, string) (endpoint.Claims, error)) func(context.Context, string) (endpoint.Claims, error) {
	return func(ctx context.Context, token string) (endpoint.Claims, error) {
		claims, err := verify(ctx, token)
		if err != nil {
			return endpoint.Claims{}, err
		}
		if claims.Subject == "" {
			return endpoint.Claims{}, fmt.Errorf("the token has no sub: %w", auth.ErrUnauthenticated)
		}
		return claims, nil
	}
}
```

## A key set that is down

A key set that can't be fetched, before any key was, is a
`*jwt.KeySetError`. The token may be good, so it is a 503 `Unavailable`,
not a 401:

```go
func KeysUnavailable(verify func(context.Context, string) (Claims, error)) func(context.Context, string) (Claims, error) {
	return func(ctx context.Context, token string) (Claims, error) {
		claims, err := verify(ctx, token)
		if _, ok := errors.AsType[*jwt.KeySetError](err); ok {
			return Claims{}, unavailable("The server can't verify tokens right now. Try again later.")
		}
		return claims, err
	}
}
```

## The bearer scheme

The scheme wraps the verify function in both:

```go
bearer := auth.Bearer("bearer", endpoint.KeysUnavailable(withSubject(tokens.Verify))).
	Format("JWT").
	Description("A JWT from the shoppingcart's identity provider. Changing a cart needs the scope carts:write.")
```

## Requiring it

The API requires the scheme on every operation:

```go
openapi.Security(auth.Require(bearer)),
```

Every operation that changes a cart requires `carts:write` too:

```go
write := openapi.Security(auth.RequireScopes(c.Bearer, ScopeWrite))
```

The document is open to anyone:

```go
openapi.Document(openapi.Root().Lit("openapi.json"), openapi.Security(auth.Disabled())),
```

## A cart's owner

Each handler takes the owner from the claims the scheme verified:

```go
func (c Carts) owner(ctx context.Context) (string, error) {
	claims, ok := c.Bearer.From(ctx)
	if !ok || claims.Subject == "" {
		return "", errors.New("the request has no bearer token with a subject")
	}
	return claims.Subject, nil
}
```

A cart is created with its owner:

```go
err := s.pool.QueryRow(ctx,
	`INSERT INTO carts (owner) VALUES ($1) RETURNING id, created_at, updated_at`, owner,
).Scan(&c.CartID, &c.CreatedAt, &c.UpdatedAt)
```

And every query after that matches the owner as well as the ID:

```go
tag, err := s.pool.Exec(ctx, `DELETE FROM carts WHERE id = $1 AND owner = $2`, cartID, owner)
```

So another subject's cart is `ErrCartNotFound`, a 404, and the API never
says whose carts exist.

## The answers

| The request | The answer |
|---|---|
| no token, or one that doesn't verify | 401 |
| a token with no `sub` | 401 |
| a change without `carts:write` | 403 |
| another subject's cart | 404 |
| a key set that can't be fetched | 503 |

[Authentication and authorization](/humus/pr-preview/pr-1289/auth/) describes each
piece.
