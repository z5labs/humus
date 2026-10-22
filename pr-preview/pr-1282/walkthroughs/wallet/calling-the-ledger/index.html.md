# Calling the ledger

The wallet has one `grpcclient` Client for the ledger. Every method and
readiness go through it.

## The client's settings

The wallet's `readSettings` starts from `grpcclient.DefaultSettings()`, and
sets the ledger's address, the CA it trusts the ledger through, and the
client certificate it presents:

```go
ledger := grpcclient.DefaultSettings()
ledger.Addr = r.Get(env("LEDGER_ADDR").Validate(grpcclient.CheckAddr))
ledger.TLS = &tls.Config{
	RootCAs: r.Get(file("LEDGER_CA_FILE").Parse(certPool)),
	GetClientCertificate: r.Get(pemFile("LEDGER_TLS_PEM_FILE").Map(func(c *tls.Config) clientCertificate {
		return c.GetClientCertificate
	})),
	MinVersion: tls.VersionTLS12,
}
ledger.Timeout = r.Get(env("LEDGER_TIMEOUT").Parse(time.ParseDuration).Default(ledger.Timeout).Validate(grpcclient.CheckTimeout))
ledger.MaxAttempts = r.Get(env("LEDGER_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(ledger.MaxAttempts).Validate(grpcclient.CheckMaxAttempts))
ledger.ForwardScenario = true
```

```go
type (
	serverCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	clientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
)
```

## The client

`Init` builds the Client, and the wallet's server calls the ledger through
the generated client on it:

```go
ledger, err := grpcclient.NewClient(s, set.Ledger)
if err != nil {
	return nil, err
}
```

```go
wallet, err := server.New(ledgerpb.NewLedgerClient(ledger), bearer, s.Telemetry)
if err != nil {
	return nil, err
}
walletpb.RegisterWalletServer(svc, wallet)
```

A caller's deadline is the handler's, and grpc-go sends it on with the call
to the ledger, which the Client's `Timeout` bounds too.

## Retries and the breaker

The wallet keeps the default `Classify`, retry budget and breaker:

| The ledger's answer | Retried | To the breaker |
|---|---|---|
| `OK` | no | a success |
| `NotFound`, `FailedPrecondition`, `InvalidArgument`, `AlreadyExists` | no | a success |
| `Unavailable`, or a connection it can't make | yes | a failure |
| `Internal` | no | a failure |

The four codes about the request are the ledger working, so they count for
it, not against it.
[Calling gRPC services](/humus/pr-preview/pr-1282/grpc-client/#the-default) describes
the default.

## Why every transfer is safe to retry

The default retries every method on `Unavailable`, which may come after the
ledger acted. That is safe here: `OpenAccount` opens an account once, the
reads change nothing, and a transfer's ID is the wallet's, so the ledger
posts it once and answers a repeat as it was made.

The transfer's ID is the token's subject and the send's `request_id`
together:

```go
func transferID(subject, requestID string) string {
	return url.PathEscape(subject) + "/" + requestID
}
```

So a retry by the Client, or a repeat by the app, is the same transfer, and
no caller can reuse another's ID. The subject is escaped, so it holds no
`/`.

## The watch

The wallet opens the ledger's watch through the same Client. Opening it is
retried, and a watch that opened is never retried: the wallet opens it again
itself, as
[Serving the wallet](/humus/pr-preview/pr-1282/walkthroughs/wallet/serving-the-wallet/#relaying-the-watch)
shows. A watch that ends with `Unavailable` is ignored by the breaker.

## `ForwardScenario`

`ForwardScenario` sends a call's failure-injection scenario on to the
ledger, because the ledger is the walkthrough's own and reads the same
codec. So a scenario the wallet trusts can fail the ledger's
`postgresql:ledger` point too, and the ledger trusts it because the
wallet's client certificate verified.

## Readiness: `Connected`

The wallet's readiness is the Client's `Connected`:

```go
probes, err := grpchealth.NewService(s, set.Probes, health.Readiness(ledger.Connected()))
if err != nil {
	return nil, err
}
return []humus.Service{svc, probes}, nil
```

It latches:

- a release that can't reach the ledger never turns ready, so its rollout
  stops at its first pod while the old pods keep serving;
- an outage after that leaves every pod ready, so apps get the wallet's own
  `Unavailable` and its reason.

## The ledger's answers, as the wallet's

`answer` passes on what the ledger answers about the request, with its
details, and makes everything else `Unavailable`, never `Internal`:

```go
func answer(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr).Err()
	}
	if rejected, ok := errors.AsType[*circuit.RejectedError](err); ok {
		return withDetails(codes.Unavailable, "the ledger is shedding calls; try again later",
			errorInfo(reasonLedgerShed),
			&errdetails.RetryInfo{RetryDelay: durationpb.New(rejected.RetryAfter)},
		)
	}
	switch status.Code(err) {
	case codes.NotFound, codes.FailedPrecondition, codes.InvalidArgument, codes.AlreadyExists:
		return status.Convert(err).Err()
	}
	return withDetails(codes.Unavailable, "the ledger can't be reached", errorInfo(reasonLedgerUnavailable))
}
```

```go
const (
	reasonLedgerUnavailable = "LEDGER_UNAVAILABLE"
	reasonLedgerShed        = "LEDGER_SHED"
)
```

```go
func errorInfo(reason string) *errdetails.ErrorInfo {
	return &errdetails.ErrorInfo{Reason: reason, Domain: walletpb.Wallet_ServiceDesc.ServiceName}
}
```

| The ledger | The wallet answers |
|---|---|
| answers `NotFound`, `FailedPrecondition`, `InvalidArgument` or `AlreadyExists` | the same, with its details |
| is shed by the breaker | `Unavailable`, with an `ErrorInfo` of `LEDGER_SHED` and a `RetryInfo` from the rejection's `RetryAfter` |
| answers anything else, or can't be reached after the retries | `Unavailable`, with an `ErrorInfo` of `LEDGER_UNAVAILABLE` |
| is still answering when the caller cancels or its deadline passes | `Canceled` or `DeadlineExceeded` |

A `RetryInfo` tells the app when to try again.
