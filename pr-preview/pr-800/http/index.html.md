# Serving HTTP

The [`httpserver`](https://pkg.go.dev/github.com/z5labs/humus/httpserver)
package serves an `http.Handler` over TLS as a `humus.Service`. The end
user's `Init` sources its `Settings`, builds the Service with
`httpserver.NewService` and returns it, and `httpserver.Main` runs it with
the defaults an HTTP server behind a load balancer needs: a 5s drain and a
10s grace period. Its test package,
[`httpservertest`](https://pkg.go.dev/github.com/z5labs/humus/httpserver/httpservertest),
runs the same App in a test, on the same defaults, with an HTTP/2 client.

The package is the server half of what `rest.Run` did before the reset,
without the REST DSL. It exports no App, runs no health Service and serves
no probes. The design behind everything on this page is in
[Proposal: HTTP server foundation](https://github.com/z5labs/humus/discussions/680),
with its [revision 1](https://github.com/z5labs/humus/discussions/680#discussioncomment-18988464)
and [revision 2](https://github.com/z5labs/humus/discussions/680#discussioncomment-18988543).

## A service

The end user's `Init` sources `httpserver.Settings` with
[`config.Struct`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Struct),
as [Configuring a humus service](/humus/pr-preview/pr-800/configuration/) describes,
builds the handler, and returns the Service that `NewService` builds from
both:

```go
// Init is the end user's Init: it sources the Settings, builds the Service
// with NewService and returns it, with any other Service the process runs.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
		return httpserver.Settings{
			// No .Default: Addr and TLS are required, so an unset ADDR or
			// TLS_PEM_FILE fails as not found.
			Addr:              r.Get(env("ADDR").Validate(httpserver.CheckAddr)),
			TLS:               r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS)),
			HTTP2Only:         r.Get(env("HTTP2_ONLY").Parse(strconv.ParseBool).Default(d.HTTP2Only)),
			ReadHeaderTimeout: r.Get(env("READ_HEADER_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadHeaderTimeout).Validate(httpserver.CheckReadHeaderTimeout)),
			ReadTimeout:       r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(httpserver.CheckReadTimeout)),
			WriteTimeout:      r.Get(env("WRITE_TIMEOUT").Parse(time.ParseDuration).Default(d.WriteTimeout).Validate(httpserver.CheckWriteTimeout)),
			IdleTimeout:       r.Get(env("IDLE_TIMEOUT").Parse(time.ParseDuration).Default(d.IdleTimeout).Validate(httpserver.CheckIdleTimeout)),
			MaxHeaderBytes:    r.Get(env("MAX_HEADER_BYTES").Parse(strconv.Atoi).Default(d.MaxHeaderBytes).Validate(httpserver.CheckMaxHeaderBytes)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", greet)

	svc, err := httpserver.NewService(s, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}
```

The variable names are the service's own: humus defines none of them.
`readFile` and `serverTLS` are the end user's helpers too, shown under
[TLS](#tls).

The service's `main` is one call to `httpserver.Main`, here with a telemetry
`Builder` that names the service, as [Telemetry](/humus/pr-preview/pr-800/telemetry/#writing-the-builder)
describes:

```go
httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
```

`NewService` runs every check on the `Settings` again, rejects a nil
handler, and then listens on `Settings.Addr` through the Setup's Host, in
`Init`. So a port that is already taken fails `Init`, and the App exits 1,
and a connection that arrives before `Serve` runs waits in the backlog
rather than being refused. The listener's close is registered with
`s.OnStop`, so an `Init` that fails after `NewService` still closes it. An
`Addr` with port 0 listens on a free port, which `Service.Addr` reports.

`Init` returns the HTTP Service along with any other Service the process
runs, such as a job or a queue consumer next to the API, or the
[health Service](/humus/pr-preview/pr-800/health/#the-health-service). Any Service
returning ends the App.

## Settings

`httpserver.Settings` follows the
[settings convention](/humus/pr-preview/pr-800/configuration/#what-a-package-with-settings-exports):
plain Go values, a `DefaultSettings()`, and a check for each field with a
valid range. The package reads no environment variable and defines no
variable name.

| Field | Default | Valid | Check |
|---|---|---|---|
| `Addr` | none: required | `host:port`, with a port from 0 to 65535 | `CheckAddr` |
| `TLS` | none: required | a config that has a certificate and that HTTP/2 can use | `CheckTLS` |
| `HTTP2Only` | `false` | either | none |
| `ReadHeaderTimeout` | `2s` | not negative | `CheckReadHeaderTimeout` |
| `ReadTimeout` | `5s` | not negative | `CheckReadTimeout` |
| `WriteTimeout` | `10s` | not negative | `CheckWriteTimeout` |
| `IdleTimeout` | `120s` | not negative | `CheckIdleTimeout` |
| `MaxHeaderBytes` | `1 << 20`, 1 MiB | positive | `CheckMaxHeaderBytes` |

- **The timeouts keep `net/http`'s meaning of zero.** A zero
  `ReadTimeout` or `WriteTimeout` means no timeout, and a zero
  `ReadHeaderTimeout` or `IdleTimeout` means `ReadTimeout` applies, so their
  checks accept zero.
- **`MaxHeaderBytes` must be positive.** In `net/http` a zero means 1 MiB,
  a default applied to a zero value, so `CheckMaxHeaderBytes` rejects it
  rather than letting zero mean something other than zero.
- **`HTTP2Only` has no check,** since both values are valid. See
  [Protocols](#protocols).

**`Addr` and `TLS` are required.** `DefaultSettings()` leaves `Addr` `""` and
`TLS` nil, which `CheckAddr` and `CheckTLS` reject, so humus imposes no port
and no certificate. The end user's chains for them have no `.Default`, as in
`Init` above, so a variable that isn't set fails as not found. The App exits
78, and `config.Struct` names every variable at fault in one error. With
neither set:

```
init: config ADDR: not found
config TLS_PEM_FILE: not found
```

`NewService` runs the same checks itself, so a `Settings` built in code
with a bad field fails `Init` too, naming the field, such as
`Settings.TLS must not be nil`. That error is no `*config.Error`, so the App
exits 1: the fault is in the code, not in the configuration.
[How a bad value fails](/humus/pr-preview/pr-800/configuration/#how-a-bad-value-fails)
explains the exit codes.

## TLS

**TLS is required.** The Service serves HTTPS with `Settings.TLS` and
nothing in plain text. There is no plain-HTTP setting and no self-signed
fallback: a service that hasn't sourced a certificate doesn't start, rather
than starting and failing every request.

### The certificate

The end user decodes the certificate from a file, read through `s.Host.FS`
so that a test reads it from its own directory and production from the real
disk, as [Reading a file](/humus/pr-preview/pr-800/configuration/#reading-a-file)
describes. How the certificate is stored and encoded is the end user's
decision. `Init` above reads one PEM file that holds the certificate chain
and the private key:

```go
// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.FS) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

// serverTLS returns a server's TLS config for a PEM file that holds its
// certificate chain and its private key.
func serverTLS(pem []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(pem, pem)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}
```

A PKCS#12 bundle needs a password as well. This chain reads the bundle from
the file `TLS_PKCS12_FILE` names, and decodes it with the password in
`TLS_PKCS12_PASSWORD`, so a missing file, a missing password and a wrong
one each fail as a config error with exit 78:

```go
settings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
	settings := httpserver.DefaultSettings()
	settings.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
	settings.TLS = r.Get(env("TLS_PKCS12_FILE").Parse(readFile(s.Host.FS)).FlatMap(func(bundle []byte) config.Value[*tls.Config] {
		return env("TLS_PKCS12_PASSWORD").Parse(pkcs12TLS(bundle))
	}).Validate(httpserver.CheckTLS))
	return settings
}).Read(ctx)
```

`pkcs12TLS` decodes the bundle with
[go-pkcs12](https://pkg.go.dev/software.sslmate.com/src/go-pkcs12), which
humus doesn't depend on:

```go
// pkcs12TLS returns a parser for config.Value.Parse that decodes bundle,
// a PKCS#12 bundle, with the password it is given, into a server's TLS
// config.
func pkcs12TLS(bundle []byte) func(password string) (*tls.Config, error) {
	return func(password string) (*tls.Config, error) {
		key, leaf, chain, err := pkcs12.DecodeChain(bundle, password)
		if err != nil {
			return nil, err
		}
		cert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
		for _, c := range chain {
			cert.Certificate = append(cert.Certificate, c.Raw)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
	}
}
```

A certificate that rotates on disk, or one from autocert, is served from a
config that sets `GetCertificate` instead of `Certificates`.

### What `CheckTLS` rejects

`CheckTLS` fails startup on a config that would otherwise fail only once
`Serve` runs, or on every HTTP/2 connection. It rejects:

- **nil**, which is what `DefaultSettings()` leaves `TLS`;
- **a config with no certificate:** none of `Certificates`,
  `GetCertificate` and `GetConfigForClient` set, which `ServeTLS` would
  reject with an error about a file named `""`;
- **a `MaxVersion` below TLS 1.2**, with which Go's HTTP/2 server refuses
  every connection with `INADEQUATE_SECURITY`, as RFC 9113 requires;
- **`CipherSuites` that HTTP/2 can't use:** a list that has neither
  `TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256` nor
  `TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256`, with a `MinVersion` below
  TLS 1.3.

It accepts a config with only `GetCertificate` set, and `CipherSuites`
without those two suites when `MinVersion` is TLS 1.3, which ignores
`CipherSuites`. Like every check, its error doesn't name the field: the
chain that sourced the config names its variable.

### Behind a proxy

Traffic is encrypted all the way to the pod, whatever sits in front of it.
So a load balancer, an ingress controller or a mesh sidecar in front of the
pod connects to it over TLS: an AWS ALB target group's protocol is HTTPS,
and an ingress controller is told that its backend speaks HTTPS, such as
ingress-nginx with its `nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"`
annotation. That holds behind a mesh that already encrypts between pods
too: every service needs a certificate. Whether the proxy verifies the
pod's certificate, and against which CA, is the proxy's configuration.

A proxy that connects in plain HTTP gets a `400 Bad Request` and the
Service logs a TLS handshake error, as [Telemetry](#telemetry) shows.

## Protocols

**HTTP/2 is always on and preferred.** Every client that offers HTTP/2 in
ALPN gets it, and a client that doesn't falls back to HTTP/1.1. The Service
sets `http.Server.Protocols` itself, so HTTP/2 doesn't depend on
`net/http`'s defaults or on the config's `NextProtos`. `GODEBUG=http2server=0`
still turns HTTP/2 off in `net/http`, whatever the Service sets: that is an
operator overriding Go, and the Service doesn't second-guess it.

### The ALPN order

The Service serves a clone of `Settings.TLS`, so the end user's config is
never changed, and sets the clone's ALPN protocols in this order:

1. `h2`;
2. `http/1.1`, unless `HTTP2Only` is set;
3. every other protocol the config's `NextProtos` lists, in its order.

The server's order wins in ALPN. `net/http` on its own adds `h2` and
`http/1.1` after whatever a config already lists, so a config that listed
`http/1.1` would prefer HTTP/1.1. The Service puts `h2` first whatever the
config lists. A protocol other than these two, such as autocert's
`acme-tls/1`, is still offered, after them: a client that offers only
`acme-tls/1` gets it, and one that offers `acme-tls/1` and `h2` gets `h2`.

### `HTTP2Only`

With `Settings.HTTP2Only`, the Service serves HTTP/2 alone. A client that
offers only HTTP/1.1, or no ALPN at all, completes the TLS handshake and is
then disconnected before a request is read: it sees its connection closed
with no response, and the Service logs nothing.

It is off by default because that failure is silent, and some clients speak
only HTTP/1.1:

- **an AWS ALB target group on its default protocol version,** HTTP1;
- **a Go client with its own `Transport`** that sets a `TLSClientConfig` or
  a dialer of its own, and sets neither `ForceAttemptHTTP2` nor
  `Protocols`. Such a `Transport` negotiates only HTTP/1.1.

It is a `Settings` field, so an operator turns it on from the manifest, with
`HTTP2_ONLY=true` in `Init` above, once every client speaks HTTP/2.

### No h2c

TLS is required, so there is no plain text to serve unencrypted HTTP/2
(h2c) on, and no setting for it. A client that sends h2c with prior
knowledge gets no response, and the Service logs a TLS handshake error.

## Shutdown

The Service follows the
[two shutdown signals](/humus/pr-preview/pr-800/lifecycle/#service) of a humus App:

- **`Serve`'s context being done changes nothing.** It is done as soon as
  the App starts shutting down, and the Service keeps serving through the
  App's [drain](/humus/pr-preview/pr-800/lifecycle/#the-drain), while load balancers
  stop sending it traffic. A request that arrives during the drain is
  served, and an in-flight request finishes.
- **`Shutdown` stops accepting connections,** closes idle ones and waits
  for in-flight requests, within the grace period. When the grace period
  runs out first, it calls `Close`, which closes every connection and so
  cancels the context of each request still in flight. The App then exits 1
  with a `*humus.GracePeriodError`.

### Request contexts

A request's context, `r.Context()`, doesn't derive from `Serve`'s context,
which is done as soon as shutdown starts, so a request that arrives during
the drain is still served. It is done when the client goes away, or when
`Close` closes its connection at the end of the grace period.

**A handler returns once `r.Context()` is done.** One that ignores it
outlives the App once its connection is closed: the process exits under it.
In a test that runs the App inside `testing/synctest`, synctest reports its
goroutine as a leak, still blocked when the test ends, which is how the
end user's tests find such a handler. A connection a handler hijacks, such
as a WebSocket, isn't tracked by `Shutdown` or by `Close`: the handler
closes it itself.

## The defaults

`httpserver.Main` is `humus.Main` with `httpserver.Defaults()` applied
before the end user's options, and does nothing else. `Defaults` is one
`humus.Option` that sets two of `humus.Main`'s
[defaults](/humus/pr-preview/pr-800/lifecycle/#the-defaults), and leaves the Host and
the telemetry `Builder` as `humus.Main`'s:

- **A drain of `humus.DrainDelay(5 * time.Second)`.** Kubernetes removes a
  terminating pod from a Service's endpoints asynchronously, so for a while
  after SIGTERM, load balancers still route requests to the pod. The
  Service keeps serving through the drain, so those requests are answered
  rather than refused. It keeps serving until `Shutdown`, so its `Serve`
  doesn't return during the drain, and the drain lasts the whole 5s.
- **A grace period of 10s**, rather than `humus.Main`'s 20s, so that the
  drain fits in the same budget.

From SIGTERM, then: the Service serves through the 5s drain; `Shutdown`
then waits up to 10s for in-flight requests and closes what is left; and
the telemetry's shutdown, `telemetry.Settings.ShutdownTimeout`, takes up to
5s more. The worst case is 20s, inside the 25s that `humus.Main` budgets
within Kubernetes' default `terminationGracePeriodSeconds` of 30s. A
service whose requests all finish during the drain exits 5s after SIGTERM,
when the drain ends.

### Overriding them

The end user's options come after `Defaults`, so they win.
`httpserver.Main(Init, humus.WithShutdownGracePeriod(15*time.Second))` has a
15s grace period and still the 5s drain, and `humus.WithDrain(nil)` removes
the drain.

An operator overrides both without a rebuild, with the lifecycle's
[variables](/humus/pr-preview/pr-800/lifecycle/#settings):
`HUMUS_SHUTDOWN_GRACE_PERIOD` sets the grace period, and
`HUMUS_SHUTDOWN_DRAIN_DELAY` replaces the drain with a delay of its value,
whatever `Defaults` or an option set. Each leaves the other as it was: with
`HUMUS_SHUTDOWN_DRAIN_DELAY=1s` alone, the drain is 1s and the grace period
still 10s.

### `terminationGracePeriodSeconds`

When Kubernetes stops a pod, it sends the container SIGTERM, and then
SIGKILL once `terminationGracePeriodSeconds` have passed, 30 by default. A
service that raises its drain or its grace period raises
`terminationGracePeriodSeconds` with them, in the same manifest, to at least
the drain, the grace period and the telemetry's shutdown, plus a margin for
the process to exit. Otherwise SIGKILL ends it first: its container exits
137, its OnStop functions never run, and the telemetry it had not yet
exported is lost.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: greeter
spec:
  selector:
    matchLabels:
      app: greeter
  template:
    metadata:
      labels:
        app: greeter
    spec:
      # 10s drain + 20s grace period + 5s telemetry shutdown, and a margin.
      terminationGracePeriodSeconds: 40
      containers:
        - name: greeter
          image: registry.example.com/greeter:1.0.0
          ports:
            - name: https
              containerPort: 8443
          env:
            - name: ADDR
              value: ":8443"
            - name: TLS_PEM_FILE
              value: /etc/greeter/tls/tls.pem
            - name: HUMUS_SHUTDOWN_DRAIN_DELAY
              value: 10s
            - name: HUMUS_SHUTDOWN_GRACE_PERIOD
              value: 20s
          volumeMounts:
            - name: tls
              mountPath: /etc/greeter/tls
              readOnly: true
      volumes:
        - name: tls
          secret:
            secretName: greeter-tls # tls.pem: the certificate chain and the private key
```

The drain does the job of a `preStop` hook that sleeps, which services
without one add for the same reason. A pod that has both spends the hook's
sleep and the drain out of the same `terminationGracePeriodSeconds`.

### `httpserver.Main` or `humus.Main`

Which `Main` a process calls depends on what a load balancer routes to, as
[the lifecycle's runtime packages](/humus/pr-preview/pr-800/lifecycle/#runtime-packages)
explains:

- **A process uses `httpserver.Main` when a load balancer routes to its
  HTTP server,** an API alone or an API next to a job or a consumer. The
  other Services then get the HTTP server's drain and grace period, 15s in
  all, rather than `humus.Main`'s 20s.
- **A process whose only HTTP server serves probes or metrics calls
  `humus.Main`,** with no drain. Nothing routes to that server, and a drain
  would make a finished [job](/humus/pr-preview/pr-800/jobs/) wait for the whole
  delay.

## Telemetry

The Service takes everything from the Setup's telemetry, and never reads or
sets OpenTelemetry's globals. The handler is served wrapped in
OpenTelemetry's `net/http` instrumentation,
[`otelhttp`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp),
given the `TracerProvider`, the `MeterProvider` and the `Propagator` of
`s.Telemetry`.

### The span and the metrics

**Each request gets one server span.** Its parent is the trace context the
request brought in, extracted with the Setup's propagator, so a request
joins its caller's trace. The propagator comes from the telemetry `Builder`
and decides which headers carry the trace context: the W3C `traceparent`
and `tracestate` headers, and `baggage`, with `telemetry.New`'s.

The span is named after the method and the route pattern, such as
`GET /hello/{name}`, when the handler sets `Request.Pattern`, as an
`http.ServeMux` does. Otherwise it is named after the method alone, such as
`GET`. That is `otelhttp`'s default name, which follows OpenTelemetry's
semantic conventions.

**Each request also records OpenTelemetry's HTTP server metrics,** such as
`http.server.request.duration`, on the Setup's `MeterProvider`.

### The log records

The Service logs through the `LoggerProvider` of the Setup's telemetry, with
the OpenTelemetry Logs API and the logger name
`github.com/z5labs/humus/httpserver`, which is the records' instrumentation
scope:

| Record | Severity | When |
|---|---|---|
| `listening`, with `server.address` set to `Service.Addr` | INFO | `Serve` starts |
| each line `net/http` writes to the server's error log, such as `http: TLS handshake error from 10.0.0.7:51234: EOF` | ERROR | `net/http` reports a failure, such as a TLS handshake error or a failure to accept a connection |
| `handler panicked`, with `exception.type`, `exception.message` and `exception.stacktrace`, in the request's context | ERROR | a handler panics: see [Panics](#panics) |

The lifecycle's own records, such as `serving` and `shutting down`, come
from the App as for any Service.

**`ErrorLog` never reaches standard error.** The Service sets the
`http.Server`'s `ErrorLog` to a logger whose every line is an ERROR record.
Without it, `net/http` would write to the `log` package's default logger,
which is the real standard error, outside the App's telemetry.

### TLS handshake errors from TCP health checks

A load balancer or a probe that checks the port with TCP, such as a
Kubernetes `tcpSocket` probe or a network load balancer's TCP health check,
opens a connection and closes it without a TLS handshake. `net/http` reports
each one as a handshake error, so every check emits an ERROR record such as:

```
http: TLS handshake error from 10.0.0.7:51234: EOF
```

The Service logs every line at ERROR rather than telling these apart,
because telling them apart means matching `net/http`'s message text, which
isn't API. A team bothered by the noise filters it in its log pipeline,
where a change in Go's wording is a filter to update rather than a release
of humus. With the OpenTelemetry Collector's
[filter processor](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/processor/filterprocessor),
for example:

```yaml
processors:
  filter/tcp-health-checks:
    error_mode: ignore
    logs:
      log_record:
        - instrumentation_scope.name == "github.com/z5labs/humus/httpserver" and IsMatch(body, "^http: TLS handshake error from .*: EOF$")
```

Matching on the scope as well as the text drops only the Service's own
records. A probe that checks the port with an HTTPS request, such as an
`httpGet` probe with `scheme: HTTPS`, causes no handshake error.

## Panics

**A handler that panics aborts its request, which is recorded but isn't
answered.** The Service recovers the panic and:

- records it on the request's span as an `exception` event, with its type,
  message and stack trace, and sets the span's status to `Error`;
- emits one ERROR record, `handler panicked`, with the same attributes, in
  the request's context, so the record carries the span's trace and span
  IDs;
- panics again with `http.ErrAbortHandler`, so `net/http` aborts the
  response without logging it a second time.

**No 500 is written,** and no header the handler hadn't written. Over
HTTP/2 the client sees its stream reset, and over HTTP/1.1 its connection
closed, even when the handler had already written part of its response. A
500 would turn a bug into an ordinary error response, counted with every
other 5xx. Nothing reaches standard error, and the Service keeps serving
every other request.

A protocol that defines an error response, such as REST, recovers in its own
handler and writes that response itself, before the Service sees the panic.
A handler that wants to abort a response without it being logged or
recorded panics with `http.ErrAbortHandler` itself, which the Service passes
through untouched.

## Testing

An HTTP service is tested whole, inside `testing/synctest`, as
[Testing a humus App](/humus/pr-preview/pr-800/testing/) describes for any App, with
two functions from `httpservertest`:

- **`httpservertest.Start`** is `humustest.Start` with `httpserver.Defaults()`
  applied before the test's options, and does nothing else. A test of a
  `main` that calls `httpserver.Main` runs with the drain and the grace
  period production runs with, and can't forget them. The test passes its
  Host, its telemetry and its environment as it would to `humustest.Start`,
  and options given with `humustest.WithApp` come after the defaults, so they
  win.
- **`httpservertest.NewClient(h, roots)`** returns `h.HTTPClient()` with a TLS
  config that trusts only `roots`, and a `Transport` whose `Protocols` is
  HTTP/2 alone. Every request it makes is HTTP/2, so a Service that stopped
  offering `h2` fails the test rather than falling back to HTTP/1.1
  unnoticed. Like `h.HTTPClient()`, it keeps keep-alives off.

### A whole service

This test runs a handler on the package's defaults, over HTTP/2, with a
certificate from a CA that [`humustest.NewCA`](/humus/pr-preview/pr-800/testing/#tls)
generates for the test, so no key or certificate is ever written into the
repository. `serves` is a helper of the test file that returns an `Init`
whose only Service serves the handler on `DefaultSettings()` on `:8443`,
over TLS with the certificate:

```go
func TestMain_serves_HTTP2_and_records_the_span(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /hello/{name}", greet)

		p, status := httpservertest.Start(t, serves(ca.Issue(t, "localhost"), mux),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening("localhost:8443")

		resp, err := httpservertest.NewClient(h, ca.Pool()).Get("https://localhost:8443/hello/humus")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, 2, resp.ProtoMajor)
		require.Equal(t, "hello, humus", string(body))

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		spans := tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, "GET /hello/{name}", spans[0].Name())
		require.Empty(t, p.Stderr())
	})
}
```

Fake time means the drain costs the test nothing: the App exits 5s after
SIGTERM, in no real time at all. A handler blocked on `r.Context()` sees it
cancelled 15s after SIGTERM, once the drain and the grace period are both
up, or 7s after with
`humustest.WithApp(humus.WithShutdownGracePeriod(2*time.Second))`.

### Giving the App its certificate

`Start` makes no CA and no certificate: how the end user's `Init` sources
its certificate, from which variable and in which encoding, is the end
user's decision. The test gives it one the way production does. It issues a
certificate from its CA, writes it into the Host's filesystem with
`humustest.WithFiles`, and sets the variables that name the file, and any
password, with `humustest.WithEnv`. For an `Init` that reads the PKCS#12
bundle [above](#the-certificate):

```go
ca := humustest.NewCA(t)
bundle, password := ca.Issue(t, "localhost").PKCS12(t)
h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{"tls.p12": bundle}))

p, status := httpservertest.Start(t, InitPKCS12,
	humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
	humustest.WithEnv(map[string]string{
		"ADDR":                ":8443",
		"TLS_PKCS12_FILE":     "tls.p12",
		"TLS_PKCS12_PASSWORD": password,
	}),
)
<-h.Listening("localhost:8443")
```

Each certificate has a new random key, and each bundle a new random
password, so nothing secret is fixed in the test. A test that builds the
`httpserver.Settings` itself, as `serves` does, sets their TLS config from
`cert.TLS()` instead. Either way, the test passes the CA's `Pool()` to
`NewClient`.

The same `Init` with no environment at all checks that `Addr` and `TLS` are
required:

```go
func TestInit_without_an_address_or_a_certificate_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := httpservertest.Start(t, Init)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config ADDR: not found")
		require.ErrorContains(t, st.Cause(), "config TLS_PEM_FILE: not found")
	})
}
```

### An HTTP/1.1 client

A test that needs HTTP/1.1, to check the fallback or `HTTP2Only`, configures
`h.HTTPClient()` itself: it trusts the CA, offers `http/1.1` in ALPN, and
sets the `Transport`'s `Protocols`. `httpserver`'s own tests do it with a
helper:

```go
// protocols is what a client offers in ALPN.
type protocols struct{ http1, http2 bool }

var (
	offersH2    = protocols{http2: true}
	offersHTTP1 = protocols{http1: true}
	offersBoth  = protocols{http1: true, http2: true}
)

// client returns h's HTTP client, trusting only ca, and offering p.
func client(h *humustest.Host, ca *humustest.CA, p protocols) *http.Client {
	c := h.HTTPClient()
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	if !p.http2 {
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	var protos http.Protocols
	protos.SetHTTP1(p.http1)
	protos.SetHTTP2(p.http2)
	tr.Protocols = &protos
	return c
}
```

`NewClient` works without `httpservertest.Start`. It needs only the Host the
App runs on and the roots its certificate chains to, so it works next to an
App started with `humustest.Start`, or with another runtime package's
`Start`, in a test of a process that serves HTTP next to another protocol.

### The Service contract

`httpserver`'s own tests hold the Service to the Service contract with
[`humustest.TestService`](/humus/pr-preview/pr-800/testing/#checking-a-service), and a
Service of the end user's own that wraps it is checked the same way:

```go
func TestService_meets_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		cert := humustest.NewCA(t).Issue(t, "localhost")
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, http.NotFoundHandler())
		require.NoError(t, err)
		return svc
	})
}
```

The Service keeps serving after `Serve`'s context is done, so the subtest
for `Shutdown` after `Serve` has returned on its own reports SKIP, as
`TestService` documents.

## Migration from `rest.Run`

`httpserver` replaces the server half of `rest.Run` from before the reset.
Its REST half moves to **Proposal: REST endpoint DSL and OpenAPI**, in
wave 4 of [the charter](https://github.com/z5labs/humus/discussions/629).

| Before the reset | With `httpserver` |
|---|---|
| `rest.Run(ctx, opts...)`, its server half | `httpserver.Main(Init, opts...)`, with the end user's `Init` returning the Service from `httpserver.NewService` |
| `internal/httpserver.App` | `httpserver.Service` |
| `rest.Port(...)`, `HUMUS_REST_PORT=8443` | `Settings.Addr`, required, sourced from whatever variable the end user names |
| `rest.ReadTimeout`, `ReadHeaderTimeout`, `WriteTimeout`, `IdleTimeout`, `MaxHeaderBytes` and their `HUMUS_REST_*` variables | the `Settings` fields of the same names, with the same defaults |
| `rest.TLSConfig(...)`, `HUMUS_REST_TLS_PKCS12_FILE` and `_PASSWORD` | `Settings.TLS`, required, a `*tls.Config` the end user decodes from a file read through `s.Host.FS`. PKCS#12 decoding is the end user's, with go-pkcs12 if they like. |
| a self-signed certificate when no TLS is configured | exit 78 when the end user's certificate variable isn't set; a config with no certificate, or one HTTP/2 can't use, fails `CheckTLS` |
| whatever bedrock's HTTP runtime and `net/http`'s defaults negotiated | HTTP/2 always on and first in ALPN, with HTTP/1.1 as a fallback that `Settings.HTTP2Only` turns off |
| `otelhttp.NewHandler(h, "rest")` on the globals | the Service's instrumentation, with `s.Telemetry`'s providers and propagator, and spans named after the route |
| `Shutdown(context.Background())` | `Shutdown` bounded by the grace period, then `Close` |
| `RecoverPanics` around the runtime | no recovery in the lifecycle; a handler's panic aborts its request, recorded in telemetry |
| `net/http`'s error log on the real stderr | ERROR records through the App's telemetry |
