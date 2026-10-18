# gRPC services

The [`grpcserver`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpcserver)
package serves gRPC over TLS as a `humus.Service`. The end user's `Init`
sources its `Settings`, builds the Service with `grpcserver.NewService`,
registers their generated services on it, as they would on a `*grpc.Server`,
and returns it. `grpcserver.Main` runs it with the defaults a gRPC server
behind a Kubernetes Service needs: a 5s drain and a 15s grace period. Its test
package,
[`grpcservertest`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpcserver/grpcservertest),
runs the same App in a test, on the same defaults, with a client that dials
the test Host over TLS.

Both packages are in the nested module `github.com/z5labs/humus/grpc`, so a
service that doesn't serve gRPC doesn't get grpc-go or otelgrpc in its module
graph. The module exports no App, and `grpcserver` runs no health Service
and serves no probes: the module's
[`grpchealth`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpchealth)
does, as [Health probes](#health-probes) describes. The design behind
everything on this page is in
[Proposal: gRPC services](https://github.com/z5labs/humus/discussions/681),
with its [revision](https://github.com/z5labs/humus/discussions/681#discussioncomment-18988629).

## A service

The end user's `Init` sources `grpcserver.Settings` with
[`config.Struct`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Struct),
as [Configuring a humus service](/humus/pr-preview/pr-942/configuration/) describes,
builds the Service with `NewService`, registers their services on it, and
returns it:

```go
// Init is the end user's Init: it sources the Settings, builds the Service
// with NewService, registers the end user's service on it and returns it,
// with any other Service the process runs.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) grpcserver.Settings {
		return grpcserver.Settings{
			// No .Default: both fields are required, so an unset variable fails as not found.
			Addr: r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
			// readFile and serverTLS are the end user's own helpers. The
			// bundle's password is read only once the bundle has been.
			TLS: r.Get(env("GRPC_TLS_P12_FILE").Parse(readFile(s.Host.FS)).FlatMap(func(bundle []byte) config.Value[*tls.Config] {
				return env("GRPC_TLS_P12_PASSWORD").Parse(serverTLS(bundle))
			}).Validate(grpcserver.CheckTLS)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	svc, err := grpcserver.NewService(s, settings)
	if err != nil {
		return nil, err
	}
	testpb.RegisterTestServiceServer(svc, greeter{})
	return []humus.Service{svc}, nil
}
```

`testpb` is grpc-go's `interop/grpc_testing` service, standing in for the end
user's generated code: a `RegisterCartServiceServer(svc, carts)` call is
written the same way. The variable names are the service's own: humus defines
none of them. `readFile` and `serverTLS` are the end user's helpers too, shown
under [TLS](#tls).

The service's `main` is one call to `grpcserver.Main`, here with a telemetry
`Builder` that names the service, as [Telemetry](/humus/pr-preview/pr-942/telemetry/#writing-the-builder)
describes:

```go
grpcserver.Main(Init, humus.WithTelemetry(buildTelemetry))
```

`NewService` runs every check on the `Settings` again, and then listens on
`Settings.Addr` through the Setup's Host, in `Init`. So a port that is already
taken fails `Init`, and the App exits 1, and a connection that arrives before
`Serve` runs waits in the backlog rather than being refused. The listener's
close is registered with `s.OnStop`, so an `Init` that fails after
`NewService` still closes it. An `Addr` with port 0 listens on a free port,
which `Service.Addr` reports.

`Init` returns the gRPC Service along with any other Service the process runs,
such as a Kafka consumer next to the gRPC server, or the
[health Service](/humus/pr-preview/pr-942/health/#the-health-service). Any Service
returning ends the App.

## Settings

`grpcserver.Settings` follows the
[settings convention](/humus/pr-preview/pr-942/configuration/#what-a-package-with-settings-exports):
plain Go values, and a check for each field. The package reads no environment
variable and no file, and defines no variable name.

| Field | Default | Valid | Check |
|---|---|---|---|
| `Addr` | none: required | `host:port`, with any host, an empty one included, and a port written as a number from 0 to 65535 | `CheckAddr` |
| `TLS` | none: required | a config that has a certificate and allows TLS 1.2 or later | `CheckTLS` |

`CheckAddr` accepts what `Host.Listen` accepts: an empty host listens on every
name, and port 0 picks a free port. It rejects `""`, an address with no port,
and a port that isn't such a number, such as the named port `:grpc`.

Everything else about the server, such as its interceptors, keepalive
parameters and message size limits, is code, and goes in `opts`, as
[Server options](#server-options) describes. An end user who wants a variable
for one of them sources it in their `Init` and passes the option.

**`Addr` and `TLS` are both required.** Neither has a default: humus imposes
no port, and serves nothing in plain text. So the package has no
`DefaultSettings()`, which would only return the zero `Settings`, whose `Addr`
of `""` and nil `TLS` both checks reject. The end user's chains for them have
no `.Default`, as in `Init` above, so a variable that isn't set fails as not
found. The App exits 78, and `config.Struct` names every variable at fault in
one error. With neither set:

```
init: config GRPC_ADDR: not found
config GRPC_TLS_P12_FILE: not found
```

`NewService` runs the same checks itself, so a `Settings` built in code with a
bad field fails `Init` too, naming the field, such as
`grpcserver: NewService: Settings.Addr "nope" is not host:port`. That error is
no `*config.Error`, so the App exits 1: the fault is in the code, not in the
configuration. [How a bad value fails](/humus/pr-preview/pr-942/configuration/#how-a-bad-value-fails)
explains the exit codes.

## TLS

**TLS is required.** The Service serves gRPC with `Settings.TLS` and nothing in
plain text. There is no plaintext setting and no self-signed fallback: a
service that hasn't sourced a certificate doesn't start, rather than starting
and failing every RPC. Traffic is encrypted all the way to the pod, whatever
sits in front of it, so every service needs a certificate, even behind a mesh
that already encrypts between pods.

### The certificate

The end user decodes the certificate from a file, read through `s.Host.FS` so
that a test reads it from its own directory and production from the real
disk, as [Reading a file](/humus/pr-preview/pr-942/configuration/#reading-a-file)
describes. How the certificate is stored and encoded is the end user's
decision. `Init` above reads a PKCS#12 bundle from the file `GRPC_TLS_P12_FILE`
names, and decodes it with the password in `GRPC_TLS_P12_PASSWORD`, so a
missing file, a missing password and a wrong one each fail as a config error
with exit 78:

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

// serverTLS returns a parser for config.Value.Parse that decodes bundle, a
// PKCS#12 bundle, with the password it is given, into a server's TLS
// config.
func serverTLS(bundle []byte) func(password string) (*tls.Config, error) {
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

`pkcs12.DecodeChain` is from
[go-pkcs12](https://pkg.go.dev/software.sslmate.com/src/go-pkcs12), which
humus doesn't depend on. A PEM file that holds the certificate chain and the
private key needs no password: its chain is
`env("GRPC_TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(grpcserver.CheckTLS)`,
with a `serverTLS` that calls `tls.X509KeyPair`, as
[Serving HTTP](/humus/pr-preview/pr-942/http/#the-certificate) shows. A certificate
that rotates on disk is served from a config that sets `GetCertificate`
instead of `Certificates`.

### What `CheckTLS` rejects

`CheckTLS` fails startup on a config that would otherwise fail every
connection. It rejects:

- **nil**, which is what the zero `Settings` leaves `TLS`;
- **a config with no certificate:** none of `Certificates`, `GetCertificate`
  and `GetConfigForClient` set, with which every handshake fails;
- **a `MaxVersion` below TLS 1.2**, which HTTP/2, and so gRPC, requires.
  grpc-go's clients ask for at least TLS 1.2 by default.

Its `CipherSuites` are not checked. Like every check, its error doesn't name
the field: the chain that sourced the config names its variable.

### Client certificates

A Service that requires its clients to present a certificate sets
`ClientAuth`, as with any `tls.Config`: `tls.RequireAndVerifyClientCert`, with
the CAs it trusts in `ClientCAs`. `CheckTLS` accepts such a config. Kubernetes'
probes present no client certificate, as [Health probes](#health-probes)
describes.

### `grpc.Creds` has no effect

`Settings.TLS` is the one way to set how the Service secures its connections.
`NewService` passes its transport credentials, built from `Settings.TLS`,
after the end user's options, and grpc-go keeps the last `grpc.Creds` it is
given. So a `grpc.Creds` in `opts`, such as
`grpc.Creds(insecure.NewCredentials())`, has no effect: the Service still
serves TLS, and a plaintext client fails to connect with `Unavailable`. The
end user's config is cloned, so it is never changed.

## Server options

Everything about the server that isn't a setting is a `grpc.ServerOption` the
end user passes to `NewService`, after the `Settings`: their interceptors,
with `grpc.ChainUnaryInterceptor` and `grpc.ChainStreamInterceptor`, keepalive
parameters, with `grpc.KeepaliveParams` and `grpc.KeepaliveEnforcementPolicy`,
message size limits, with `grpc.MaxRecvMsgSize` and `grpc.MaxSendMsgSize`, and
a second stats handler, as in
`grpcserver.NewService(s, settings, unary, stream)`, with the two
interceptors `grpcauth.Interceptors` returns, as
[Authentication](#authentication) shows.

`NewService` builds the server with humus's options first, then `opts`, then
humus's transport credentials:

| Option | Why |
|---|---|
| `grpc.StatsHandler`, with otelgrpc's server handler | traces and metrics, from `s.Telemetry`: see [Telemetry](#telemetry) |
| `grpc.ChainUnaryInterceptor` and `grpc.ChainStreamInterceptor`, with panic recovery, and for streams the context that `Shutdown` cancels | see [Panics](#panics) and [Streams at shutdown](#streams-at-shutdown) |
| `grpc.WaitForHandlers(true)` | `Stop` waits for every handler to return, so no handler outlives `Shutdown` |
| the end user's `opts` | |
| `grpc.Creds`, from `Settings.TLS` | last, so an end user's `grpc.Creds` can't replace it |

**grpc-go keeps one value of some options and chains others,** so where an
option in `opts` lands depends on which it is:

- **An option that chains**, such as `grpc.ChainUnaryInterceptor`,
  `grpc.ChainStreamInterceptor` or `grpc.StatsHandler`, adds to humus's. The
  end user's interceptors run inside humus's, after panic recovery, and a
  stats handler in `opts` is called after humus's.
- **An option that grpc-go keeps one value of**, such as `grpc.MaxRecvMsgSize`
  or `grpc.KeepaliveParams`, takes the last value it is given, so one in
  `opts` wins over the same option in humus's. Of those, humus sets only
  `grpc.WaitForHandlers`, before `opts`, and `grpc.Creds`, after them.
- **`grpc.WaitForHandlers(false)` breaks the Service contract.** It replaces
  humus's `grpc.WaitForHandlers(true)`, and lets a handler outlive `Shutdown`,
  the App's grace period and the process's telemetry.
  `humustest.TestService` reports the handler's goroutine as a leak.

### Spreading clients across pods

A gRPC client keeps one HTTP/2 connection, and a Kubernetes ClusterIP
Service picks a pod for each connection, not for each RPC. So each client
stays on one pod until its connection ends, and a pod added by a scale-up
gets no client that is already connected. A server spreads its clients by
ending their connections now and then, with
`keepalive.ServerParameters{MaxConnectionAge}` in `grpc.KeepaliveParams`,
one of `NewService`'s options:

```go
// Every connection is closed with a GOAWAY once it is about a
// minute old, so its clients dial again, through the
// Kubernetes Service, and reach whichever pod it picks.
grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: time.Minute}),
```

The server sends a GOAWAY once a connection reaches that age, give or take
10%, and lets the RPCs on it finish. Its clients dial again, and reach
whichever pod the Service picks, so a `grpcclient` Client sees no error.
`MaxConnectionAgeGrace` bounds how long the RPCs on an old connection may
take before it is closed under them, which a server with long streams sets.
[Calling gRPC services](/humus/pr-preview/pr-942/grpc-client/#spreading-the-load-with-maxconnectionage)
describes what a client does with it.

## Registration

The `*Service` is a `grpc.ServiceRegistrar`, so generated code registers
services on it, as `Init` above does. `RegisterService` must be called before
`Serve`, as with a `*grpc.Server`.

**`RegisterService` panics instead of exiting.** grpc-go calls `os.Exit` on
three registration mistakes, which would bypass the lifecycle, the `OnStop`
functions and telemetry: a registration after `Serve`, a service registered
twice, and an implementation that doesn't implement the service's interface.
The Service checks for the same three first, and panics instead, with the
service's name in the message. All three are programming errors, so the panic
fails the end user's `Init` in the first test or the first start.

**Server reflection is off by default.** Reflection exposes the service's
schema to anyone who can reach its port, so the Service registers none. An end
user who wants it registers it in their `Init`, after their own services,
which `Service.GetServiceInfo` makes work:

```go
reflection.Register(svc)
```

`reflection` is `google.golang.org/grpc/reflection`.

## Shutdown

The Service follows the [two shutdown signals](/humus/pr-preview/pr-942/lifecycle/#service)
of a humus App:

- **`Serve`'s context being done changes nothing.** It is done as soon as the
  App starts shutting down, and the Service keeps serving connections and
  RPCs through the App's [drain](/humus/pr-preview/pr-942/lifecycle/#the-drain), while
  load balancers stop sending it traffic. An RPC that arrives during the drain
  is served, and an RPC in flight finishes.
- **`Shutdown` ends every stream,** as [Streams at shutdown](#streams-at-shutdown)
  describes, **and then calls `GracefulStop`.** That stops accepting
  connections, sends every client a GOAWAY, and waits for the RPCs in flight
  to finish. gRPC connections are long-lived, and the GOAWAY is what moves a
  client off a terminating pod: it opens its next connection elsewhere.
- **When the grace period runs out first, `Shutdown` calls `Stop`,** which
  closes every connection and cancels every RPC's context. With
  `grpc.WaitForHandlers(true)`, which humus sets, `Stop` then waits for every
  handler to return. The App exits 1 with a `*humus.GracePeriodError`.

**A handler returns once its context is done.** `Stop` waits for every
handler, so a handler that ignores its context holds the App up past the grace
period, and in a test that runs the App inside `testing/synctest`, synctest
reports its goroutine as a leak.

### Streams at shutdown

A stream that runs until its client leaves, such as a subscription, would hold
`GracefulStop` up until the grace period, and every deployment would exit 1.
So `Shutdown` ends every stream, with a status the client can act on. A stream
interceptor of humus's gives every streaming RPC's handler a context of its
own, derived from the RPC's, which `Shutdown` cancels. That covers server,
client and bidirectional streams. When `Shutdown` starts:

- **it cancels the context of every stream in flight,** before it calls
  `GracefulStop`. A stream that starts after that gets a context that is
  already cancelled;
- **when a handler then returns nil, an error that is `context.Canceled`, or a
  status with `codes.Canceled`,** the stream ends with `codes.Unavailable` and
  the message `server is shutting down`. Without that, grpc-go would end it
  with `Canceled`, and a handler that returned nil, such as a download cut
  short, would end its stream with `OK`;
- **a handler that returns a status of its own,** such as `Aborted` with
  details, keeps it;
- **unary RPCs are not cancelled.** They finish within the grace period.

`GracefulStop` keeps each connection open until its streams have ended, so the
status reaches the client, in the stream's trailers. `Unavailable` is gRPC's
code for a condition the client corrects by retrying, so the end user's client
reconnects on it:

```go
// receive is the end user's client of a subscription: it receives from
// stream until the stream ends, and reports whether the client should
// subscribe again.
func receive(stream grpc.ServerStreamingClient[testpb.StreamingOutputCallResponse]) (bool, error) {
	for {
		_, err := stream.Recv()
		if status.Code(err) == codes.Unavailable {
			// The server is going away. Reconnect: the new stream lands on another pod.
			return true, nil
		}
		if errors.Is(err, io.EOF) {
			return false, nil // the server ended the stream with OK
		}
		if err != nil {
			return false, err
		}
	}
}
```

The streams are cancelled when `Shutdown` starts, after the drain, and not
when `Serve`'s context is done. During the drain, a load balancer may still
route a client that reconnects back to this pod.

### A handler blocked in `Recv`

A handler sees the shutdown through its context. The Service doesn't make
`Recv` return when that context is cancelled: that would take a goroutine for
each receive, which could still be blocked in grpc-go after the handler had
moved on. So a handler that is blocked in `Recv`, waiting for its client's
next message, sees the shutdown only when the client sends, or when `Stop`
closes the connection at the end of the grace period, and the App then exits 1
with a `*humus.GracePeriodError`.

A handler that has to notice shutdown while it waits receives in a goroutine
and selects on its context, as it would for any call that takes no context.

## The defaults

`grpcserver.Main` is `humus.Main` with `grpcserver.Defaults()` applied before
the end user's options, and does nothing else. `Defaults` is one
`humus.Option` that sets two of `humus.Main`'s
[defaults](/humus/pr-preview/pr-942/lifecycle/#the-defaults), and leaves the Host and
the telemetry `Builder` as `humus.Main`'s:

- **A drain of `humus.DrainDelay(5 * time.Second)`.** Kubernetes removes a
  terminating pod from a Service's endpoints asynchronously, so for a while
  after SIGTERM, clients and load balancers still send RPCs and open
  connections to the pod. The Service keeps serving through the drain, so
  those RPCs are answered rather than refused, while the endpoint removal
  reaches every node. It keeps serving until `Shutdown`, so its `Serve`
  doesn't return during the drain, and the drain lasts the whole 5s.
- **A grace period of 15s**, rather than `humus.Main`'s 20s, so that the drain
  fits in the same budget. It gives unary RPCs, and stream handlers ending on
  their contexts, time to finish once `Shutdown` has sent every client a
  GOAWAY. Streams that never end don't use it up, since `Shutdown` ends them.

From SIGTERM, then: the Service serves through the 5s drain; `Shutdown` then
ends every stream and waits up to 15s for the RPCs in flight, and stops what
is left; and the telemetry's shutdown, `telemetry.Settings.ShutdownTimeout`,
takes up to 5s more. The worst case is 25s, the budget that `humus.Main` keeps
within Kubernetes' default `terminationGracePeriodSeconds` of 30s. A service
whose RPCs all finish during the drain exits 5s after SIGTERM, when the drain
ends.

### Overriding them

The end user's options come after `Defaults`, so they win.
`grpcserver.Main(Init, humus.WithShutdownGracePeriod(10*time.Second))` has a
10s grace period and still the 5s drain, and `humus.WithDrain(nil)` removes
the drain.

An operator overrides both without a rebuild, with the lifecycle's
[variables](/humus/pr-preview/pr-942/lifecycle/#settings):
`HUMUS_SHUTDOWN_GRACE_PERIOD` sets the grace period, and
`HUMUS_SHUTDOWN_DRAIN_DELAY` replaces the drain with a delay of its value,
whatever `Defaults` or an option set. Each leaves the other as it was: with
`HUMUS_SHUTDOWN_DRAIN_DELAY=1s` alone, the drain is 1s and the grace period
still 15s.

### `terminationGracePeriodSeconds`

When Kubernetes stops a pod, it sends the container SIGTERM, and then SIGKILL
once `terminationGracePeriodSeconds` have passed, 30 by default. The defaults
already use the 25s that leaves room for the process to exit, so a service
that raises its drain or its grace period raises
`terminationGracePeriodSeconds` with them, in the same manifest, to at least
the drain, the grace period and the telemetry's shutdown, plus a margin.
Otherwise SIGKILL ends it first: its container exits 137, its OnStop functions
never run, and the telemetry it had not yet exported is lost.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: carts
spec:
  selector:
    matchLabels:
      app: carts
  template:
    metadata:
      labels:
        app: carts
    spec:
      # 5s drain + 30s grace period + 5s telemetry shutdown, and a margin.
      terminationGracePeriodSeconds: 45
      containers:
        - name: carts
          image: registry.example.com/carts:1.0.0
          ports:
            - name: grpc
              containerPort: 9090
          env:
            - name: GRPC_ADDR
              value: ":9090"
            - name: GRPC_TLS_P12_FILE
              value: /etc/carts/tls/tls.p12
            - name: GRPC_TLS_P12_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: carts-tls
                  key: password
            - name: HUMUS_SHUTDOWN_GRACE_PERIOD
              value: 30s
          volumeMounts:
            - name: tls
              mountPath: /etc/carts/tls
              readOnly: true
      volumes:
        - name: tls
          secret:
            secretName: carts-tls # tls.p12: the PKCS#12 bundle
```

The drain does the job of a `preStop` hook that sleeps, which services without
one add for the same reason. A pod that has both spends the hook's sleep and
the drain out of the same `terminationGracePeriodSeconds`.

### `grpcserver.Main` or `humus.Main`

Which `Main` a process calls depends on what a load balancer routes to, as
[the lifecycle's runtime packages](/humus/pr-preview/pr-942/lifecycle/#runtime-packages)
explains:

- **A process uses `grpcserver.Main` when a load balancer, or a Kubernetes
  Service, routes RPCs to its gRPC server,** a gRPC service alone or one next
  to a job or a queue consumer. That server needs the drain, and the other
  Services then get its 5s drain and 15s grace period, rather than
  `humus.Main`'s 20s grace period with no drain.
- **A process that no load balancer routes to,** such as a job, calls
  `humus.Main`, with no drain. A drain would make a finished
  [job](/humus/pr-preview/pr-942/jobs/) wait for the whole delay.

A process that serves HTTP and gRPC, each routed to, uses either package's
`Main`. Each package's defaults set both the drain and the grace period, so
passing the other package's defaults as well replaces both: the last one wins.

## Health probes

**Kubernetes' gRPC probes can't reach the Service on most clusters.** A probe
with `grpc:` connects in plain text and presents no client certificate, and
the Service serves nothing in plain text. `grpcserver` serves no probes, and
registers no health service. The module's
[`grpchealth`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpchealth)
package answers gRPC health v1 from an App's `health.Probes`, in two ways:

- **`grpchealth.NewService`, on a plaintext port of its own,** for a cluster
  whose `grpc` probes can't use TLS, which is most clusters today. It
  registers only `grpc.health.v1.Health`, so the end user's services are
  never reachable in plain text. It owns the App's readiness, so `Init`
  returns it next to the gRPC Service and no health Service. It serves
  through the drain and shuts down as this Service does.
- **`grpchealth.NewServer`, registered on this Service,** next to a health
  Service, on a cluster with the `GRPCContainerProbeTLS` feature gate on. A
  probe with `mode: TLS` then connects over TLS, and reaches the Service on
  its own port, with no second one, unless the Service requires client
  certificates with `ClientAuth`: the kubelet presents none. The gate is
  alpha and off by default; see Kubernetes'
  [Use TLS with gRPC probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/#grpc-probe-tls).
  grpc-go's clients reach it on every cluster, for client-side health
  checking. Under `grpcauth`, its methods are opened by name, as
  [Authentication](#authentication) shows.

[Health](/humus/pr-preview/pr-942/health/#exposing-the-probes) shows both wirings,
the names they answer, the manifests and how to test them.

## Authentication

The module's [`grpcauth`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpcauth)
package authenticates and authorizes each RPC by its method, with the schemes
and requirements the [`auth`](https://pkg.go.dev/github.com/z5labs/humus/auth)
package declares. `grpcauth.Interceptors` returns a unary and a stream
interceptor, built from rules: `grpcauth.Default`, the requirement of every
method no rule names, and `grpcauth.Method`, one method's, by the full-name
constant protoc-gen-go-grpc generates.

`grpcauth` exempts no method on its own, so **gRPC health registered on the
Service with `grpchealth.NewServer` passes through it like every RPC,** and
the kubelet's probes present no credentials. The rules open Health's
`Check`, `Watch` and `List` by name, with `auth.Disabled()`:

```go
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
if err != nil {
	return nil, err
}
testpb.RegisterTestServiceServer(svc, greeterService{bearer: bearer})
hs := health.NewService()
healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, hs.Probes()))
return []humus.Service{svc, hs}, nil
```

Without those three rules, the probes get `Unauthenticated` and the pod never
turns ready, which fails at the first deploy, never silently. A probes
Service of its own, `grpchealth.NewService`, has no authentication and needs
no rule.

[Authentication and authorization](/humus/pr-preview/pr-942/auth/) covers schemes,
requirements and scopes, the answers `grpcauth` gives and the gRPC status a
verifier's error ends with, verifying JWTs with `auth/jwt`, and testing.

## Telemetry

The Service takes everything from the Setup's telemetry, and never reads or
sets OpenTelemetry's globals. The first of humus's options is a stats handler,
OpenTelemetry's gRPC instrumentation,
[otelgrpc](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc),
given the `TracerProvider`, the `MeterProvider` and the `Propagator` of
`s.Telemetry`.

### The span and the metric

**Each RPC gets one server span,** named after the method without its leading
slash, such as `grpc.testing.TestService/UnaryCall`. Its parent is the trace
context the RPC's metadata brought in, extracted with the Setup's propagator,
so an RPC joins its caller's trace. The propagator comes from the telemetry
`Builder` and decides which metadata carries the trace context: `traceparent`
and `tracestate`, and `baggage`, with `telemetry.New`'s. The span carries the
attributes of OpenTelemetry's RPC semantic conventions, among them
`rpc.system.name`, `rpc.method`, `server.port` and
`rpc.response.status_code`.

**Its status is `Error` for the codes that are the server's errors:**
`Unknown`, `DeadlineExceeded`, `Unimplemented`, `Internal`, `Unavailable` and
`DataLoss`. It is left unset for every other code, such as `NotFound`, which
is the caller's error rather than the server's.

**Each RPC also records the histogram `rpc.server.call.duration`** on the
Setup's `MeterProvider`, in otelgrpc's instrumentation scope.

These are the names of the stable RPC conventions, which otelgrpc uses unless
`OTEL_SEMCONV_STABILITY_OPT_IN`, read from the real environment rather than
the Process's, asks for the old ones with `rpc/old` or `rpc/dup`.

### The log records

The Service writes one log record of its own, for a panic, through the
`LoggerProvider` of the Setup's telemetry, with the logger name
`github.com/z5labs/humus/grpc/grpcserver`:

| Record | Severity | When |
|---|---|---|
| `RPC panicked`, with `rpc.method`, `exception.type`, `exception.message` and `exception.stacktrace`, in the RPC's context | ERROR | a handler or an interceptor in `opts` panics: see [Panics](#panics) |

The lifecycle's own records, such as `serving` and `shutting down`, come from
the App as for any Service.

### Error spans at a rollout

otelgrpc marks the span of each stream that `Shutdown` ends `Error`, as it
does for every `Unavailable`. So a rollout records one error span for each
stream that was open on each pod it replaced. That is accurate: the server
ended those streams.

### gRFC A66 metrics

otelgrpc follows OpenTelemetry's semantic conventions, which the rest of an
OpenTelemetry setup and the HTTP server's instrumentation use. An end user who
also wants the metrics of [gRFC A66](https://github.com/grpc/proposal/blob/master/A66-otel-stats.md),
grpc-go's own OpenTelemetry metrics, such as `grpc.server.call.duration`,
passes grpc-go's
[`stats/opentelemetry`](https://pkg.go.dev/google.golang.org/grpc/stats/opentelemetry)
in `opts` as a second stats handler, given the Setup's `MeterProvider`, and
grpc-go calls it after humus's:

```go
svc, err := grpcserver.NewService(s, settings, opentelemetry.ServerOption(opentelemetry.Options{
	MetricsOptions: opentelemetry.MetricsOptions{MeterProvider: s.Telemetry.MeterProvider},
}))
```

### grpc-go's logger

**grpc-go's own logger is an ambient leak.** Its `grpclog` package reads
`GRPC_GO_LOG_SEVERITY_LEVEL`, `GRPC_GO_LOG_VERBOSITY_LEVEL` and
`GRPC_GO_LOG_FORMATTER`, and its `internal/envconfig` package reads its other
`GRPC_*` variables, from the real environment rather than the Process's, and
it writes to the real standard error, outside the App's telemetry. It is on
[the lifecycle's list of ambient leaks](/humus/pr-preview/pr-942/lifecycle/#ambient-leaks).

Replacing it means `grpclog.SetLoggerV2`, which is process-wide state, and
humus sets none. An end user who wants grpc-go's logs in their telemetry sets
the logger in `main`, before calling `grpcserver.Main`, with a
`grpclog.LoggerV2` that writes to their telemetry. Until then, a test sees
what grpc-go logs only on the test binary's real standard error.

## Panics

**A handler that panics fails only its own RPC.** grpc-go recovers nothing, so
the outermost of humus's interceptors, a unary and a stream one, do. They run
inside the stats handler and outside the end user's interceptors, so the panic
of an interceptor in `opts` is recovered too. The Service:

- fails the RPC with `codes.Internal` and the message `internal error`, so the
  panic's value never reaches the client. A stream that had already sent
  messages ends with that status;
- records the panic on the RPC's span as an `exception` event, and the span's
  status is `Error`, from the `Internal` code;
- emits one ERROR record, `RPC panicked`, as [The log records](#the-log-records)
  describes, in the RPC's context, so the record carries the span's trace and
  span IDs.

Every other RPC, and the server, carry on. The cost is that of `net/http`'s
recovery: whatever the handler left half-changed stays that way. A lock it
held is never released, and a structure it was updating is left as it was
when it panicked, for the RPCs that come after.

Only RPCs are recovered. The lifecycle itself, the end user's `Init` and the
Service's `Serve` and `Shutdown`, never recovers a panic.

## Failure injection

The Service strips the failure scenario an RPC carries, before any
interceptor or handler of the end user's sees it, from every caller that
`Settings.TrustScenario` doesn't trust, so a caller on the internet can't
make the service fail on purpose. `TrustScenario` is given the RPC's
context, with its peer and its incoming metadata, and its full method, such
as to trust a caller whose client certificate the service mesh issued. When
it is nil, the default, no caller is trusted. Without `humus.WithFaults`,
nothing is stripped. [Failure injection](/humus/pr-preview/pr-942/failure-injection/)
explains the model, whom to trust and how, and how to test a scenario.

## Testing

A gRPC service is tested whole, inside `testing/synctest`, as
[Testing a humus App](/humus/pr-preview/pr-942/testing/) describes for any App, with
two functions from `grpcservertest`:

- **`grpcservertest.Start`** is `humustest.Start` with `grpcserver.Defaults()`
  applied before the test's options, and does nothing else. A test of a
  `main` that calls `grpcserver.Main` runs with the drain and the grace period
  production runs with, and can't forget them: an RPC that never returns is
  cancelled 20s after SIGTERM. Options given with `humustest.WithApp` come
  after the defaults, so they win, and `HUMUS_SHUTDOWN_GRACE_PERIOD` and
  `HUMUS_SHUTDOWN_DRAIN_DELAY`, given with `humustest.WithEnv`, override them
  as an operator's manifest does.
- **`grpcservertest.NewClient(t, h, address, roots)`** returns a
  `*grpc.ClientConn` for the Service listening on `address` in the test Host
  `h`. It dials through the Host, never resolves `address`, and verifies the
  Service's certificate for `address`'s host against `roots`. A certificate
  from another CA, or for another name, fails the handshake, and the RPC fails
  with `Unavailable`. The client is closed when the test ends.

### A whole service

This test runs a service whose stream would never end on the package's
defaults, with a certificate from a CA that
[`humustest.NewCA`](/humus/pr-preview/pr-942/testing/#tls) generates for the test, so no
key or certificate is ever written into the repository. `serves` and
`settingsFor` are helpers of the test file: `settingsFor` returns `Settings` on
`:9090` with a certificate for `carts` from the CA, and `serves` returns an
`Init` whose only Service serves the handler on them:

```go
func TestMain_shutdown_ends_a_stream_with_Unavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, exit := grpcservertest.Start(t, serves(settingsFor(t, ca, ":9090"), subscriber{}),
			humustest.WithApp(humus.WithHost(h.Host())),
		)
		<-h.Listening("carts:9090")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		stream, err := client.StreamingOutputCall(t.Context(), &testpb.StreamingOutputCallRequest{})
		require.NoError(t, err)
		_, err = stream.Recv()
		require.NoError(t, err)

		<-p.Notified(syscall.SIGTERM)
		begin := time.Now()
		p.Signal(syscall.SIGTERM)
		_, err = stream.Recv()
		require.Equal(t, codes.Unavailable, status.Code(err), err)
		require.Equal(t, "server is shutting down", status.Convert(err).Message())
		require.Equal(t, 5*time.Second, time.Since(begin)) // the drain, then Shutdown ends the stream

		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}
```

Fake time means the drain costs the test nothing: the stream ends 5s after
SIGTERM, in no real time at all.

### Giving the App its certificate

`Start` makes no CA and no certificate: how the end user's `Init` sources its
certificate, from which variable and in which encoding, is the end user's
decision. The test gives it one the way production does. It issues a
certificate from its CA for the name it dials, writes it into the Host's
filesystem with `humustest.WithFiles`, and sets the variables that name the
file and its password with `humustest.WithEnv`. For the `Init` that reads the
PKCS#12 bundle [above](#the-certificate), the tests do it with a helper:

```go
// p12Env returns a test Host holding a PKCS#12 bundle with a certificate
// for carts from ca, as tls.p12, and the environment that names it and its
// password, with addr as GRPC_ADDR.
func p12Env(t *testing.T, ca *humustest.CA, addr string) (*humustest.Host, map[string]string) {
	t.Helper()
	bundle, password := ca.Issue(t, "carts").PKCS12(t)
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{"tls.p12": bundle}))
	return h, map[string]string{
		"GRPC_ADDR":             addr,
		"GRPC_TLS_P12_FILE":     "tls.p12",
		"GRPC_TLS_P12_PASSWORD": password,
	}
}
```

and start the App with it, then call it with a client that trusts the CA:

```go
ca := humustest.NewCA(t)
h, env := p12Env(t, ca, ":9090")

p, exit := grpcservertest.Start(t, Init,
	humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
	humustest.WithEnv(env),
)
<-h.Listening("carts:9090")

client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
resp, err := client.UnaryCall(t.Context(), &testpb.SimpleRequest{})
require.NoError(t, err)
require.Equal(t, "greeter", resp.GetUsername())
```

Each certificate has a new random key, and each bundle a new random password,
so nothing secret is fixed in the test. A test that builds the
`grpcserver.Settings` itself, as `settingsFor` does, sets their TLS config from
`cert.TLS()` instead. Either way, the test passes the CA's `Pool()` to
`NewClient`.

The same `Init` with no environment at all checks that `Addr` and `TLS` are
required:

```go
func TestInit_without_an_address_or_a_certificate_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, exit := grpcservertest.Start(t, Init)

		st := <-exit
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config GRPC_ADDR: not found")
		require.ErrorContains(t, st.Cause(), "config GRPC_TLS_P12_FILE: not found")
	})
}
```

### A plaintext server

Transport credentials in `NewClient`'s options replace its own, since they are
applied after them. A test of a server that serves plain text, such as
`grpchealth`'s probes port, passes `insecure.NewCredentials()`, and `nil` roots:

```go
conn := grpcservertest.NewClient(t, h, "localhost:8081", nil,
	grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

`NewClient` works without `grpcservertest.Start`. It needs only the Host the
App runs on and the roots its certificate chains to, so it works next to an
App started with `humustest.Start`, or with another runtime package's
`Start`, in a test of a process that serves gRPC next to HTTP.

### The `dns` resolver and synctest

**A client's target must not use the `dns` resolver.** A gRPC client looks its
target up with a resolver, picked by the target's scheme. A target with no
scheme, such as `carts:9090`, gets grpc-go's default, the `dns` resolver,
which looks the host up with `net.DefaultResolver`, whatever dialler the
client has. That reaches the real network, where a name that exists only on
the test Host is not found. Inside a `testing/synctest` bubble it is worse:
the lookup crashes the test binary with
`fatal error: send on synctest channel from outside bubble`.

So `NewClient` targets `passthrough:///` and the address. The passthrough
resolver hands the address to the dialler as it is, and the dialler is the
Host's: no lookup is made, and a name that exists nowhere but on the test Host
works, inside a bubble or out. A test that builds a client of its own, with
`grpc.NewClient`, does the same: it targets `passthrough:///` and the address,
and dials through the Host with `grpc.WithContextDialer`, whose function takes
no network, so it calls `h.Dial(ctx, "tcp", addr)`.

### The Service contract

`grpcserver`'s own tests hold the Service to the Service contract with
[`humustest.TestService`](/humus/pr-preview/pr-942/testing/#checking-a-service), and a
Service of the end user's own that wraps it is checked the same way:

```go
func TestService_meets_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		cert := humustest.NewCA(t).Issue(t, "carts")
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: ":9090",
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		})
		require.NoError(t, err)
		testpb.RegisterTestServiceServer(svc, &testServer{})
		return svc
	})
}
```

The Service keeps serving after `Serve`'s context is done, so the subtest for
`Shutdown` after `Serve` has returned on its own reports SKIP, as
`TestService` documents. A Service built with `grpc.WaitForHandlers(false)` in
`opts` fails the contract: a handler outlives `Shutdown`, and `TestService`
reports its goroutine as a leak.

## Migration from `grpc.NewApi`

`grpcserver` replaces the `grpc` package from before the reset, whose
`grpc.NewApi` built a `*grpc.Server` that nothing has been able to serve since
[#485](https://github.com/z5labs/humus/pull/485).

| Before the reset | With `grpcserver` |
|---|---|
| `grpc.NewApi()` | `grpcserver.NewService(s, settings, opts...)`, in the end user's `Init` |
| `(*Api).RegisterService`, called by generated code | `(*Service).RegisterService`. Generated `RegisterXServer(svc, impl)` calls don't change. |
| running an `Api`: not possible since #485 | `grpcserver.Main`, with the Service among the Services the end user's `Init` returns |
| `internal/grpcserver`'s unbounded `GracefulStop` | `Shutdown`: every stream ends with `Unavailable`, then `GracefulStop`, bounded by the grace period, then `Stop` |
| `stats/opentelemetry` fed from `otel.Get*` | otelgrpc fed from `s.Telemetry` |
| an implementation that is a `health.Monitor` is health-checked implicitly | nothing implicit: the monitor goes in the readiness of `grpchealth.NewService`, or of the health Service whose `Probes` `grpchealth.NewServer` answers from |
| `internal/grpchealth` | `grpchealth.NewServer`, registered on the Service, or `grpchealth.NewService` on a plaintext port of its own; see [Health](/humus/pr-preview/pr-942/health/#exposing-the-probes) |
| `grpc/internal/echopb` in tests | grpc-go's `interop/grpc_testing` |
| a handler's panic ends the process | the RPC fails with `Internal`, and the panic is logged |
