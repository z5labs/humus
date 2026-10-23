# The contract

The contract is two protobuf packages under `proto/`, `ledger.v1` and
`wallet.v1`, generated into `ledgerpb` and `walletpb`.

## The ledger's service

`proto/ledger/v1/ledger.proto` declares five methods:

```protobuf
service Ledger {
  rpc OpenAccount(OpenAccountRequest) returns (OpenAccountResponse);
  rpc GetAccount(GetAccountRequest) returns (GetAccountResponse);
  rpc Deposit(DepositRequest) returns (DepositResponse);
  rpc Transfer(TransferRequest) returns (TransferResponse);
  rpc WatchAccount(WatchAccountRequest) returns (stream WatchAccountResponse);
}
```

An account is its ID, its balance, and when the balance last changed. A
transfer is its ID, the two accounts, the amount and when it was made.
Amounts are whole cents, in one currency:

```protobuf
message Account {
  string id = 1;
  int64 balance_cents = 2;
  google.protobuf.Timestamp balance_change_time = 3;
}

message Transfer {
  string id = 1;
  string from_account_id = 2;
  string to_account_id = 3;
  int64 amount_cents = 4;
  google.protobuf.Timestamp create_time = 5;
}
```

A transfer's request carries its ID, so a transfer sent twice is posted
once:

```protobuf
message TransferRequest {
  string transfer_id = 1;
  string from_account_id = 2;
  string to_account_id = 3;
  int64 amount_cents = 4;
}
```

A deposit's request is the same, with one account, since a deposit comes
from the ledger's own `external` account:

```protobuf
message DepositRequest {
  string transfer_id = 1;
  string account_id = 2;
  int64 amount_cents = 3;
}
```

## The wallet's service

`proto/wallet/v1/wallet.proto` declares four. Its `Account` and `Transfer`
have the ledger's fields:

```protobuf
service Wallet {
  rpc OpenAccount(OpenAccountRequest) returns (OpenAccountResponse);
  rpc GetAccount(GetAccountRequest) returns (GetAccountResponse);
  rpc Send(SendRequest) returns (SendResponse);
  rpc WatchAccount(WatchAccountRequest) returns (stream WatchAccountResponse);
}
```

No request names the caller's account, because the bearer token's subject
is the only account a caller can use:

```protobuf
message OpenAccountRequest {}
```

A send names its own ID, `request_id`, so a repeat by the app is the same
transfer:

```protobuf
message SendRequest {
  string request_id = 1;
  string to_account_id = 2;
  int64 amount_cents = 3;
}
```

## buf's rules

`proto/buf.yaml` lints with buf's `STANDARD` rules, and checks breaking
changes file by file:

```yaml
version: v2
lint:
  use:
    - STANDARD
  except:
    - SERVICE_SUFFIX
breaking:
  use:
    - FILE
```

`SERVICE_SUFFIX` is left out, since the services are `Ledger` and `Wallet`,
not `LedgerService` and `WalletService`.

## `buf generate`

`proto/buf.gen.yaml` runs protoc-gen-go and protoc-gen-go-grpc as `go tool`s,
so the module's `go.mod`, which lists both under `tool`, pins their versions:

```yaml
version: v2
plugins:
  - local: ["go", "tool", "protoc-gen-go"]
    out: ..
    opt: module=github.com/z5labs/humus/example/grpc/wallet
  - local: ["go", "tool", "protoc-gen-go-grpc"]
    out: ..
    opt: module=github.com/z5labs/humus/example/grpc/wallet
```

Run it in `proto/`:

```sh
cd example/grpc/wallet/proto
buf lint
buf generate
```

It writes `ledgerpb` and `walletpb`, from each file's `go_package`:

```protobuf
option go_package = "github.com/z5labs/humus/example/grpc/wallet/ledgerpb";
```

The generated code is checked in. CI runs `buf lint`, `buf breaking`
against main's contract, and `buf generate`, and fails when the generated
code differs from what is checked in:

```sh
buf breaking --against "$GITHUB_WORKSPACE/main/example/grpc/wallet/proto"
```

Next: [The ledger's accounts](/humus/pr-preview/pr-1308/walkthroughs/grpc/wallet/accounts/)
