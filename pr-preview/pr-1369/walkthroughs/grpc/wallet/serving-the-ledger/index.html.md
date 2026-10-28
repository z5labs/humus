# Serving the ledger

The ledger's `server` package is `ledger.v1.Ledger`, on the Store.

## The methods

| Method | Answers |
|---|---|
| `OpenAccount` | the account, opened with a zero balance, or as it is if it was already open |
| `GetAccount` | the account, or `NotFound` |
| `Deposit` | the deposit, as a transfer from `external`. `NotFound` for no such account |
| `Transfer` | the transfer. `FailedPrecondition` when the source's balance is short, `NotFound` for either account, `InvalidArgument` for an amount that isn't positive or an account sent to itself, and `AlreadyExists` for a transfer's ID reused for another transfer |
| `WatchAccount` | the account at once, then again each time its balance changes. `NotFound` for no such account |

The server takes the Store as an interface, so its tests give it one of
their own:

```go
type Store interface {
	OpenAccount(ctx context.Context, id string) (store.Account, error)
	Account(ctx context.Context, id string) (store.Account, error)
	Deposit(ctx context.Context, transferID, accountID string, amountCents int64) (store.Transfer, error)
	Transfer(ctx context.Context, transferID, fromAccountID, toAccountID string, amountCents int64) (store.Transfer, error)
}
```

## A method

Each method checks its required fields, calls the Store, and turns its error
into a status:

```go
func (s *Server) GetAccount(ctx context.Context, req *ledgerpb.GetAccountRequest) (*ledgerpb.GetAccountResponse, error) {
	if err := required(field{"account_id", req.GetAccountId()}); err != nil {
		return nil, err
	}
	a, err := s.store.Account(ctx, req.GetAccountId())
	if err != nil {
		return nil, s.answer(ctx, err, nil)
	}
	return &ledgerpb.GetAccountResponse{Account: account(a)}, nil
}
```

A missing field is `InvalidArgument`, with a `google.rpc.BadRequest`
naming each one:

```go
func required(fields ...field) error {
	var violations []*errdetails.BadRequest_FieldViolation
	for _, f := range fields {
		if f.value == "" {
			violations = append(violations, &errdetails.BadRequest_FieldViolation{Field: f.name, Description: "is required"})
		}
	}
	if len(violations) == 0 {
		return nil
	}
	return withDetails(codes.InvalidArgument, "the request is missing a required field", &errdetails.BadRequest{FieldViolations: violations})
}
```

`Transfer` names the field each of the Store's invalid amounts and accounts
is about:

```go
t, err := s.store.Transfer(ctx, req.GetTransferId(), req.GetFromAccountId(), req.GetToAccountId(), req.GetAmountCents())
if err != nil {
	err = s.answer(ctx, err, []invalid{
		{store.ErrNotPositive, "amount_cents", "must be more than zero"},
		{store.ErrSameAccount, "to_account_id", "is the account the transfer is from"},
	})
```

## The statuses, with details

`answer` turns the Store's errors into statuses:

```go
func answer(err error, invalids []invalid) error {
	var short *store.ShortBalanceError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.As(err, &short):
		return withDetails(codes.FailedPrecondition, err.Error(), &errdetails.PreconditionFailure{
			Violations: []*errdetails.PreconditionFailure_Violation{{
				Type:        "BALANCE",
				Subject:     short.AccountID,
				Description: short.Error(),
			}},
		})
	case errors.Is(err, store.ErrReusedID):
		return status.Error(codes.AlreadyExists, err.Error())
	}
	for _, inv := range invalids {
		if errors.Is(err, inv.err) {
			return withDetails(codes.InvalidArgument, err.Error(), &errdetails.BadRequest{
				FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: inv.field, Description: inv.description}},
			})
		}
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case errors.Is(err, store.ErrUnavailable):
		return status.Error(codes.Unavailable, "the ledger's database can't be reached")
	}
	return status.Error(codes.Internal, "the ledger failed to answer the request")
}
```

| The Store's error | The status | Its details |
|---|---|---|
| `ErrNotFound` | `NotFound` | |
| `*ShortBalanceError` | `FailedPrecondition` | a `PreconditionFailure` naming the account |
| `ErrReusedID` | `AlreadyExists` | |
| `ErrNotPositive`, `ErrSameAccount` | `InvalidArgument` | a `BadRequest` naming the field |
| `ErrUnavailable` | `Unavailable` | |
| anything else | `Internal`, saying nothing of the error | |

The details are attached with `WithDetails`:

```go
func withDetails(code codes.Code, msg string, details ...protoadapt.MessageV1) error {
	st, err := status.New(code, msg).WithDetails(details...)
	if err != nil {
		return status.Error(code, msg)
	}
	return st.Err()
}
```

## The watch

`WatchAccount` sends the account at once, then reads it every
`WATCH_INTERVAL` and sends it when its balance has changed:

```go
sent, err := s.store.Account(ctx, req.GetAccountId())
if err != nil {
	return s.answer(ctx, err, nil)
}
if err := stream.Send(&ledgerpb.WatchAccountResponse{Account: account(sent)}); err != nil {
	return err
}

ticker := time.NewTicker(s.watchInterval)
defer ticker.Stop()
for {
	select {
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-ticker.C:
	}
	a, err := s.store.Account(ctx, req.GetAccountId())
	if err != nil {
		return s.answer(ctx, err, nil)
	}
	if a.BalanceCents == sent.BalanceCents {
		continue
	}
	if err := stream.Send(&ledgerpb.WatchAccountResponse{Account: account(a)}); err != nil {
		return err
	}
	sent = a
}
```

So a watch sends the balance it settles on, not each transfer.

## mTLS

The ledger's TLS asks for a client certificate, and verifies one against
`GRPC_CLIENT_CA_FILE`:

```go
set.GRPC.TLS = &tls.Config{
	GetCertificate: got.ServerTLS.GetCertificate,
	ClientAuth:     tls.VerifyClientCertIfGiven,
	ClientCAs:      got.ClientCAs,
	MinVersion:     tls.VersionTLS12,
}
```

It doesn't require one in the handshake, so a call without one reaches
`grpcauth` and gets an `Unauthenticated` that says why.

## `grpcauth`

Every method requires a certificate the internal CA issued:

```go
cert := auth.MutualTLS("mutualTLS", subject)
unary, stream, err := grpcauth.Interceptors(s, grpcauth.Default(auth.Require(cert)))
if err != nil {
	return nil, err
}
```

The scheme's principal is the subject of the chain's leaf:

```go
func subject(_ context.Context, chains [][]*x509.Certificate) (pkix.Name, error) {
	if len(chains) == 0 || len(chains[0]) == 0 {
		return pkix.Name{}, fmt.Errorf("no verified chain: %w", auth.ErrUnauthenticated)
	}
	return chains[0][0].Subject, nil
}
```

The interceptors are options to `grpcserver.NewService`, and the server is
registered on the Service it returns:

```go
ledgerpb.RegisterLedgerServer(svc, ledger)
```

## The injection point

The Store evaluates its injection point, `postgresql:ledger`, before each
transaction:

```go
const point = "postgresql:ledger"
```

```go
func (s *Store) inject(ctx context.Context) error {
	f := s.faults.Eval(ctx, point)
	if err := f.Wait(ctx); err != nil {
		return &fault.InjectedError{Point: point, Err: err}
	}
	if f.Abort {
		return &fault.InjectedError{Point: point}
	}
	return nil
}
```

`unavailable` makes an abort `ErrUnavailable`, as an unreachable database
is, so the method answers `Unavailable`.

## The probes

The probes are `grpchealth.NewService`, on a plaintext port of their own,
`PROBES_ADDR`, because the kubelet presents no certificate and its `grpc`
probes don't use TLS on most clusters:

```go
probes, err := grpchealth.NewService(s, set.Probes, health.Readiness(newReached(ping)))
if err != nil {
	return nil, err
}
return []humus.Service{svc, probes}, nil
```

The ledger is ready once it has reached PostgreSQL, and stays ready through
an outage after that, so the wallet gets the ledger's own `Unavailable`
rather than no ledger at all:

```go
func (r *reached) Healthy(ctx context.Context) (bool, error) {
	if r.ok.Load() {
		return true, nil
	}
	select {
	case r.turn <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-r.turn }()
	if r.ok.Load() {
		return true, nil
	}
	if err := r.check(ctx); err != nil {
		return false, err
	}
	r.ok.Store(true)
	return true, nil
}
```

One probe pings at a time, so a slow database isn't pinged by every probe
at once.

Next: [Calling the ledger](/humus/pr-preview/pr-1369/walkthroughs/grpc/wallet/calling-the-ledger/)
