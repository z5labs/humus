# Serving the wallet

The wallet's `server` package is `wallet.v1.Wallet`, on the ledger's
generated client.

## The methods

| Method | Needs | Answers |
|---|---|---|
| `OpenAccount` | `wallet:write` | the caller's account, opened in the ledger |
| `GetAccount` | `wallet:read` | the caller's account |
| `Send` | `wallet:write` | the transfer from the caller's account, to the account it names |
| `WatchAccount` | `wallet:read` | the caller's account, as the ledger's watch sends it |

## The claims and the scopes

A token's claims are its subject and its scopes:

```go
const (
	ScopeRead  = "wallet:read"
	ScopeWrite = "wallet:write"
)
```

```go
type Claims struct {
	Subject string `json:"sub"`
	Scope   string `json:"scope"`
}

func (c Claims) HasScope(scope string) bool {
	return slices.Contains(strings.Fields(c.Scope), scope)
}
```

## The fetched key set

The wallet has one `httpclient` Client for the key set, from
`TOKEN_KEYS_CA_FILE` and the defaults:

```go
keys := httpclient.DefaultSettings()
keys.TLS = &tls.Config{
	RootCAs:    r.Get(file("TOKEN_KEYS_CA_FILE").Parse(certPool)),
	MinVersion: tls.VersionTLS12,
}
keys.Timeout = r.Get(env("TOKEN_KEYS_TIMEOUT").Parse(time.ParseDuration).Default(keys.Timeout).Validate(httpclient.CheckTimeout))
keys.MaxAttempts = r.Get(env("TOKEN_KEYS_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(keys.MaxAttempts).Validate(httpclient.CheckMaxAttempts))
```

Its base URL is `TOKEN_KEYS_URL`'s origin:

```go
got.KeySet.Client.BaseURL = &url.URL{Scheme: got.KeySet.URL.Scheme, Host: got.KeySet.URL.Host}
```

`Init` passes the Client to `jwt.NewRemoteKeySet`, and verifies tokens
against it:

```go
keysClient, err := httpclient.NewClient(s, set.KeySet.Client)
if err != nil {
	return nil, err
}
keys, err := jwt.NewRemoteKeySet(set.KeySet.URL, keysClient)
if err != nil {
	return nil, err
}
tokens, err := jwt.NewVerifier[server.Claims](s, set.JWT, keys)
if err != nil {
	return nil, err
}
```

The Verifier fetches the key set for a `kid` it lacks, and after
`MaxKeyAge`, so a rotated key reaches the wallet without a restart.

## A key set that can't be fetched

A token the wallet can't verify because the key set can't be fetched is
`Unavailable`, never `Unauthenticated`:

```go
func keysUnavailable(verify func(context.Context, string) (server.Claims, error)) func(context.Context, string) (server.Claims, error) {
	return func(ctx context.Context, token string) (server.Claims, error) {
		claims, err := verify(ctx, token)
		if _, ok := errors.AsType[*jwt.KeySetError](err); ok {
			return server.Claims{}, unavailableKeys()
		}
		return claims, err
	}
}

func unavailableKeys() error {
	st := status.New(codes.Unavailable, "the wallet can't verify tokens right now; try again later")
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: reasonKeysUnavailable,
		Domain: walletpb.Wallet_ServiceDesc.ServiceName,
	})
	if err != nil {
		return st.Err()
	}
	return detailed.Err()
}
```

```go
const reasonKeysUnavailable = "KEYS_UNAVAILABLE"
```

`grpcauth` passes the status on. The key set isn't part of readiness, so an
outage of it fails only a token whose key the wallet doesn't hold.

## Bearer tokens

`grpcauth` checks the token on every method. Every method needs
`wallet:read`, but `OpenAccount` and `Send`, which need `wallet:write`:

```go
bearer := auth.Bearer("bearer", keysUnavailable(tokens.Verify))
write := auth.RequireScopes(bearer, server.ScopeWrite)
unary, stream, err := grpcauth.Interceptors(s,
	grpcauth.Default(auth.RequireScopes(bearer, server.ScopeRead)),
	grpcauth.Method(walletpb.Wallet_OpenAccount_FullMethodName, write),
	grpcauth.Method(walletpb.Wallet_Send_FullMethodName, write),
)
if err != nil {
	return nil, err
}
```

## The subject's account

A token's subject is the only account it can use. A handler reads it with
`bearer.From`:

```go
func (s *Server) subject(ctx context.Context) (string, error) {
	claims, ok := s.bearer.From(ctx)
	if !ok || claims.Subject == "" {
		return "", status.Error(codes.Unauthenticated, "the request has no bearer token with a subject")
	}
	return claims.Subject, nil
}
```

The account's ID in the ledger is the subject:

```go
func (s *Server) OpenAccount(ctx context.Context, _ *walletpb.OpenAccountRequest) (*walletpb.OpenAccountResponse, error) {
	id, err := s.subject(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.ledger.OpenAccount(ctx, &ledgerpb.OpenAccountRequest{AccountId: id})
	if err != nil {
		return nil, s.answer(ctx, err)
	}
	return &walletpb.OpenAccountResponse{Account: account(resp.GetAccount())}, nil
}
```

## A send

`Send` requires a `request_id`, and sends the transfer from the subject's
account, with its ID made from both:

```go
if req.GetRequestId() == "" {
	return nil, withDetails(codes.InvalidArgument, "the request is missing a required field", &errdetails.BadRequest{
		FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "request_id", Description: "is required"}},
	})
}
resp, err := s.ledger.Transfer(ctx, &ledgerpb.TransferRequest{
	TransferId:    transferID(id, req.GetRequestId()),
	FromAccountId: id,
	ToAccountId:   req.GetToAccountId(),
	AmountCents:   req.GetAmountCents(),
})
if err != nil {
	return nil, s.answer(ctx, err)
}
return &walletpb.SendResponse{Transfer: transfer(resp.GetTransfer())}, nil
```

## Relaying the watch

`WatchAccount` opens the ledger's watch of the subject's account and sends
on what it sends:

```go
func (s *Server) relay(ctx context.Context, id string, stream grpc.ServerStreamingServer[walletpb.WatchAccountResponse], last **ledgerpb.Account) (received bool, err error) {
	watch, err := s.ledger.WatchAccount(ctx, &ledgerpb.WatchAccountRequest{AccountId: id})
	if err != nil {
		return false, err
	}
	for {
		resp, err := watch.Recv()
		if err != nil {
			return received, err
		}
		received = true
		if proto.Equal(resp.GetAccount(), *last) {
			continue
		}
		if err := stream.Send(&walletpb.WatchAccountResponse{Account: account(resp.GetAccount())}); err != nil {
			return received, &sendError{err: err}
		}
		*last = resp.GetAccount()
	}
}
```

A watch opened again sends the account at once, so `relay` skips an account
the caller already has.

When the ledger's watch ends with `Unavailable` after its first message,
the wallet opens it again. That is the ledger's shutdown, or a lost
connection:

```go
var last *ledgerpb.Account
for {
	received, err := s.relay(ctx, id, stream, &last)
	if sent, ok := errors.AsType[*sendError](err); ok {
		return sent.err
	}
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case received && status.Code(err) == codes.Unavailable && ctx.Err() == nil:
		s.reopened.Add(ctx, 1)
		continue
	}
	return s.answer(ctx, err)
}
```

When it ends before its first message, or any other way, the wallet ends
the caller's watch with the answer a unary method would get. So a rollout
of the ledger keeps every app's watch open, and a ledger that is down ends
it, rather than the wallet opening it again forever.

When the wallet shuts down, `grpcserver` ends the caller's watch with
`Unavailable`, and the app watches again.

Next: [Init](/humus/walkthroughs/grpc/wallet/init/)
