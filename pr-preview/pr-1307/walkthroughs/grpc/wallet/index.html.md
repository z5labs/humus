# Wallet

The wallet is two services, each a gRPC service served by `grpcserver`:

- **the wallet**, `wallet.v1.Wallet`, keeps each caller's own account, for a
  bearer token's subject;
- **the ledger**, `ledger.v1.Ledger`, keeps the accounts and the transfers
  between them, on PostgreSQL.

Sending money crosses both:

1. the app calls the wallet's `Send`, with a bearer token whose scopes hold
   `wallet:write`;
2. the wallet verifies the token against the key set it fetches over HTTPS;
3. the wallet calls the ledger's `Transfer`, from the token's subject's
   account, over mTLS;
4. the ledger posts the transfer in one PostgreSQL transaction.

```text
app ──gRPC, TLS + bearer──▶ wallet (grpcserver) ──gRPC, mTLS──▶ ledger (grpcserver)
                               │                                   │
                               ▼                                   ▼
                      the key set, over HTTPS              PostgreSQL: ledger
```

A watch of an account crosses them the same way: the wallet relays the
ledger's, and opens it again when the ledger's pod goes away.

## The files it ends with

Its code is a module of its own, at
[`example/grpc/wallet`](https://github.com/z5labs/humus/tree/main/example/grpc/wallet):

```text
example/grpc/wallet/
├── main.go                 the wallet: grpcserver.Main(app.Init, ...)
├── app/                    the wallet's Init, settings, key set, tokens, grpcauth rules, trust, probes, BuildTelemetry and Faults
├── server/                 the wallet.v1.Wallet service, on the ledger's client
├── ledger/
│   ├── main.go             the ledger: grpcserver.Main(app.Init, ...)
│   ├── app/                its Init, settings, grpcauth rules, trust, probes, BuildTelemetry and Faults
│   ├── server/             the ledger.v1.Ledger service, on a Store
│   └── store/              the Store on PostgreSQL, its injection point, and schema.sql
├── proto/                  wallet/v1/wallet.proto, ledger/v1/ledger.proto, buf.yaml and buf.gen.yaml
├── walletpb/               wallet.v1's generated code, checked in
├── ledgerpb/               ledger.v1's generated code, checked in
├── cmd/walletctl/          a CLI that makes the key set, signs tokens and calls the wallet, to try it by hand
├── internal/certfile/      certificates read from files, and read again when they're renewed
├── cases/                  the test cases the in-process tests, the container tests and kind share
├── functional/             the cases against kind
├── deploy/                 the base, the kind overlay and up.sh, the alerts and the dashboard
└── Dockerfile              one image, with the wallet and the ledger
```

These pages build the contract, the ledger's accounts and its server, then
the wallet's calls to the ledger and its server, then each service's `Init`
and `main`. Then they test both services, deploy them, and observe them,
and end with the checklist they meet.
