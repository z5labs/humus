# Calling HTTP services

The [`httpclient`](https://pkg.go.dev/github.com/z5labs/humus/httpclient)
package calls another service over HTTP. `httpclient.NewClient` builds a
client for one dependency in the end user's `Init`, from `Settings` the end
user sources, and returns a plain `*http.Client`. Its calls go through the
Setup's Host, are traced and measured with the Setup's telemetry, are retried
when that is safe, and fail at once while a circuit breaker says the
dependency keeps failing.

Service code, generated clients and SDKs take the `*http.Client` as they
take any other, and depend on `net/http`'s type rather than on this package,
so a unit test of that code passes any client. The client dials only
through the Setup's Host, so a test runs the end user's whole App, its
outbound calls included, on humustest's network, as [Testing](#testing)
shows.

The design behind everything on this page is in
[Proposal: outbound HTTP clients](https://github.com/z5labs/humus/discussions/762),
with its [revision 1](https://github.com/z5labs/humus/discussions/762#discussioncomment-19009376).

## A client for each dependency

The end user's `Init` sources the dependency's `httpclient.Settings`, builds
its client with `NewClient`, and hands the client to the code that calls the
dependency, here the handler of an [`httpserver`](/humus/pr-preview/pr-900/http/)
Service:

```go
// Init is the end user's Init: it sources the payments dependency's
// Settings, builds its client with NewClient, and hands the client to the
// handler of an httpserver Service.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := httpclient.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	paymentsSettings, err := config.Struct(func(r *config.Resolver) httpclient.Settings {
		c := d
		// No .Default: BaseURL is required, and the dependency is https,
		// so PAYMENTS_CA_FILE is too.
		c.BaseURL = r.Get(env("PAYMENTS_URL").Parse(url.Parse).Validate(httpclient.CheckBaseURL))
		c.TLS = r.Get(env("PAYMENTS_CA_FILE").Parse(readFile(s.Host.FS)).Parse(trusting).Validate(httpclient.CheckTLS))
		c.Timeout = r.Get(env("PAYMENTS_TIMEOUT").Parse(time.ParseDuration).Default(d.Timeout).Validate(httpclient.CheckTimeout))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}
	serverSettings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
		c := httpserver.DefaultSettings()
		c.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		c.TLS = r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err
	}

	// One client for the dependency, built once and shared by every
	// request the handler serves.
	payments, err := httpclient.NewClient(s, paymentsSettings)
	if err != nil {
		return nil, err
	}

	api := newAPI(payments, paymentsSettings.BaseURL)
	svc, err := httpserver.NewService(s, serverSettings, api)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}
```

The handler joins its paths onto the `BaseURL`, and passes its request's
context to the call, so the call ends when the request does:

```go
mux.HandleFunc("GET /orders/{id}/payment", func(w http.ResponseWriter, r *http.Request) {
	// The call ends with the request's context, and service code joins
	// its paths onto the BaseURL.
	u := base.JoinPath("charges", r.PathValue("id"))
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := payments.Do(req)
	if err != nil {
		http.Error(w, "payments is unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("payments answered %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	_, _ = io.Copy(w, resp.Body)
})
```

The service's `main` is the server's, as [Serving HTTP](/humus/pr-preview/pr-900/http/#a-service)
describes:

```go
httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
```

**A client is not a `humus.Service`.** `NewClient` opens no connection, and
`Init` returns no Service for it. It registers one function with
`s.OnStop`, which closes the client's idle connections once every Service
has stopped, so nothing is still calling the dependency by then. It runs
when `Init` fails after `NewClient` too. A call in flight ends with its own
context: a handler's call ends when its request's context does, so the
client has nothing to drain.

### One client per dependency

A dependency is an origin: the scheme, host and port of `Settings.BaseURL`,
with the scheme's default port when the URL has none, so `https://payments`
and `https://payments:443` are one origin. The client sends requests to that
origin and to no other. A request to another origin fails without being
sent, and nothing is dialled, with an error such as:

```
httpclient: request to https://ledger:8443 on a client for https://payments:8443
```

`BaseURL`'s path, if it has one, is for service code to join request paths
onto. The client doesn't use it. A redirect within the origin is followed,
up to 10, as `net/http`'s default policy does; a redirect to another origin
isn't, and service code gets the 3xx response, whose `Location` header it
may read.

**Build each client once, in `Init`, and share it** between every Service
that calls the dependency. That is why `NewClient` takes the
`*humus.Setup`. The cost that matters is a client built for each call: it
pools nothing, so every call dials and handshakes, and each one registers
one more function with `s.OnStop`.

### Two clients of one origin

Two APIs behind one gateway, which the end user wants configured
separately, get two clients of one origin, each with its own `BaseURL` path
and its own `Settings`, and so each with its own breaker and retry budget:

```go
// Two clients of one origin, each with its own Settings, built
// once in Init and shared by everything that calls them.
payments, err := httpclient.NewClient(s, paymentsSettings)
if err != nil {
	return nil, err
}
ledger, err := httpclient.NewClient(s, ledgerSettings)
if err != nil {
	return nil, err
}
```

### Why pooling costs nothing

A client per dependency pools connections as one shared `http.Transport`
would, since `net/http` keys its idle connections by scheme, host and port,
and a shared transport never lends one dependency's connection to another.
The two differ in two ways, and neither is a cost in practice:

- **Idle limits are per client.** Each client keeps up to 100 idle
  connections to its origin for 90s, where one shared transport's
  `MaxIdleConns` would cap every dependency together. A process with many
  dependencies can hold more idle sockets, up to what each dependency's
  busiest burst opened, until they have been idle for 90s. In return, a
  burst to one dependency never evicts another's idle connections, which
  would cost each a new TLS handshake. `net/http`'s default of 2 idle
  connections per host would make a busy client open and close connections
  on every burst.
- **Two clients of one origin don't share connections.** Each opens its own,
  which over HTTP/2 is one more connection for each client.

## Settings

`httpclient.Settings` follows the
[settings convention](/humus/pr-preview/pr-900/configuration/#what-a-package-with-settings-exports):
plain Go values, a `DefaultSettings()`, and a check for each field with a
valid range. The package reads no environment variable and defines no
variable name: the end user's `Init` sources each field with
[`config.Struct`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Struct),
with whatever variable names the service uses, and passes the field's check
to `Validate`, as `Init` above does. An operator's bad value then fails
startup with exit 78, naming the variable.

| Field | Default | Valid | Check |
|---|---|---|---|
| `BaseURL` | none: required | an absolute `http` or `https` URL with a host, and no user info, query or fragment | `CheckBaseURL` |
| `TLS` | `nil`: plain text | `nil`, or a config that verifies the server and allows TLS 1.2 or later | `CheckTLS` |
| `Timeout` | `10s` | positive | `CheckTimeout` |
| `AttemptTimeout` | `5s` | positive | `CheckAttemptTimeout` |
| `MaxAttempts` | `3` | 1 or more | `CheckMaxAttempts` |
| `BackoffBase` | `100ms` | positive | `CheckBackoffBase` |
| `BackoffMax` | `5s` | positive, and at least `BackoffBase`, which `NewClient` checks | `CheckBackoffMax` |
| `RetryBudget` | `{MaxTokens: 10, TokenRatio: 0.1}` | `MaxTokens` from 3 to `retry.TokenLimit`, `TokenRatio` from 0.001 to `retry.TokenLimit` | `CheckRetryBudgetMaxTokens`, `CheckRetryBudgetTokenRatio` |
| `Classify` | the table under [How an attempt's result counts](#how-an-attempts-result-counts) | set | none: set in code |
| `Breaker` | `{Window: 20, Probes: 1, OpenFor: 10s, ProbeTimeout: 10s}` | `Window` and `Probes` from 1 to `circuit.MaxCount`, the durations positive | `CheckBreakerWindow`, `CheckBreakerProbes`, `CheckBreakerOpenFor`, `CheckBreakerProbeTimeout` |
| `BreakerPolicy` | see [The circuit breaker](#the-circuit-breaker) | `Trip` and `Probe` set | none: set in code |

**`BaseURL` has no default,** so its chain has no `.Default`, and a variable
that isn't set fails as not found. `CheckBaseURL` rejects user info: a
password in the URL would sit in a setting, and in every error that names
the client's origin. Credentials go in a header that service code sets.

**`Classify` and `BreakerPolicy` are set in code, not sourced.** What a
dependency's answers mean, and how it fails, are the end user's knowledge,
not an operator's choice. `NewClient` runs every check again, and rejects a
nil `Classify`, `Trip` or `Probe`, failing `Init` with exit 1, since the
fault is in the code.

**Two rules take two fields, and only `NewClient` checks them:** a
`BackoffMax` below `BackoffBase`, and a `BaseURL` whose scheme disagrees
with `TLS`, as [TLS and plain text](#tls-and-plain-text) describes. A check
given to `Validate` sees one value, so neither fails in `config.Struct`.
Both fail `Init` with exit 1, and an error that names the fields, such as
`Settings.TLS must be set for an https Settings.BaseURL`, rather than with
exit 78 naming a variable, even when an operator's values caused them. An
`Init` whose operators set both variables of either pair checks the pair
itself after `Read`, and returns a `*config.Error` naming the variables.
[How a bad value fails](/humus/pr-preview/pr-900/configuration/#how-a-bad-value-fails)
explains the exit codes.

## TLS and plain text

`Settings.TLS` is the config the client dials the dependency with: the roots
it trusts in `RootCAs`, and for mTLS, a client certificate in
`Certificates`. The client dials with a clone of it, so the end user's
config is never changed. `Init` above reads the dependency's CA from a file,
through `s.Host.FS`, as [Reading a file](/humus/pr-preview/pr-900/configuration/#reading-a-file)
describes, and trusts only that CA:

```go
// trusting returns a client's TLS config that trusts only the CA
// certificates in pem.
func trusting(pem []byte) (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("holds no PEM certificate")
	}
	return &tls.Config{RootCAs: roots}, nil
}
```

`readFile` is the same helper as under [Serving HTTP](/humus/pr-preview/pr-900/http/#the-certificate).

`CheckTLS` rejects a config that sets `InsecureSkipVerify`, or that allows
nothing from TLS 1.2 on. A test trusts the CA it generates with
`humustest.NewCA` through `RootCAs`, so nothing needs to skip verification.

**A config with no `RootCAs` trusts the system's roots,** which
`crypto/x509` loads from the real host: the files `SSL_CERT_FILE` and
`SSL_CERT_DIR` name in the real environment, and the OS's certificate
files. That is one of the [ambient leaks](/humus/pr-preview/pr-900/lifecycle/#ambient-leaks).
A service that calls only its own services sets `RootCAs` to its CA, as
`trusting` does, and reads nothing ambient.

**A nil `TLS` means plain text, with an `http` BaseURL.** humus can't control
what a service connects to, and a mesh's sidecar, a node-local agent or a
cloud metadata endpoint is often plain HTTP. The URL names its scheme too,
so `NewClient` requires the two to agree: an `https` BaseURL with a nil
`TLS`, or an `http` one with a config, fails startup rather than quietly
doing one of the two. Every humus server serves TLS, so a call to one is
always `https`.

The client never reads `HTTP_PROXY`, `HTTPS_PROXY` or `NO_PROXY`, and has no
proxy setting. Its `Jar` is nil, so it keeps no cookies; the end user may
set one.

## Which requests are retried

A call is retried only when sending it twice is both safe and possible:

- **Safe:** its method is `GET`, `HEAD`, `OPTIONS`, `TRACE`, `PUT` or
  `DELETE`, which RFC 9110 defines as idempotent, or it carries an
  `Idempotency-Key` or `X-Idempotency-Key` header, which `net/http` treats
  the same way.
- **Possible:** it has no body, or its `GetBody` is set, which
  `http.NewRequest` does for a `*bytes.Buffer`, a `*bytes.Reader` and a
  `*strings.Reader`. The client rewinds the body with `GetBody` before each
  attempt after the first.

Any other request makes one attempt, whatever `Classify` says of its result.
Whether a request is safe to send twice is about the request, not about what
the dependency answered, so a `POST` without an idempotency key can't be
charged twice. Its result still counts in the retry budget and in the
breaker. `net/http`'s own list of requests it replays on a stale connection
leaves out `PUT` and `DELETE`; RFC 9110 defines both as idempotent, and a
service that sends them relies on that already.

Retries are bounded three ways:

- **`MaxAttempts`** bounds the attempts of each request a call sends, 3 by
  default, and 1 turns retries off. A redirect is a request of its own, with
  attempts of its own.
- **`RetryBudget`** bounds every call to the dependency together, as
  [`retry.Budget`](https://pkg.go.dev/github.com/z5labs/bedrock/retry#Budget)
  describes: each attempt classified `Retry` takes a token, each `Success`
  adds a tenth of one back, and retries stop while half the tokens or fewer
  are left. So while the dependency fails, every call makes one attempt,
  rather than three, and the retries don't multiply its load.
- **`Timeout`** bounds the call, as [Timeouts](#timeout-and-attempttimeout)
  describes.

The wait before each retry is drawn from
[`retry.Exponential`](https://pkg.go.dev/github.com/z5labs/bedrock/retry#Exponential)'s
schedule, with `BackoffBase` and `BackoffMax`: after attempt n, from 0 up to
the smaller of `BackoffMax` and `BackoffBase`·2ⁿ⁻¹.

## How an attempt's result counts

Every attempt's result is classified twice: for retry, whether another
attempt follows, and for the dependency's circuit breaker, whether the
result counts against the dependency. `Settings.Classify` returns both, in a
`Classification`. Some of that is about the call, and the client decides it
the same way for every dependency. The rest is about what the dependency's
answers mean, which differs from one API to another, and `Classify` decides
it.

### What the client decides

`Classify` isn't asked about:

- **an attempt the circuit breaker rejects,** which ends the call;
- **an error once the call's context is done,** because the caller gave up
  or the call's deadline passed. Another attempt can't help, so the call
  ends. The breaker ignores the caller giving up, and counts the deadline
  against the dependency;
- **a body that fails once service code reads it,** which
  [the breaker](#the-circuit-breaker) counts as a failure, or ignores when
  the caller cancelled the call's context.

And whatever `Classify` returns:

- a request that isn't safe to send twice, or can't be replayed, makes one
  attempt;
- a `RetryAfter` longer than `BackoffMax` ends the call, with the response as
  it came;
- a response whose body doesn't fit in 64 KiB isn't retried.

### The default

`DefaultSettings().Classify` decides, of every response and every error
while the call's context is live:

| An attempt's result | Retry | Breaker |
|---|---|---|
| a status below 500, other than 408 and 429 | `Success` | `Success` |
| 408 Request Timeout | `Retry` | `Success` |
| 429 Too Many Requests | `Retry`, after `Retry-After` | `Success` |
| 500 Internal Server Error | `Stop` | `Failure` |
| 502 Bad Gateway, 504 Gateway Timeout | `Retry` | `Failure` |
| 503 Service Unavailable | `Retry`, after `Retry-After` | `Failure` |
| any other 5xx, such as 501 Not Implemented | `Stop` | `Success` |
| a server certificate the client doesn't trust | `Stop` | `Failure` |
| any other error, such as refused, reset or `AttemptTimeout` | `Retry` | `Failure` |

A 4xx is the dependency working: it answered, and the request was wrong, so
another attempt won't fix it, and the dependency counts as healthy. 408 and
429 are retried, and don't count against it: a 408 says the server gave up
waiting for the request, which RFC 9110 says a client may repeat, and a 429
is the server's own flow control, which says when to come back. A 500 counts
against the dependency but isn't retried: it is usually the server's bug,
which a second attempt meets again. 502, 503 and 504 say the dependency, or
what is in front of it, is unavailable, which is what retries are for. A
certificate the client doesn't trust is met again by every attempt. The
default reads only the status and the headers, and never a body.

### Writing a `Classify`

An end user whose API means something else by an answer writes a `Classify`
of their own, which usually handles what differs and hands the rest to the
default. This ledger answers 409 while another write to the account is in
flight, which is over by the next attempt:

```go
d := httpclient.DefaultSettings()
classify := d.Classify
settings.Classify = func(resp *http.Response, err error) httpclient.Classification {
	if resp != nil && resp.StatusCode == http.StatusConflict {
		return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
	}
	return classify(resp, err)
}
```

A 409 is then retried, as a `GET` or another request that is safe to send
twice, and counts for the dependency rather than against it.

**A `Classify` may read the body,** for an API that answers 200 with an error
in its body. It closes the body it read, and leaves in `resp.Body` what
service code is to read, such as the bytes it read:

```go
settings.Classify = func(resp *http.Response, err error) httpclient.Classification {
	if err != nil {
		return classify(resp, err)
	}
	b, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if readErr != nil {
		return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}
	}
	if bytes.Contains(b, []byte(`"error"`)) {
		return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
	}
	return classify(resp, err)
}
```

Its read counts against `Timeout`, but not against `AttemptTimeout`, which
ends with the headers. [The response body](#the-response-body)'s rules apply
to the body it leaves, but not to its own read: `io.ReadAll` holds the whole
body in memory, so a `Classify` for an API whose bodies can be large reads
through an `io.LimitReader`. The client counts what this `Classify` returns,
so a read that fails because the caller gave up counts as the `Failure` it
returns here. A `Classify` for which that matters returns `circuit.Ignored`
when `resp.Request.Context().Err()` is `context.Canceled`.

**One `Classify` serves every API that shares a convention,** such as a
company's own services. The function is the end user's own, so it is
written once and set on the client of each:

```go
// The APIs behind the gateway share a convention: a 409 says another
// write to the account is in flight, which is over by the next
// attempt. One Classify says so, and is set on the client of each.
classify := httpclient.DefaultSettings().Classify
ourAPIs := func(resp *http.Response, err error) httpclient.Classification {
	if resp != nil && resp.StatusCode == http.StatusConflict {
		return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
	}
	return classify(resp, err)
}
paymentsSettings.Classify = ourAPIs
ledgerSettings.Classify = ourAPIs
```

`Classify` is called on the goroutine that made the call, once for each
attempt, so it must be safe for concurrent use. A `Classification` with a
zero `Outcome`, or with `retry.Retry` and a negative `RetryAfter`, panics, as
bedrock's classifiers do. The client recovers nothing: the panic passes to
the goroutine that made the call, once the attempt's response is closed, so
its connection isn't held. humus ships no classifiers for well-known APIs,
and no helpers to build one.

## `Retry-After`, `Timeout` and `AttemptTimeout`

### `Retry-After`

The default `Classify` reads the `Retry-After` header of the 429s and 503s
it retries, and returns it as the `Classification`'s `RetryAfter`. Both of
RFC 9110's forms are read: a number of seconds, which is used as it is, and
an HTTP-date, which is measured from now and clamped at 0. A malformed
header is ignored. The wait before the next attempt is the longer of
`RetryAfter` and the backoff. An end user's `Classify` returns whatever its
API says, such as a header of its own.

**A `RetryAfter` longer than `BackoffMax` isn't waited for,** from any
`Classify`: the call ends, and service code gets the response as it came. A
server that asks for a minute is not one a call in flight should wait on. A
wait that would end after the call's deadline isn't started either: the call
returns the last response at once.

### Timeout and AttemptTimeout

**`Timeout` bounds a whole call:** every attempt, the waits between them,
and reading the response's body. It is the `http.Client`'s `Timeout`, which
`net/http` puts on the request's context as a deadline, unless the caller's
context has an earlier one, which then wins. So the retries see the
deadline, and don't start a wait that would end after it. It covers reading
the body, so a client that streams large bodies raises it. `CheckTimeout`
rejects a `Timeout` that is not positive: a client with no bound on a call
holds a goroutine and a connection for as long as a dependency hangs.

**`AttemptTimeout` bounds one attempt,** from dialling until the response's
headers arrive. The attempt's context is cancelled after it, with a cause
that wraps `context.DeadlineExceeded`, and the attempt fails with an error
that wraps it too. The call's context is still live, so the error reaches
`Classify`, and the default retries it, as any failed attempt. With the
defaults, an attempt to a dependency that hangs fails after 5s, which leaves
time for a second before `Timeout`'s 10s. The second has less than 5s left,
so `Timeout` ends it, and the call, before `AttemptTimeout` would: against a
dependency that hangs, the defaults make two attempts, not `MaxAttempts`'
three. A client that wants every attempt sets `Timeout` above
`AttemptTimeout` times `MaxAttempts`, plus the waits. `AttemptTimeout` stops counting
once the headers arrive, so a slow body isn't cut off: that is `Timeout`'s
to bound.

## The circuit breaker

Each client has one circuit breaker,
[bedrock's `circuit.Breaker`](https://pkg.go.dev/github.com/z5labs/bedrock/circuit),
which guards its dependency: while the dependency keeps failing, the client
fails calls at once, rather than hold every caller for its timeouts and
retries, and a probe now and then finds out when the dependency is back. The
breaker sits inside the retries, so each attempt is a call it admits or
rejects.

What counts is each attempt's result, as the `Breaker` outcome of its
`Classification`: `Success`, `Failure`, or `Ignored` for a result that says
nothing about the dependency. The client decides some of it itself:

- an error once the caller cancelled the call's context is `Ignored`;
- an error once the call's deadline passed, `Timeout` or one the caller set,
  is a `Failure`: a dependency that hangs ends every call this way, and a
  breaker that ignored deadlines would never open;
- a response's result is counted with its body, with its `Classification`'s
  `Breaker`, once service code reads the body to the end or closes it. A
  read that fails is a `Failure` instead, whatever `Classify` said of the
  status, since the dependency sent its headers and then failed, or
  `Ignored` when the caller cancelled the call's context. A body the client
  buffers before a retry is counted then.

**The default opens after 5 failures in a row, or once half of the last 20
or more results failed.** It then rejects every attempt for `OpenFor`, 10s,
and then lets one probe through at a time: a failed probe reopens it, and 3
successful ones close it. `Settings.Breaker` configures, from
configuration, the `Window` of results the policy sees, how many `Probes`
may be in flight at once, 1 by default, and the durations `OpenFor` and
`ProbeTimeout`. `Settings.BreakerPolicy` decides when it opens and closes,
in code: its `Trip` decides the 5 and the half of 20, and its `Probe` the 3
successful probes. A policy whose `Trip` always
returns false turns the breaker off:

```go
settings.BreakerPolicy.Trip = func(circuit.Counts) bool { return false }
```

The policy runs with the breaker locked, on the goroutine of the call whose
result it decides on, so it must be fast and safe for concurrent use. A
panic in it passes to that goroutine, and `circuit` leaves the breaker
consistent.

**An attempt the breaker rejects sends nothing, and isn't retried,** which
would defeat the breaker. `Classify` isn't asked about it. The call returns
at once, with an error that wraps a `*circuit.RejectedError`, through
`http.Client`'s `*url.Error`. `errors.Is(err, circuit.ErrRejected)` tells
service code nothing was sent, and the `RejectedError`'s `RetryAfter` says
how much longer the breaker stays open.

A body that service code never closes leaves its result uncounted. That
costs nothing while the breaker is closed. While it is half-open, the
attempt was a probe, whose slot the body holds until it is read or closed,
or for at most `Breaker.ProbeTimeout`, after which another probe may go.
`net/http` asks for every body to be closed anyway.

### Its log records

The client logs through the Setup's `LoggerProvider`, with the logger
`github.com/z5labs/humus/httpclient`, one record for each change of the
breaker's state, in the context of the call that made it:

| Record | Severity | When |
|---|---|---|
| `circuit breaker open` | WARN | the breaker opens, from closed or from half-open |
| `circuit breaker half-open` | INFO | `OpenFor` has passed, and the breaker lets a probe through |
| `circuit breaker closed` | INFO | enough probes succeeded |

Each has the dependency's `server.address` and `server.port`, the state the
breaker left as `humus.circuit.previous_state`, and the counts of that
state: `humus.circuit.successes`, `humus.circuit.failures`,
`humus.circuit.consecutive_successes`, `humus.circuit.consecutive_failures`
and `humus.circuit.timed_out`. The client logs nothing else: an error is
returned to service code, not logged.

## The response body

Before another attempt follows, the client reads the body of the response it
replaces into memory, up to 64 KiB, and closes it, before the wait. Closing
it ends the attempt's span, counts its result in the breaker, and frees its
connection during the backoff. If the wait is cut short, such as by the
caller's context, the buffered response is what the call returns.

**A body larger than 64 KiB isn't retried:** the call returns the response
with its body intact. Error bodies are small, so 64 KiB keeps every one the
client retries, and bounds what each call in flight holds.

A `Classify` that reads the body leaves what service code reads in
`resp.Body`, and these rules apply to the body it leaves. Service code reads
or closes every body, as `net/http` asks, which is also what counts the
result in [the breaker](#the-circuit-breaker).

## HTTP/2 behind a Kubernetes Service

For an `https` BaseURL, the client offers HTTP/2 and then HTTP/1.1 in ALPN,
so every server that offers `h2` gets HTTP/2, a humus server set to
`HTTP2Only` among them. An `http` BaseURL is HTTP/1.1 alone, since humus
serves no unencrypted HTTP/2 (h2c).

**HTTP/2 puts every call on one connection.** A Kubernetes ClusterIP Service
balances connections, not requests: kube-proxy picks a pod when a
connection opens, and every request on it goes to that pod. So one client's
calls reach one pod of the dependency until that connection closes, and the
calls spread across the dependency's pods only as its callers' connections
do. Balancing each request across pods is out of humus's scope: a mesh, or a
proxy that balances requests, does it.

**A humus server's shutdown moves the calls during a rollout.** When a pod
of the dependency is stopped, an `httpserver` Service keeps serving through
its [drain](/humus/pr-preview/pr-900/http/#the-defaults), so the calls already on its
connection are still answered while Kubernetes removes the pod from the
Service's endpoints. Its `Shutdown` then sends the client a GOAWAY, and
waits for the calls in flight. The client opens its next connection through
the Service, which reaches a pod still in its endpoints, and its calls move
there. The same holds when the dependency scales up: a new pod gets a
client's calls only once the client's connection closes, at a pod's
shutdown or once it has been idle for 90s.

## Telemetry

The client traces and measures every attempt with the Setup's telemetry,
through OpenTelemetry's `net/http` instrumentation,
[`otelhttp`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp),
given the `TracerProvider`, the `MeterProvider` and the `Propagator` of
`s.Telemetry`. Nothing reaches OpenTelemetry's globals: a process that sets
them sees none of the client's spans or metrics.

**Each attempt is one span of kind CLIENT,** every retry and redirect
included, a child of the span in the request's context, such as a handler's
request span or a job's run span. A slow or failing dependency shows up in
the caller's trace. A request to another origin is refused before it is an
attempt, so it has no span.

- **The span is named after the method,** such as `GET`. The HTTP semantic
  conventions name a client span after its method and URL template, and a
  client has no template, only whatever URL service code built, which would
  make a span name per resource.
- **It carries `otelhttp`'s attributes,** among them `url.full`, with any
  user info removed, `server.address`, `server.port`,
  `http.response.status_code` and `network.protocol.version`, which is `2.0`
  for HTTP/2. Its status is `Error` for a 4xx or 5xx response, and for an
  attempt that fails with no response, such as a refused connection.
- **An attempt that is not a call's first carries
  `http.request.resend_count`,** which `otelhttp` doesn't set. A retry and a
  redirect each count, as the semantic conventions count them.
- **An attempt the breaker rejects still has its span,** which ends at once
  with status `Error` and `error.type` `*circuit.RejectedError`.

**The Setup's propagator puts the attempt's trace context and the request
context's baggage in each attempt's headers,** so a dependency served with
`httpserver` joins the caller's trace, with the attempt's span as its server
span's parent, and its handler sees the caller's baggage.

**`otelhttp` records `http.client.request.duration` and
`http.client.request.body.size` for each attempt,** in its own scope, with
the status code, or `error.type` for an attempt that failed with no
response. A rejected attempt is among them, with an `error.type` that
`otelhttp` writes differently from the span's: the package path,
`github.com/z5labs/bedrock/circuit.RejectedError`, where the span has
`*circuit.RejectedError`. A query or an alert on rejections matches the
form of the signal it reads.

The breaker's state changes are the client's only log records, as
[Its log records](#its-log-records) lists.

### humus's own instruments

The client records four instruments of humus's own, for its breaker and its
retries, with the Setup's `MeterProvider`, in the scope
`github.com/z5labs/humus/httpclient`, the scope of its log records:

| Instrument | Kind | Unit | Attributes | Recorded |
|---|---|---|---|---|
| `humus.circuit.state` | asynchronous UpDownCounter | `{breaker}` | `humus.circuit.state`: `closed`, `open` or `half-open` | at each collection: 1 for the breaker's state, 0 for the other two |
| `humus.circuit.transitions` | Counter | `{transition}` | `humus.circuit.state`, the state it went to, and `humus.circuit.previous_state`, the one it left | at each change of the breaker's state, beside its log record |
| `humus.circuit.calls` | Counter | `{call}` | `humus.circuit.outcome`: `success`, `failure`, `ignored` or `rejected` | as each attempt's result counts in the breaker, or as the breaker rejects it |
| `humus.retry.decisions` | Counter | `{decision}` | `humus.retry.decision`: `again`, `succeeded`, `stopped`, `exhausted`, `throttled` or `expired` | after each attempt |

Every data point also carries the dependency's `server.address` and
`server.port`, of `BaseURL`'s origin, the same as the breaker's log records,
and nothing per call, such as a method or a route: `otelhttp`'s instruments
break calls down already. Each other attribute's value is the `String()` of
bedrock's own type, `circuit.State`, `circuit.Outcome` or `retry.Decision`.

- **`humus.circuit.state` is read, not tracked.** At each collection it reads
  the breaker's state once, so it shows `half-open` as soon as `OpenFor` has
  passed, with no call made. It is a count of breakers in each state, so two
  clients of one origin add up, as `closed` 1 and `open` 1. The App's last
  collection, after the `OnStop` functions, still reads it, so the final
  export has the state the App stopped in.
- **`humus.circuit.transitions` catches the short opens.** A breaker can
  open, go half-open and close between two collections, a minute apart by
  default, and `humus.circuit.state` never shows it open.
- **`humus.circuit.calls` counts an attempt when the breaker does:** a
  response's result once service code reads its body to the end or closes
  it. It is the only place the verdicts of the end user's `Classify` show:
  a 409 counted as a `failure` is a 4xx in `otelhttp`'s instruments. An
  attempt the breaker rejects is `rejected`, and is in
  `http.client.request.duration` too.
- **`humus.retry.decisions` counts what `retry` decided,** so `throttled`
  counts the retries the budget refused, `exhausted` the calls that used
  every attempt, and `expired` the calls whose deadline came before the next
  attempt could start. A rejected attempt is `stopped`. `again` is not yet
  what the client did: no attempt follows a request that isn't safe to
  repeat, a response whose body doesn't fit the 64 KiB buffer, or a call
  whose context ends during the wait.

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

## Testing

A service that calls a dependency is tested whole, inside
`testing/synctest`, as [Testing a humus App](/humus/pr-preview/pr-900/testing/)
describes for any App. The client dials through the Setup's Host, so the
fake dependency is a second App on the same humustest Host: an
`httpserver` Service whose handler answers as the dependency would, with a
certificate from a CA that [`humustest.NewCA`](/humus/pr-preview/pr-900/testing/#tls)
generates for the test. `httpclient`'s own tests start one with a helper:

```go
// startPaymentsWith is startPayments, with the dependency's telemetry built
// by tel.
func startPaymentsWith(t *testing.T, h *humustest.Host, host humus.Host, tel telemetry.Builder, ca *humustest.CA, handler http.Handler) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "payments")
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(tel)))
	<-h.Listening("payments:8443")
	return func() {
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}
```

It listens on `:8443`, every name on the Host's network, so the client's
dial of `payments:8443` reaches it, and its certificate is for `payments`,
the host the client's `BaseURL` names, so the client's verification passes.
`startPayments` is the same with no telemetry.

The end user's whole App then runs against it, with the end user's own
`Init` above, and the environment production gives it. The test writes the
App's certificate and the dependency's CA into the Host's filesystem with
`humustest.WithFiles`, and names them with `humustest.WithEnv`, as
[Giving the App its certificate](/humus/pr-preview/pr-900/http/#giving-the-app-its-certificate)
describes. `keyPairPEM` and `caPEM` are helpers of the test file that
PEM-encode them: `keyPairPEM` a certificate's chain and private key, and
`caPEM` the CA certificate that issued a certificate. `orders` and the
dependency's certificate are both issued by `ca`, so `caPEM(orders)` is the
dependency's CA too:

```go
synctest.Test(t, func(t *testing.T) {
	ca := humustest.NewCA(t)
	orders := ca.Issue(t, "orders")
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"tls.pem":         keyPairPEM(t, orders),
		"payments-ca.pem": caPEM(orders),
	}))
	stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "charge for "+r.URL.Path+": paid")
	}))
	defer stop()

	p, status := humustest.Start(t, Init,
		humustest.WithApp(httpserver.Defaults()),
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(map[string]string{
			"ADDR":             ":9443",
			"TLS_PEM_FILE":     "tls.pem",
			"PAYMENTS_URL":     "https://payments:8443/v1",
			"PAYMENTS_CA_FILE": "payments-ca.pem",
		}),
	)
	<-h.Listening("orders:9443")

	c := h.HTTPClient()
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	resp, err := c.Get("https://orders:9443/orders/7/payment")
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", b)
	require.Equal(t, "charge for /v1/charges/7: paid", string(b))

	<-p.Notified(syscall.SIGTERM)
	p.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
})
```

The request reaches the App, the App's handler calls the fake dependency
through the client, and the answer comes back, all on the Host's in-memory
network, in no real time. The client trusts the CA through `RootCAs`, so the
test reads no system root and skips no verification.

Fake time makes the client's waits free too. A handler that answers 503
with `Retry-After: 2` the first time costs the test 2s of fake time and no
real time, and a dependency that hangs fails the attempt after
`AttemptTimeout` just as instantly, so a test of retries, timeouts or the
breaker opening runs as fast as one of a single call. A test that asserts
on the client's spans, metrics or breaker records gives the App
`humustest.NewTelemetry()`'s `Builder` rather than `telemetry.NoopBuilder`.
