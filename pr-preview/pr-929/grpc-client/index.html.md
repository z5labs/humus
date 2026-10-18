# Calling gRPC services

The [`grpcclient`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpcclient)
package calls another service over gRPC. `grpcclient.NewClient` builds a
`Client` for one dependency in the end user's `Init`, from `Settings` the end
user sources. The `Client` is a `grpc.ClientConnInterface`, so generated code
takes it as it takes a `*grpc.ClientConn`. Its calls go through the Setup's
Host, are traced and measured with the Setup's telemetry, are retried as the
end user's `Classify` decides, and fail at once while a circuit breaker says
the dependency keeps failing.

The package is in the nested module `github.com/z5labs/humus/grpc`, next to
[`grpcserver`](/humus/pr-preview/pr-929/grpc/): `grpcserver` is the server half, and
`grpcclient` the client half. The `Client` dials only through the Setup's
Host, so a test runs the end user's whole App, its outbound calls included,
on humustest's network, as [Testing](#testing) shows.

The design behind everything on this page is in
[Proposal: outbound gRPC clients](https://github.com/z5labs/humus/discussions/763),
with its [revision 1](https://github.com/z5labs/humus/discussions/763#discussioncomment-19009949).

## A Client for each dependency

The end user's `Init` sources the dependency's `grpcclient.Settings`, builds
its `Client` with `NewClient`, and hands the `Client` to generated code. Here
the end user's service, served with `grpcserver`, calls `carts`:

```go
// Init is the end user's Init: it sources the carts dependency's Settings,
// builds its Client with NewClient, and hands the Client to generated code,
// in the end user's service, which it serves with grpcserver.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := grpcclient.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	cartsSettings, err := config.Struct(func(r *config.Resolver) grpcclient.Settings {
		c := d
		// No .Default: Addr is required, and carts serves TLS, so
		// CARTS_CA_FILE is too.
		c.Addr = r.Get(env("CARTS_ADDR").Validate(grpcclient.CheckAddr))
		c.TLS = r.Get(env("CARTS_CA_FILE").Parse(readFile(s.Host.FS)).Parse(clientTLS).Validate(grpcclient.CheckTLS))
		c.Timeout = r.Get(env("CARTS_TIMEOUT").Parse(time.ParseDuration).Default(d.Timeout).Validate(grpcclient.CheckTimeout))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}
	serverSettings, err := config.Struct(func(r *config.Resolver) grpcserver.Settings {
		return grpcserver.Settings{
			Addr: r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
			TLS:  r.Get(env("GRPC_TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(grpcserver.CheckTLS)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err
	}

	// One Client for the dependency, built once and shared by every RPC
	// the service serves.
	carts, err := grpcclient.NewClient(s, cartsSettings)
	if err != nil {
		return nil, err
	}

	api := &api{carts: testpb.NewTestServiceClient(carts)}
	svc, err := grpcserver.NewService(s, serverSettings)
	if err != nil {
		return nil, err
	}
	testpb.RegisterTestServiceServer(svc, api)
	return []humus.Service{svc}, nil
}
```

The samples on this page use grpc-go's `interop/grpc_testing` service, as
`testpb`, where a real service has its own generated code. The handler passes
its RPC's context to the call, so the call ends when the RPC it serves does:

```go
// api is the end user's service: grpc-go's interop/grpc_testing
// TestService, whose UnaryCall asks carts, the dependency, for its username,
// and answers with it.
type api struct {
	testpb.UnimplementedTestServiceServer

	carts testpb.TestServiceClient
}

func (a *api) UnaryCall(ctx context.Context, _ *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	// The call to carts ends with the inbound RPC's context.
	resp, err := a.carts.UnaryCall(ctx, &testpb.SimpleRequest{FillUsername: true})
	if err != nil {
		return nil, err
	}
	return &testpb.SimpleResponse{Username: "orders, via " + resp.GetUsername()}, nil
}
```

The service's `main` is the server's, as [gRPC services](/humus/pr-preview/pr-929/grpc/#a-service)
describes:

```go
grpcserver.Main(Init, humus.WithTelemetry(buildTelemetry))
```

A service that serves HTTP, or runs a job, builds its `Client` the same way,
and runs on that package's `Main`.

**A `Client` is not a `humus.Service`.** It has nothing to serve, and
nothing to drain: an RPC in flight ends with its own context. `NewClient`
dials nothing, as `grpc.NewClient` doesn't: the first RPC dials, or the first
check of [`Connected`](#readiness-connected). So a dependency that is down
when the service starts doesn't fail `Init`, and the service doesn't
crash-loop over another service's outage. Its RPCs fail with `Unavailable`
until the dependency is reachable.

`NewClient` registers the connection's `Close` with `s.OnStop`. `OnStop`
functions run after every Service has stopped, so no Service is still
calling, and within the grace period. They run when `Init` fails after
`NewClient` too. An RPC made after the close fails with `Canceled`, and
`grpc: the client connection is closing`. `Client` has no `Close` of its
own, so the end user's code can't close a connection that other Services
share, under them.

### One Client per dependency

A `Client` is one dependency at one `Addr`, over one connection, with one
retry budget and one circuit breaker. **Build each `Client` once, in `Init`,
and share it** between every Service that calls the dependency: the `Client`
is safe for concurrent use, and its RPCs share its connection, its budget and
its breaker. A `Client` built for each call would dial and handshake for each
call, start each with a full budget and a closed breaker that never sees a
second failure, and register one more function with `s.OnStop` each time.
`NewClient` takes the `*humus.Setup` because it belongs in `Init`.

### Two Clients of one address

Every method of a `Client`, from every goroutine, goes through its one
breaker. Two APIs served at one address, such as behind one gateway, whose
outages the end user wants kept apart get two `Client`s of that address,
each with its own `Settings`, and so each with its own breaker and retry
budget:

```go
// Two Clients of one address, each with its own Settings, and
// so its own breaker, built once in Init and shared by
// everything that calls them.
payments, err := grpcclient.NewClient(s, paymentsSettings)
if err != nil {
	return nil, err
}
ledger, err := grpcclient.NewClient(s, ledgerSettings)
if err != nil {
	return nil, err
}
```

While `payments` fails, and its breaker rejects its calls at once, `ledger`'s
calls still go through. Each `Client` opens a connection of its own.

## Settings

`grpcclient.Settings` follows the
[settings convention](/humus/pr-preview/pr-929/configuration/#what-a-package-with-settings-exports):
plain Go values, a `DefaultSettings()`, and a check for each field with a
valid range. The package reads no environment variable and defines no
variable name: the end user's `Init` sources each field with
[`config.Struct`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Struct),
with whatever variable names the service uses, and passes the field's check
to `Validate`, as `Init` above does. An operator's bad value then fails
startup with exit 78, naming the variable.

| Field | Default | Valid | Check |
|---|---|---|---|
| `Addr` | none: required | `host:port`, with a host and a port from 1 to 65535, and no scheme | `CheckAddr` |
| `TLS` | `nil`: plain text | `nil`, or a config that allows TLS 1.2 or later | `CheckTLS` |
| `Timeout` | `10s` | positive | `CheckTimeout` |
| `MaxAttempts` | `3` | 1 or more | `CheckMaxAttempts` |
| `BackoffBase` | `100ms` | positive | `CheckBackoffBase` |
| `BackoffMax` | `5s` | positive, and at least `BackoffBase`, which `NewClient` checks | `CheckBackoffMax` |
| `RetryBudget` | `{MaxTokens: 10, TokenRatio: 0.1}` | `MaxTokens` from 3 to `retry.TokenLimit`, `TokenRatio` from 0.001 to `retry.TokenLimit` | `CheckRetryBudgetMaxTokens`, `CheckRetryBudgetTokenRatio` |
| `Classify` | the table under [The default](#the-default) | set | none: set in code |
| `Breaker` | `{Window: 20, Probes: 1, OpenFor: 10s, ProbeTimeout: 10s}` | `Window` and `Probes` from 1 to `circuit.MaxCount`, the durations positive | `CheckBreakerWindow`, `CheckBreakerProbes`, `CheckBreakerOpenFor`, `CheckBreakerProbeTimeout` |
| `BreakerPolicy` | see [The circuit breaker](#the-circuit-breaker) | `Trip` and `Probe` set | none: set in code |

**`Addr` has no default,** so its chain has no `.Default`, and a variable
that isn't set fails as not found. `CheckAddr` rejects an empty host, which
`grpcserver.CheckAddr` accepts: a client dials a host, it doesn't listen on
every name. It rejects a scheme, such as `dns:///carts:9090`, since the
`Client` never resolves names itself, a named port, such as `:grpc`, and
`?`, `#` and `%`, which grpc-go's target would read as something other than
the address.

**`Classify` and `BreakerPolicy` are set in code, not sourced.** What a
dependency's codes mean, and how it fails, are the end user's knowledge, not
an operator's choice. `NewClient` runs every check again, rejects a nil
`Classify`, `Trip` or `Probe`, and a `BackoffMax` below `BackoffBase`, which
takes both fields to tell, and reports every bad field in one error that
names each one. That fails `Init` with exit 1, since the fault is in the
code, or in a pair of values no check given to `Validate` sees together.
[How a bad value fails](/humus/pr-preview/pr-929/configuration/#how-a-bad-value-fails)
explains the exit codes.

## TLS and plain text

`Settings.TLS` is the config the `Client` dials with: the roots it trusts in
`RootCAs`, and for mTLS, a client certificate in `Certificates`. The
`Client` dials with a clone of it, so the end user's config is never
changed. grpc-go adds `h2` to its ALPN and enforces it, and `CheckTLS`
rejects a config that allows nothing from TLS 1.2 on, which HTTP/2 requires.
`Init` above reads the dependency's CA from a file, through `s.Host.FS`, as
[Reading a file](/humus/pr-preview/pr-929/configuration/#reading-a-file) describes,
and trusts only that CA:

```go
// clientTLS returns a client's TLS config that trusts only the CA
// certificates in pem.
func clientTLS(pem []byte) (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("holds no PEM certificate")
	}
	return &tls.Config{RootCAs: roots}, nil
}
```

`readFile` and `serverTLS` are the same helpers as under
[gRPC services](/humus/pr-preview/pr-929/grpc/#the-certificate).

**A config with no `RootCAs` trusts the system's roots,** which
`crypto/x509` loads from the real host: the files `SSL_CERT_FILE` and
`SSL_CERT_DIR` name in the real environment, and the OS's certificate files.
That is one of the [ambient leaks](/humus/pr-preview/pr-929/lifecycle/#ambient-leaks).
A service that calls only its own services sets `RootCAs` to its CA, as
`clientTLS` does, and reads nothing ambient, and a test sets it to the pool
of a CA from [`humustest.NewCA`](/humus/pr-preview/pr-929/testing/#tls).

**A nil `TLS` means plain text.** `Addr` is `host:port` and names no scheme,
so a nil `TLS` is the one way to ask for it. humus can't control what a
service connects to: a mesh's sidecar, a vendor's agent or a local emulator
may well be plain text. Every humus server requires TLS, so a `Client` of
one always sets it, and a `Client` that dials one in plain text fails its
RPCs.

### `:authority` and the certificate

grpc-go takes the authority from the target, so every RPC's `:authority` is
`Addr`, and TLS verifies the server's certificate for `Addr`'s host. A
`Client` of `carts:9090` sends `:authority carts:9090`, and requires a
certificate for `carts`. Two settings replace both, as they would on any
grpc-go client: a `ServerName` in `Settings.TLS`, and a `grpc.WithAuthority`
in `NewClient`'s options. A `Client` of `10.0.0.7:9090` whose `TLS` has the
`ServerName` `carts` sends `:authority carts`, and requires a certificate for
`carts`.

## Names, addresses and load

`NewClient` targets `passthrough:///` and `Settings.Addr`, and dials with the
Setup's `Host.Dial`. The passthrough resolver hands `Addr` to the dialler as
it is, so grpc-go looks no name up: the Host resolves the name when it
dials. In `main`, `humus.OSHost`'s `Dial` looks the name up as `net.Dial`
does, each time it dials. grpc-go's `dns` resolver, the default for a target
with no scheme, is never used: it would look the name up around the Host,
and inside a `testing/synctest` bubble it crashes the test binary, as
[The `dns` resolver and synctest](/humus/pr-preview/pr-929/grpc/#the-dns-resolver-and-synctest)
describes. The `Client` uses no HTTP CONNECT proxy, and never reads
`HTTPS_PROXY` or the other proxy variables.

### One connection, picked first

The passthrough resolver hands grpc-go one address, whatever its form, a name
or an IP, so the `Client` has grpc-go's default `pick_first` policy, and
**every RPC goes over one HTTP/2 connection**, to whichever backend that
dial reached. `OSHost`'s `Dial` tries a name's addresses in the order the
lookup returns them, and keeps the first connection that succeeds. So:

- **Behind a Kubernetes ClusterIP Service,** the Service's name resolves to
  its one virtual IP, and kube-proxy picks a pod for each connection. The
  `Client` stays on that pod.
- **Behind a headless Service,** the name resolves to every ready pod, and
  the `Client` connects to the first that accepts. `Client`s in different
  pods may reach different backends, but each `Client` keeps one.
- **When the connection ends,** grpc-go dials the same name again, with its
  own connection backoff, which starts at 1s and grows to 120s, and the Host
  looks the name up again, so a `Client` finds pods that weren't there when
  it first dialled. A pod's IP, given as `Addr`, never moves.

This is what an end user who calls `grpc.NewClient` with nothing but an
address already has: grpc-go's `dns` resolver and `pick_first` keep one
connection too, even to a name with several addresses. The `Client` changes
who looks the name up, not how many connections there are. Client-side load
balancing is out of humus's scope: it wouldn't help a ClusterIP Service,
whose name has one address whichever resolver looks it up.

### Spreading the load with `MaxConnectionAge`

Two things move a `Client` to another pod. `grpcserver`'s `Shutdown` sends
every client a GOAWAY, so a rollout of the dependency moves them. And a
server whose end user sets `keepalive.ServerParameters{MaxConnectionAge}` in
`grpcserver.NewService`'s options sends a GOAWAY when a connection reaches
that age. The `Client` then dials again, through the Kubernetes Service, and
reaches whichever pod kube-proxy picks, or, behind a headless Service,
whichever pod accepts first. [gRFC A9](https://github.com/grpc/proposal/blob/master/A9-server-side-conn-mgt.md)
added `MaxConnectionAge` for this kind of rebalancing. The dependency's
`Init` sets it:

```go
// Every connection is closed with a GOAWAY once it is about a
// minute old, so its clients dial again, through the
// Kubernetes Service, and reach whichever pod it picks.
grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: time.Minute}),
```

as an option of `grpcserver.NewService`, after the `Settings`, as
[Server options](/humus/pr-preview/pr-929/grpc/#spreading-clients-across-pods)
describes. grpc-go adds a jitter of 10% either way, so a dependency's clients
don't all dial again at once. A GOAWAY lets the RPCs in flight finish, and
the next RPC goes over a new connection, so a `Client` sees no error. A
server whose streams may stay open for longer than that also sets
`MaxConnectionAgeGrace`, after which the connection is closed with them
still open.

A dependency that wants its callers' load spread across its pods sets
`MaxConnectionAge` to a minute or a few: shorter reaches a scale-up's new
pods sooner, and costs each client a new connection and TLS handshake that
often.

## Which calls are retried

Every unary RPC is retried with bedrock's
[`retry`](https://pkg.go.dev/github.com/z5labs/bedrock/retry) package, as
`Settings.Classify` decides. `NewClient` builds one retry budget, one
schedule and one retrier, shared by every method and every goroutine, so a
`Client` is one dependency with one budget. Each attempt is one grpc-go RPC,
and the call returns the last attempt's error.

Retries are bounded three ways:

- **`MaxAttempts`** bounds the attempts of each call, 3 by default, and 1
  turns retries off.
- **`RetryBudget`** bounds every call to the dependency together, as gRPC's
  retry throttling does, and as
  [`retry.Budget`](https://pkg.go.dev/github.com/z5labs/bedrock/retry#Budget)
  describes: each attempt classified `Retry` takes a token, each `Success`
  adds a tenth of one back, and retries stop while half the tokens or fewer
  are left. So while the dependency fails, every call makes one attempt,
  rather than three, and the retries don't multiply its load. First attempts
  are never limited.
- **`Timeout`** bounds each unary call: every attempt, and the waits between
  them. The `Client` applies it to the call's context before the first
  attempt, so a caller's earlier deadline wins, but a later one doesn't
  extend it: an end user whose calls need longer sets the field. A `Timeout`
  that has passed fails the call with `DeadlineExceeded`, and a wait that
  would end after the call's deadline isn't started: the call returns the
  last attempt's error at once.

The wait before each retry is drawn from
[`retry.Exponential`](https://pkg.go.dev/github.com/z5labs/bedrock/retry#Exponential)'s
schedule, with `BackoffBase` and `BackoffMax`: after attempt n, from 0 up to
the smaller of `BackoffMax` and `BackoffBase`·2ⁿ⁻¹.

An interceptor or other option in `NewClient`'s options runs inside the
retries, once per attempt: an interceptor chained with
`grpc.WithChainUnaryInterceptor` sees every attempt, as grpc-go's stats
handlers do.

**grpc-go's own configurable retries are off.** grpc-go retries an RPC by
itself when a service config gives its method a `retryPolicy`. The `Client`
passes `grpc.WithDisableRetry`, so no service config, from
`grpc.WithDefaultServiceConfig` in the options or any other source, turns
them on: whether an RPC is tried again is decided by the `Client`, never by
a service config that an operator can't see in `Settings`. grpc-go's
transparent retries stay on. They retry an RPC when no data reached the wire,
or when the server reports it never processed it, such as a stream refused by
a server that sent a GOAWAY, so they never repeat anything a server saw.

## How an attempt's result counts

Every attempt's result is classified twice: for retry, whether another
attempt follows, and for the dependency's circuit breaker, whether the
result counts against the dependency. `Settings.Classify` returns both, in a
`Classification`. Some of that is about the call, and the `Client` decides
it the same way for every dependency. The rest is about what the
dependency's codes mean, which differs from one API to another, and
`Classify` decides it.

`Classify` gets the RPC's full method name, such as
`/cart.v1.CartService/AddItem`, the error the attempt ended with, nil for
`OK`, and the attempt's trailer. `status.FromError` gives the error's code
and its details.

### What the client decides

`Classify` isn't asked about these, and the call ends, since another attempt
can't help:

- **a call the circuit breaker rejected.** It reached nothing, and the
  breaker would reject the next attempt too;
- **an error once the call's context was cancelled:** the caller gave up.
  The breaker ignores it;
- **an error once the call's deadline passed,** `Timeout` or a deadline the
  caller set or inherited. The breaker counts it as a failure.

And whatever `Classify` returns:

- a `RetryAfter` longer than `BackoffMax` ends the call, with the attempt's
  error, as [The pushback](#the-pushback) describes;
- a stream that opened is never retried, as [Streams](#streams) describes.

### The default

`DefaultSettings().Classify` decides, of every other result, by its code,
which `status.Code` gives, so an error with no gRPC status, such as one an
interceptor returns, is `Unknown`:

| An attempt's code | Retry | Breaker |
|---|---|---|
| `OK` | `Success` | `Success` |
| `Unavailable` | `Retry`, after the pushback if there is one | `Failure` |
| `ResourceExhausted` with a pushback of 0 or more | `Retry`, after the pushback | `Failure` |
| `ResourceExhausted` without a pushback | `Stop` | `Failure` |
| `DeadlineExceeded`, `Internal`, `Unknown`, `DataLoss` | `Stop` | `Failure` |
| `Canceled` | `Stop` | `Ignored` |
| any other code, such as `NotFound`, `InvalidArgument` or `Aborted` | `Stop` | `Success` |

A `grpc-retry-pushback-ms` that is negative or not a number makes any of
these `Stop` but `OK`, which has nothing to retry: the server asked not to be
tried again.

- **`Unavailable` is gRPC's code for "try again".** It is what grpc-go
  returns when it can't reach the server, and what `grpcserver` ends streams
  with at shutdown.
- **`ResourceExhausted` is retried only when the server says when.** Without
  a pushback it is as likely to be a quota as an overload, and a quota isn't
  fixed by trying again a few hundred milliseconds later. Either way it
  counts against the dependency, which is shedding load.
- **`DeadlineExceeded` while the call's own deadline is live is the
  server's:** it gave up on something of its own, such as its dependency. It
  counts, as the call's own deadline does, and isn't retried, because the
  server's deadline is likely to end the next attempt the same way.
- **`Canceled` while the call's context is live** comes from the server
  cancelling the RPC, or the connection closing at `OnStop`. It says nothing
  about the dependency.
- **Any other code is the dependency working.** It answered, and the request
  was wrong or conflicted with its state, so the breaker counts the
  dependency healthy, and the default doesn't try again.

The default reads only the code and the trailer, never a status's details.

**The default retries every method on `Unavailable`, whatever the method.**
A server that returns `Unavailable` after acting on a request breaks gRPC's
meaning of the code, so for most APIs that is safe. But grpc-go also returns
`Unavailable` when the connection breaks after the request was sent, and
then the server may have acted on it. So a method that must never be sent
twice, such as one that charges a card, opts out in `Classify`, which gets
the method: there is no per-method table in `Settings`.

### Writing a `Classify`

An end user whose API means something else by a code writes a `Classify` of
their own, which usually handles what differs and hands the rest to the
default. This API aborts a write that races another write to the same
account, which is over by the next attempt, so its `Classify` retries
`Aborted`, and counts it for the dependency rather than against it:

```go
classify := s.Classify
s.Classify = func(method string, err error, trailer metadata.MD) grpcclient.Classification {
	if status.Code(err) == codes.Aborted {
		return grpcclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
	}
	return classify(method, err, trailer)
}
```

`s` is the `Settings` here, whose `Classify` starts as the default. **A
method opts out of retries** in a `Classify` that asks the default first,
and turns its `Retry` into `Stop` for that method alone, so its result still
counts in the breaker as the default says. Here `EmptyCall` stands in for a
method such as `Charge`:

```go
classify := s.Classify
s.Classify = func(method string, err error, trailer metadata.MD) grpcclient.Classification {
	c := classify(method, err, trailer)
	// EmptyCall must never be sent twice: it makes one
	// attempt, and its result counts in the breaker as
	// the default says.
	if method == "/grpc.testing.TestService/EmptyCall" && c.Retry == retry.Retry {
		c.Retry, c.RetryAfter = retry.Stop, 0
	}
	return c
}
```

**One `Classify` serves every API that shares a convention,** such as a
company's own services. The function is the end user's own, so it is
written once and set on the `Client` of each, here the two `Client`s of one
address [above](#two-clients-of-one-address):

```go
// The APIs at gateway:9090 share a convention: Aborted says
// another write to the account raced this one, which is over by
// the next attempt. One Classify says so, and is set on the
// Client of each.
classify := grpcclient.DefaultSettings().Classify
ourAPIs := func(method string, err error, trailer metadata.MD) grpcclient.Classification {
	if status.Code(err) == codes.Aborted {
		return grpcclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
	}
	return classify(method, err, trailer)
}
paymentsSettings.Classify = ourAPIs
ledgerSettings.Classify = ourAPIs
```

An API that sends `google.rpc.RetryInfo` in its statuses' details, rather
than a pushback, has a `Classify` that reads it with `status.FromError` and
returns it as `RetryAfter`.

`Classify` is called on the goroutine that made the call, once for each
attempt, so it must be safe for concurrent use. It is asked about `OK`, a
nil error, too, so a `Classify` that returns `retry.Retry` for every result
sends a call that succeeded again: it returns `retry.Success` for `OK`, as
the default does. A `Classification` with a zero `Outcome`, or with
`retry.Retry` and a negative `RetryAfter`, panics, as bedrock's classifiers
do. The `Client` recovers nothing: the panic passes to the goroutine that
made the call. humus exports no classifier but the default, and no helpers
to build one.

### The pushback

`grpc-retry-pushback-ms` is
[gRFC A6](https://github.com/grpc/proposal/blob/master/A6-client-retries.md)'s
trailer, in which a server says how many milliseconds to wait before the next
attempt. The default `Classify` reads it from the attempt's trailer and
returns it as the `Classification`'s `RetryAfter`, so the wait before the
next attempt is the larger of it and the schedule's. A pushback that is
negative, not a number or given more than once means "don't retry". An end
user's `Classify` returns whatever its API says.

**A `RetryAfter` longer than `BackoffMax` isn't waited for,** from any
`Classify`: the call ends, and returns the attempt's error. A server that
asks for longer than that is not one a call in flight should wait on, and a
wait past the call's deadline would end it anyway.

## The circuit breaker

Each `Client` has one circuit breaker,
[bedrock's `circuit.Breaker`](https://pkg.go.dev/github.com/z5labs/bedrock/circuit),
which guards the dependency at `Addr`: while the dependency keeps failing,
the `Client` fails calls at once, rather than hold every caller for its
timeouts and retries, and a probe now and then finds out when the dependency
is back. The breaker sits inside the retries, so each attempt is a call it
admits or rejects.

What counts is the `Breaker` of each attempt's `Classification`: a
`Success` counts toward the dependency being healthy, a `Failure` against
it, and an `Ignored` result counts nothing. The caller giving up is ignored.
**Every deadline counts as a failure,** whether it is `Timeout` or a
deadline the caller set or inherited from the request it serves: a
dependency that hangs ends every call that way, so a breaker that ignored
deadlines would never open on the outage it exists for.

**The default opens after 5 failures in a row, or once half of the last 20
or more results failed.** It then rejects every call for `Breaker.OpenFor`,
10s, and then lets `Breaker.Probes` calls through at once, one by default: a
failed probe reopens it, and 3 successful ones close it.
`Settings.Breaker` configures, from configuration, the `Window` of results
the policy sees, the `Probes`, and the durations `OpenFor` and
`ProbeTimeout`. `Settings.BreakerPolicy` decides when it opens and closes,
in code: its `Trip` decides the 5 and the half of 20, and its `Probe` the 3
successful probes. The ratio rule needs 20 results, so with a
`Breaker.Window` below 20 only the rule of 5 failures in a row trips it. A
policy whose `Trip` always returns false turns the breaker off:

```go
s.BreakerPolicy.Trip = func(circuit.Counts) bool { return false }
```

The policy's functions run with the breaker locked, on the goroutine of the
call whose result they decide on, so they must be fast and safe for
concurrent use. A panic in one passes to that goroutine, and the breaker
stays consistent, so the next call goes through it as before.

### Rejections

**A call the breaker rejects reaches neither grpc-go nor the backend, and
isn't retried:** the call ends at once. `Classify` isn't asked about it. Its
error matches `circuit.ErrRejected` with `errors.Is`, and
`*circuit.RejectedError` with `errors.As`, whose `RetryAfter` says how much
longer the breaker stays open. Its gRPC status is `Unavailable`, as for a
server the `Client` can't reach, with the rejection's message, such as
`circuit: call rejected: open for another 4.2s`. So code that handles
`Unavailable` handles a rejection too, and code that needs to know nothing
was sent asks `errors.Is(err, circuit.ErrRejected)`.

### Its log records

The `Client` logs through the Setup's `LoggerProvider`, under the scope
`github.com/z5labs/humus/grpc/grpcclient`, one record for each change of the
breaker's state, in the context of the call that made it:

| Record | Severity | When |
|---|---|---|
| `circuit breaker open` | WARN | the breaker opens, from closed or from half-open |
| `circuit breaker half-open` | INFO | `OpenFor` has passed, and the breaker lets a probe through |
| `circuit breaker closed` | INFO | enough probes succeeded |

Each has the dependency's `server.address` and `server.port`, from `Addr`,
the state the breaker left as `humus.circuit.previous_state`, and the counts
of that state: `humus.circuit.successes`, `humus.circuit.failures`,
`humus.circuit.consecutive_successes`, `humus.circuit.consecutive_failures`
and `humus.circuit.timed_out`. The bodies and the attributes are those of
[`httpclient`](/humus/pr-preview/pr-929/http-client/#its-log-records)'s, so one query
finds every breaker. The `Client` logs nothing else: an error is returned to
the end user's code, not logged.

## Streams

A stream runs the same loop as a unary call, with `MaxAttempts` and the
backoff, but not `Timeout`: a stream may rightly live as long as its caller
wants, so it ends with the context its caller opened it with. Each attempt
opens the stream, through the breaker:

- **Opening is retried.** Opening fails before anything reaches the server:
  no connection, or the channel in transient failure. A failed open is
  classified as a unary attempt is, and `Classify` gets it with no trailer,
  since nothing came back.
- **An open stream is never retried.** The end user's code sends and
  receives on it directly, and a retry would replay what it already saw. A
  stream that fails after it opened ends with that error.

A stream that opens is counted in the breaker once, when it ends, with the
`Breaker` of `Classify`'s answer: when `RecvMsg` returns an error, which
`Classify` gets with the stream's trailer, and `io.EOF` given as nil; or, for
a client-streaming RPC, when `RecvMsg` returns its one response, given as
nil. A stream whose caller cancelled its context is ignored. One whose
context reached its deadline counts as a failure, as a unary call's does.

A stream its caller abandons, without reading it to its end or cancelling
its context, is never counted. While the breaker is closed that costs
nothing. While it is half-open, the abandoned stream holds a probe's slot,
but for at most `Breaker.ProbeTimeout`, after which another probe may go,
and the probe counts as timed out, which the default policy reads as neither
a success nor a failure. So one leaked stream can't keep the breaker from
closing.

## Telemetry

The `Client` takes everything from the Setup's telemetry. `NewClient`'s
first dial option is a stats handler, OpenTelemetry's gRPC instrumentation,
[`otelgrpc`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc),
given the `TracerProvider`, the `MeterProvider` and the `Propagator` of
`s.Telemetry`. Nothing reaches OpenTelemetry's globals: a process that sets
them sees none of the `Client`'s spans or metrics.

**Each attempt is one span of kind CLIENT,** a child of the span in the
call's context, such as the server span of the RPC the end user's handler
serves, or a job's run span. A call the `Client` retries is a span for each
attempt, siblings under the caller's span, and so is a call that grpc-go
retries transparently. A stream is one span, from its opening to its end.

- **The span is named after the full method** without its leading slash,
  such as `cart.v1.CartService/AddItem`.
- **It carries the attributes of OpenTelemetry's RPC semantic
  conventions,** among them `rpc.system.name`, which is `grpc`,
  `rpc.method`, and `rpc.response.status_code`, the name of the call's code,
  such as `UNAVAILABLE`.
- **Its status is `Error` for every code other than `OK`:** otelgrpc counts
  every failed call as an error on the client's side, `NotFound` included,
  unlike on a server's.

**A call the breaker rejected still has a span.** Nothing reached grpc-go,
so otelgrpc makes none, and the `Client` starts one of its own, under the
scope `github.com/z5labs/humus/grpc/grpcclient`, named and attributed as
otelgrpc names an attempt's span, with `rpc.response.status_code`
`UNAVAILABLE`, and with `server.address` and `server.port` taken from
`Addr`, since no connection was made. It ends at once, with status `Error`
and the rejection's message.

**Each attempt carries the trace context of its span, and the baggage of
the call's context, in its metadata,** written by the Setup's propagator. So
a dependency served with `grpcserver` joins the caller's trace, its server
span a child of the attempt's client span, and its handler sees the caller's
baggage.

**Each attempt records `rpc.client.call.duration`** on the Setup's
`MeterProvider`, in otelgrpc's instrumentation scope. The `Client` records
four instruments of humus's own as well, for its breaker and its retries, as
[humus's own instruments](#humuss-own-instruments) lists. The breaker's
state changes are the `Client`'s only log records, as
[Its log records](#its-log-records) lists.

What is still read from the real environment is what
[gRPC services](/humus/pr-preview/pr-929/grpc/#grpc-gos-logger) records: grpc-go's
logger, which reads `GRPC_GO_LOG_SEVERITY_LEVEL` and the other `GRPC_GO_LOG_`
variables and writes to the real standard error, grpc-go's other `GRPC_`
variables, and `OTEL_SEMCONV_STABILITY_OPT_IN`, which otelgrpc reads. The
`Client` adds nothing to that list.

### humus's own instruments

The `Client` records four instruments of humus's own, for its breaker and
its retries, with the Setup's `MeterProvider`, in the scope
`github.com/z5labs/humus/grpc/grpcclient`, the scope of its log records:

| Instrument | Kind | Unit | Attributes | Recorded |
|---|---|---|---|---|
| `humus.circuit.state` | asynchronous UpDownCounter | `{breaker}` | `humus.circuit.state`: `closed`, `open` or `half-open` | at each collection: 1 for the breaker's state, 0 for the other two |
| `humus.circuit.transitions` | Counter | `{transition}` | `humus.circuit.state`, the state it went to, and `humus.circuit.previous_state`, the one it left | at each change of the breaker's state, beside its log record |
| `humus.circuit.calls` | Counter | `{call}` | `humus.circuit.outcome`: `success`, `failure`, `ignored` or `rejected` | as each unary attempt or stream counts in the breaker, or as the breaker rejects it |
| `humus.retry.decisions` | Counter | `{decision}` | `humus.retry.decision`: `again`, `succeeded`, `stopped`, `exhausted`, `throttled` or `expired` | after each unary attempt and each attempt to open a stream |

Every data point also carries the dependency's `server.address` and
`server.port`, from `Addr`, the same as the breaker's log records, and
nothing per call, such as `rpc.method`: otelgrpc's instruments break calls
down already. Each other attribute's value is the `String()` of bedrock's
own type, `circuit.State`, `circuit.Outcome` or `retry.Decision`.

- **`humus.circuit.state` is read, not tracked.** At each collection it reads
  the breaker's state once, so it shows `half-open` as soon as `OpenFor` has
  passed, with no call made. It is a count of breakers in each state, so two
  `Client`s of one address add up, as `closed` 1 and `open` 1. The App's
  last collection, after the `OnStop` functions, still reads it, so the
  final export has the state the App stopped in.
- **`humus.circuit.transitions` catches the short opens.** A breaker can
  open, go half-open and close between two collections, a minute apart by
  default, and `humus.circuit.state` never shows it open.
- **`humus.circuit.calls` counts a call when the breaker does.** One call is
  one unary attempt, or one stream's open. A unary attempt counts once it
  has returned, and a stream once it has ended, as [Streams](#streams)
  describes, so a stream that is still open counts nothing yet. A stream
  whose caller cancelled it counts nothing at all. It is the only place the
  verdicts of the end user's `Classify` show. A call the breaker rejects is
  `rejected`: it never reached grpc-go, so it is in no instrument of
  otelgrpc's.
- **`humus.retry.decisions` counts what `retry` decided,** so `throttled`
  counts the retries the budget refused, `exhausted` the calls that used
  every attempt, and `expired` the calls whose deadline came before the next
  attempt could start. A rejected attempt is `stopped`, and a stream that
  opens is `succeeded`, however it later ends.

In Prometheus, they are `humus_circuit_state`,
`humus_circuit_transitions_total`, `humus_circuit_calls_total` and
`humus_retry_decisions_total`, and each attribute is a label such as
`humus_circuit_state`. An alert on an open breaker reads the state, and one
on breakers that open and close between scrapes reads the transitions:

```promql
# A breaker is open now.
humus_circuit_state{humus_circuit_state="open"} > 0

# A breaker opened in the last 5 minutes, however briefly.
increase(humus_circuit_transitions_total{humus_circuit_state="open"}[5m]) > 0
```

A View in the end user's telemetry `Builder` drops or renames any of them.
humus has no setting to turn them off. A test reads them as it reads any
metric, with `humustest.Telemetry`'s `Metrics`, filtered to the scope.
[humus's own instruments](/humus/pr-preview/pr-929/telemetry/#humuss-own-instruments)
on the Telemetry page lists what each of humus's clients records, with the
View and the test written out.

## Readiness: `Connected`

`Client.Connected` returns a `health.Monitor` for readiness. It is healthy
once the `Client` has connected to `Addr`: unhealthy until its connection is
first ready, its TLS handshake done, and healthy from then until the
connection is closed. It never returns an error. It goes in the readiness of
whatever exposes the probes, here a `grpchealth` probes port, as
[Health](/humus/pr-preview/pr-929/health/#exposing-the-probes) describes:

```go
carts, err := grpcclient.NewClient(s, cartsSettings)
if err != nil {
	return nil, err
}
svc, err := grpcserver.NewService(s, serverSettings)
if err != nil {
	return nil, err
}
testpb.RegisterTestServiceServer(svc, &api{carts: testpb.NewTestServiceClient(carts)})

// The service turns ready once its Client has connected to
// carts, and stays ready through an outage of carts after
// that.
probes, err := grpchealth.NewService(s, probeSettings, health.Readiness(carts.Connected()))
if err != nil {
	return nil, err
}
return []humus.Service{svc, probes}, nil
```

**What it gates is a rollout.** `NewClient` dials nothing, so without it a
release with a wrong `Addr`, a CA that doesn't trust the server, a missing
TLS config, or a dependency it can't reach turns ready, replaces the pods
that worked, and fails every RPC. With `Connected` in its readiness, it never
turns ready, and the rollout stops at its progress deadline with the old pods
still serving.

**It latches,** as a Kafka consumer's
[`Joined`](/humus/pr-preview/pr-929/kafka/#readiness) does, for the same reason.
After the first connection, an outage of the dependency hits every pod at
once. Turning them all unready would take the service out of its load
balancer, so its callers would get no answer instead of this service's own
error, and the parts of it that don't need the dependency would stop too. It
would also block a rollout that might be the fix. So once the connection has
been ready, the monitor stays healthy whatever the connection's state: after
a GOAWAY, from a server's `MaxConnectionAge` or a rollout of the dependency,
after a lost connection, and after grpc-go closes a connection idle for 30
minutes. Every monitor `Connected` returns reads the same latch, kept on the
`Client`, so an HTTP probe and a gRPC one agree.

**`Connected` is for readiness only.** A liveness monitor would restart every
pod in an outage of the dependency, and restarting the pod doesn't bring a
dependency back.

**Its first check dials.** Until the connection is first ready, a check while
it is idle starts it, so a service with `Connected` in its readiness dials at
its first probe rather than its first RPC. The check then waits until the
connection is ready, the connection is closed, or the check's context is
done. So a reachable dependency passes the first probe, and an unreachable
one fails it when the probe's own timeout ends the check. A check makes no
RPC: the dependency sees no call, the trace has no span, and nothing is
logged. After `s.OnStop` has closed the connection, the monitor is
unhealthy; by then every Service has stopped, and the probes have already
turned readiness false.

### A monitor of the dependency's `grpc.health.v1`

A monitor that asks the dependency's own `grpc.health.v1` service is the end
user's own, a few lines over the `Client`:

```go
monitor := health.MonitorFunc(func(ctx context.Context) (bool, error) {
	resp, err := healthpb.NewHealthClient(carts).Check(ctx, &healthpb.HealthCheckRequest{Service: "grpc.testing.TestService"})
	return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING, nil
})
```

Such a monitor makes an RPC through the `Client`, so whatever fails the
`Client`'s calls fails it too: an outage of the dependency, and an open
circuit breaker. It then turns unhealthy, and with it every pod, in the
outage `Connected`'s latch exists to ride out. `Connected` makes no RPC, so
neither turns it unhealthy. humus ships no such monitor.

## Testing

A service that calls a dependency is tested whole, inside
`testing/synctest`, as [Testing a humus App](/humus/pr-preview/pr-929/testing/)
describes for any App. The `Client` dials through the Setup's Host, so the
dependency is a second App on the same humustest Host: a `grpcserver`
Service that answers as the dependency would, with a certificate from a CA
that [`humustest.NewCA`](/humus/pr-preview/pr-929/testing/#tls) generates for the
test. `grpcclient`'s own tests start one with a helper:

```go
// startCarts runs the dependency on h, at carts:9090, over TLS with a
// certificate for carts from ca. It returns a function that stops the
// dependency and requires it to exit 0.
func startCarts(t *testing.T, h *humustest.Host, ca *humustest.CA) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "carts")
	p, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: "carts:9090",
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		})
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, carts{})
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("carts:9090")
	return func() {
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}
```

Its certificate is for `carts`, the host the `Client`'s `Addr` names, so the
`Client`'s verification passes. `carts` is the test's implementation of the
service, whose `UnaryCall` answers with the username `carts`.

The end user's whole App then runs against it, with the end user's own
`Init` above, through `grpcservertest.Start`, and the environment production
gives it. The test writes the App's certificate and the dependency's CA into
the Host's filesystem with `humustest.WithFiles`, and names them with
`humustest.WithEnv`, as
[Giving the App its certificate](/humus/pr-preview/pr-929/grpc/#giving-the-app-its-certificate)
describes. `pemFile` and `caPEM` are helpers of the test file that
PEM-encode them: `pemFile` a certificate's chain and private key, and `caPEM`
the CA certificate that issued a certificate. `orders` and the dependency's
certificate are both issued by `ca`, so `caPEM(orders)` is the dependency's
CA too:

```go
synctest.Test(t, func(t *testing.T) {
	ca := humustest.NewCA(t)
	orders := ca.Issue(t, "orders")
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"tls.pem":      pemFile(t, orders),
		"carts-ca.pem": caPEM(orders),
	}))
	stop := startCarts(t, h, ca)
	defer stop()

	p, exit := grpcservertest.Start(t, Init,
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(map[string]string{
			"GRPC_ADDR":         ":8443",
			"GRPC_TLS_PEM_FILE": "tls.pem",
			"CARTS_ADDR":        "carts:9090",
			"CARTS_CA_FILE":     "carts-ca.pem",
		}),
	)
	<-h.Listening("orders:8443")

	client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "orders:8443", ca.Pool()))
	for range 2 {
		resp, err := client.UnaryCall(t.Context(), &testpb.SimpleRequest{})
		require.NoError(t, err)
		require.Equal(t, "orders, via carts", resp.GetUsername())
	}

	<-p.Notified(syscall.SIGTERM)
	p.Signal(syscall.SIGTERM)
	st := <-exit
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
})
```

The RPC reaches the App, the App's handler calls the dependency through the
`Client`, and the answer comes back, all on the Host's in-memory network, in
no real time. The `Client` trusts the CA through `RootCAs`, so the test reads
no system root and skips no verification.

Fake time makes the `Client`'s waits free too. A dependency that answers
`Unavailable` with a pushback of 2000 costs the test 2s of fake time and no
real time, and a breaker's `OpenFor` passes just as instantly, so a test of
retries, timeouts or the breaker opening and closing runs as fast as one of
a single call. A dependency stopped with the function `startCarts` returns
tests an outage, and a test that asserts on the `Client`'s spans, metrics or
breaker records gives the App `humustest.NewTelemetry()`'s `Builder` rather
than `telemetry.NoopBuilder`.
