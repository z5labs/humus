# Health

A service reports its health as [monitors](#monitors) it combines into two
[probes](#liveness-and-readiness), liveness and readiness, and an App's
readiness is owned by a [health Service](#the-health-service) it returns
from `Init` like any other Service. All of it is in the
[`health`](https://pkg.go.dev/github.com/z5labs/humus/health) package, which
depends on nothing but the standard library and serves no protocol.
[`httphealth`](https://pkg.go.dev/github.com/z5labs/humus/httphealth) and
[`grpc/grpchealth`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpchealth)
[expose the probes](#exposing-the-probes) to Kubernetes.

The design behind this page is in
[Proposal: health monitors and probes, Revision 2](https://github.com/z5labs/humus/discussions/634#discussioncomment-18976159),
and, for exposing the probes, in
[Proposal: exposing health probes](https://github.com/z5labs/humus/discussions/761).

## Monitors

A [`Monitor`](https://pkg.go.dev/github.com/z5labs/humus/health#Monitor) is
anything that can report whether it is healthy right now: a database ping, a
flag the service sets once it has started, a check of a queue's depth. It has
one method, `Healthy(ctx context.Context) (bool, error)`, which returns
`false`, or an error, when the monitor is not healthy. Two
monitors come with the package:

- [`Binary`](https://pkg.go.dev/github.com/z5labs/humus/health#Binary) is a
  monitor the service marks healthy or unhealthy itself, with `MarkHealthy`
  and `MarkUnhealthy`. Its zero value is unhealthy, and it is safe for
  concurrent use.
- [`MonitorFunc`](https://pkg.go.dev/github.com/z5labs/humus/health#MonitorFunc)
  turns any `func(context.Context) (bool, error)` into a monitor. Its
  `Healthy` calls the function with the context it is given.

**A monitor honours the `ctx` it is given.** Whatever checks a monitor, such
as an orchestrator's probe with a timeout, bounds the check with that
context, so a monitor that dials or queries something passes `ctx` on rather
than using a context of its own. A database ping that ignores it can hang a
probe past its deadline.

## Combining monitors

Monitors combine, and the combinations are monitors themselves, so they nest:

- [`And`](https://pkg.go.dev/github.com/z5labs/humus/health#And) is healthy
  when every one of its monitors is. It checks them in order and **stops at
  the first failure**, the first monitor that is unhealthy or returns an
  error, and returns that monitor's result without checking the rest. `And`
  with no monitors is healthy.
- [`Or`](https://pkg.go.dev/github.com/z5labs/humus/health#Or) is healthy
  when any one of its monitors is. It stops at the first that is healthy;
  otherwise it is unhealthy and **joins the errors** every monitor returned,
  with [`errors.Join`](https://pkg.go.dev/errors#Join). `Or` with no monitors
  is unhealthy.

Both pass the context they are given to every monitor they check, nested ones
included, so a caller's deadline reaches each check.

## Liveness and readiness

An orchestrator such as Kubernetes asks a service two questions, and
[`Probes`](https://pkg.go.dev/github.com/z5labs/humus/health#Probes) holds a
monitor for each:

| Probe | Question | When it fails, Kubernetes |
|---|---|---|
| `Liveness` | is the process still working? | restarts the container |
| `Readiness` | can the process serve traffic right now? | stops sending it traffic until the probe passes again, without restarting it |

So a liveness probe should fail only when a restart would fix the process, a
deadlock for example. **Liveness checks no dependencies by default**, which
is `And` with no monitors: restarting a pod doesn't fix a database that is
down, and a liveness probe that checks one restarts every pod of the service
while the database is down. Dependency checks go in readiness, which takes
the pod out of the load balancer until the database is back. A gRPC
dependency, called through a `grpcclient` Client, has a readiness monitor
of its own, `Connected`, which latches once the Client has first connected,
as [A gRPC dependency's readiness](#a-grpc-dependencys-readiness) describes.

Here readiness checks that the service has started and that its database
answers `ping`, and liveness checks nothing:

```go
var started health.Binary
probes := health.Probes{
	Liveness: health.And(),
	Readiness: health.And(&started, health.MonitorFunc(func(ctx context.Context) (bool, error) {
		return ping(ctx) == nil, nil
	})),
}
```

## The health Service

A [`health.Service`](https://pkg.go.dev/github.com/z5labs/humus/health#Service)
is a [Service](/humus/pr-preview/pr-1237/lifecycle/#service) that owns an App's
readiness. It is built for [probes attached](#attached-probes) to a server
the App already runs: a [probes server](#probes-servers) owns readiness
itself, so a pod with one returns no health Service. The end user's `Init`
builds one with
[`NewService`](https://pkg.go.dev/github.com/z5labs/humus/health#NewService),
adds monitors to its probes with
[`Liveness`](https://pkg.go.dev/github.com/z5labs/humus/health#Liveness) and
[`Readiness`](https://pkg.go.dev/github.com/z5labs/humus/health#Readiness),
and returns it next to the App's other Services:

```go
Init: func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	hs := health.NewService(
		health.Liveness(&working),
		health.Readiness(health.MonitorFunc(func(ctx context.Context) (bool, error) {
			return ping(ctx) == nil, nil
		})),
	)
	probes <- hs.Probes() // where an HTTP or gRPC Service would expose them
	return []humus.Service{hs}, nil
},
```

[`Service.Probes`](https://pkg.go.dev/github.com/z5labs/humus/health#Service.Probes)
returns its probes. Readiness is `And` of the Service's own state and the
monitors `Readiness` added, in the order they were added, so no monitor is
checked while the Service itself is not ready. Liveness is `And` of the
monitors `Liveness` added, and is healthy when there are none. The probes
report the Service's state when they are checked, so `Probes` may be called
before the App runs. `Liveness` and `Readiness` panic on a nil monitor.

### Readiness through the lifecycle

The Service's own readiness follows the [lifecycle](/humus/pr-preview/pr-1237/lifecycle/#what-run-does-in-order):

1. **Not ready until it is serving.** It is not ready from `NewService`
   until the App calls its `Serve`.
2. **Ready while it serves**, as long as its readiness monitors are healthy.
3. **Not ready from the moment shutdown starts, through the drain.**
   The App cancels `Serve`'s context as soon as it starts shutting down,
   before the drain, and the Service turns not ready then, for good. A
   load balancer checking readiness stops sending the App new traffic while
   the App's HTTP and gRPC Services, which keep serving until `Shutdown`,
   answer what is still draining towards them.

So the health Service is one of the Services that stop on the first shutdown
signal: its `Serve` returns as soon as its context is done. A Service is
served once; a `Serve` called after `Shutdown`, or after an earlier `Serve`,
returns at once and leaves it not ready.

**A health Service built but not returned from `Init` never turns ready.**
`humus.App` runs only the Services `Init` returns, so a Service it is never
handed is never served, and its readiness probe fails for as long as the
process lives. If an App's readiness never passes, check that its health
Service is in the slice `Init` returns.

## Exposing the probes

Two packages serve an App's probes to an orchestrator, one per protocol:

- [`httphealth`](https://pkg.go.dev/github.com/z5labs/humus/httphealth), in
  the core module, answers Kubernetes' `httpGet` probes;
- [`grpc/grpchealth`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpchealth),
  in the `grpc` module, answers
  [gRPC health v1](https://github.com/grpc/grpc/blob/master/doc/health-checking.md),
  which Kubernetes' `grpc` probes, `grpc-health-probe` and grpc-go's
  client-side health checking all ask.

Each has two shapes: a handler or server **attached** to a server the App
already runs, answering from a health Service's `Probes`, and a **probes
server** on a port of its own, which owns readiness itself. The design
behind this section is in
[Proposal: exposing health probes](https://github.com/z5labs/humus/discussions/761),
with its [revision 1](https://github.com/z5labs/humus/discussions/761#discussioncomment-19008091)
and [revision 2](https://github.com/z5labs/humus/discussions/761#discussioncomment-19008236).

Probes are explicit. The end user's `Init` returns a health Service next to
the server its probes are attached to, or a probes server, like any other
Service. No runtime package's `Main` exposes probes by default: humus
imposes no port, so it has nowhere to serve them.

### Which wiring fits the pod

**Attach the probes to a server the pod already runs,** rather than opening
another port. A probes port is only for:

- a pod with no server, such as a consumer's or a long-running job's;
- a server that requires client certificates, which the kubelet never
  presents;
- a gRPC service on a cluster whose `grpc` probes can't use TLS.

| The pod | Its `main` | The probes | Kubernetes |
|---|---|---|---|
| a consumer, or a long-running job | `humus.Main` | `httphealth.NewService` on a port of its own | `httpGet`, `scheme: HTTPS`, on that port |
| an HTTPS API | `httpserver.Main` | a health Service, and `httphealth.NewHandler` attached to the API's mux; `httphealth.NewService` instead when the API requires client certificates | `httpGet`, `scheme: HTTPS`, on the API's port or the probes port |
| a gRPC service, on a cluster whose `grpc` probes can use TLS | `grpcserver.Main` | a health Service, and `grpchealth.NewServer` registered on the end user's Service; `grpchealth.NewService` instead when the Service requires client certificates | `grpc` with `service: liveness` or `readiness`, over TLS on the Service's port, or on the probes port |
| a gRPC service, on any other cluster | `grpcserver.Main` | `grpchealth.NewService` on a plaintext port of its own | `grpc` with `service: liveness` or `readiness`, on that port |
| a Kubernetes Job's pod, usually | `humus.Main` | none | none |

A probes server adds no drain, since nothing routes traffic to it, so a
consumer's or a job's pod with a probes server stays on `humus.Main`.

### Attached probes

[`httphealth.NewHandler`](https://pkg.go.dev/github.com/z5labs/humus/httphealth#NewHandler)
answers the probes from a health Service's `Probes`. It routes on the full
path, so it is mounted at `"/health/"` of the mux the API is served on,
without `http.StripPrefix`, next to the API, and the health Service is
returned from `Init` next to the API's Service:

```go
db := openStore() // the end user's database
hs := health.NewService(health.Readiness(health.MonitorFunc(func(ctx context.Context) (bool, error) {
	if err := db.Ping(ctx); err != nil {
		return false, err // logged, never in the probe's answer
	}
	return true, nil
})))

api := http.NewServeMux()
api.HandleFunc("GET /hello", hello)

mux := http.NewServeMux()
mux.Handle("/", api)
mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))
svc, err := httpserver.NewService(s, settings, mux)
if err != nil {
	return nil, err
}
return []humus.Service{svc, hs}, nil
```

The pod then needs no second port: the kubelet probes the API's own port,
with `scheme: HTTPS`, and doesn't verify its certificate. The handler
recovers no panic of a monitor: the server it is attached to does.

[`grpchealth.NewServer`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpchealth#NewServer)
is the gRPC health v1 server. It is registered on the end user's
[`grpcserver.Service`](/humus/pr-preview/pr-1237/grpc/), next to their own services,
and answers from a health Service returned next to it:

```go
svc, err := grpcserver.NewService(s, settings)
if err != nil {
	return nil, err
}
testpb.RegisterTestServiceServer(svc, greeter{}) // the end user's own service
hs := health.NewService()
healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, hs.Probes()))
return []humus.Service{svc, hs}, nil
```

A `grpcserver.Service` serves only TLS, and the kubelet's `grpc` probes
connect in plain text unless the cluster has the `GRPCContainerProbeTLS`
feature gate on. So the kubelet reaches an attached server only on a
cluster with that gate on, and only when the Service requires no client
certificate. grpc-go's clients reach it on every cluster, for client-side
health checking.

One health Service's `Probes` can go to `NewHandler` and `NewServer` both,
to answer HTTP and gRPC probes from one state.

The kubelet presents no credentials, so attached probes must stay open on a
server that requires authentication. `httphealth.NewHandler` on the end
user's mux is open as long as nothing wraps it: `openapi.Security` covers
only the API's operations, and `auth.NewHandler` only the handler it is
given, so it wraps the API rather than the whole mux. `grpchealth.NewServer`
passes through `grpcauth`'s interceptors like every RPC, so its `Check`,
`Watch` and `List` are opened by name with `auth.Disabled()`, as
[gRPC services](/humus/pr-preview/pr-1237/grpc/#authentication) shows. A probes server
has no authentication.
[Authentication and authorization](/humus/pr-preview/pr-1237/auth/#probes-and-other-open-paths)
covers both.

### Probes servers

[`httphealth.NewService`](https://pkg.go.dev/github.com/z5labs/humus/httphealth#NewService)
and [`grpchealth.NewService`](https://pkg.go.dev/github.com/z5labs/humus/grpc/grpchealth#NewService)
serve the probes, and nothing else, on a port of their own. **A probes
server owns the App's readiness, as a health Service does.** It builds a
health Service from the `health.Option`s it is given, `health.Liveness` and
`health.Readiness`, and runs it for as long as it serves, so the end user's
`Init` returns no health Service next to it. Here a worker, a stand-in for
a consumer, is ready once it has connected:

```go
w := newWorker()
probes, err := httphealth.NewService(s, probeSettings, health.Readiness(w.Connected()))
if err != nil {
	return nil, err
}
return []humus.Service{w, probes}, nil
```

A probes server's readiness follows the
[lifecycle](#readiness-through-the-lifecycle) as a health Service's does:
false until its `Serve` starts, false for good once `Serve`'s context is
done, which is when the App starts shutting down, or once `Shutdown` is
called, and otherwise `And` of the monitors `health.Readiness` added, in
order. Liveness checks the monitors `health.Liveness` added, and is healthy
when there are none. A pod that serves its probes in two ways, such as an
HTTP probes port and `NewServer` registered on its gRPC Service for
client-side health checking, gives each the same monitors.

**A probes server serves through the drain.** It keeps serving until
`Shutdown`, like an HTTP or gRPC Service, so once the App starts shutting
down a readiness probe is answered not ready, rather than refused, and a
liveness probe is still answered healthy.

#### Settings and their checks

`httphealth.Settings` has two fields, `Addr` and `TLS`, and
`grpchealth.Settings` one, `Addr`. None has a default, so neither package
has a `DefaultSettings`, and the end user sources every field, with no
`.Default` in their chains, as
[Configuring a humus service](/humus/pr-preview/pr-1237/configuration/) describes.
Each field has a check that reports what is wrong with a value without
naming the field: `CheckAddr` in both packages, and `httphealth.CheckTLS`.
An unset variable then fails as not found, with exit 78:

```go
probeSettings, err := config.Struct(func(r *config.Resolver) httphealth.Settings {
	return httphealth.Settings{
		// No .Default: both fields are required.
		Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
		TLS:  r.Get(env("PROBES_TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httphealth.CheckTLS)),
	}
}).Read(ctx)
if err != nil {
	return nil, err // exit 78, naming every bad variable
}
```

`readFile` and `serverTLS` are the end user's helpers, as on
[Serving HTTP](/humus/pr-preview/pr-1237/http/#tls). `CheckAddr` rejects an address
that isn't `host:port`, `""` included, and a port that isn't a number from
0 to 65535. `NewService` runs every check again, rejects a nil `Option`,
and listens on `Addr` through the Setup's Host, in `Init`.

#### TLS on the HTTP probes port

**The HTTP probes server serves HTTPS, as every HTTP port humus serves
does,** so its probes set `scheme: HTTPS`. The kubelet doesn't verify the
certificate of such a probe, so the end user's own certificate serves, the
one their other ports serve or any other: the probes need none of their
own. A `tls.Config` shared with an API that requires client certificates
can't be passed as it is, though, as the next paragraph explains: pass a
`Clone` of it with `ClientAuth` set to `tls.NoClientCert`. The server serves a clone of `Settings.TLS`, offering only `http/1.1`
in ALPN, since the kubelet speaks HTTP/1.1, and it serves no HTTP/2. A probe
that leaves out `scheme: HTTPS` is answered 400 by `net/http`, so it fails,
and the failed TLS handshake is logged at ERROR, so the log says why.

**`CheckTLS` rejects a config that requires a client certificate,** one
whose `ClientAuth` is `RequireAnyClientCert` or
`RequireAndVerifyClientCert`. The kubelet presents none, so every probe
would fail and the pod would never turn ready. `VerifyClientCertIfGiven`,
and every weaker `ClientAuth`, is accepted. `CheckTLS` also rejects a nil
config and one with no certificate, where none of `Certificates`,
`GetCertificate` and `GetConfigForClient` is set.

#### The gRPC probes port, in plain text

**The gRPC probes server serves plain text,** because the kubelet's `grpc`
probes use TLS only with the `GRPCContainerProbeTLS` feature gate, which is
alpha and off by default. It answers gRPC health v1 and nothing else: no
service of the end user's is registered on it, so nothing of theirs is
reachable in plain text, and their own services stay TLS-only on the
`grpcserver.Service`'s port:

```go
svc, err := grpcserver.NewService(s, cfg.grpc)
if err != nil {
	return nil, err
}
testpb.RegisterTestServiceServer(svc, greeter{}) // the end user's own service
probes, err := grpchealth.NewService(s, cfg.probes)
if err != nil {
	return nil, err
}
return []humus.Service{svc, probes}, nil
```

`cfg.probes` is the probes server's `Settings`, sourced next to the
`grpcserver.Settings`:

```go
probes: grpchealth.Settings{
	// No .Default: the field is required.
	Addr: r.Get(env("PROBES_ADDR").Validate(grpchealth.CheckAddr)),
},
```

The port stays plaintext until Kubernetes' `grpc` probes use TLS without a
feature gate. TLS is made required on it then, by a proposal of its own.
`Shutdown` ends every stream, a `Watch` included, with `Unavailable`, as a
`grpcserver.Service` does, and then stops the server gracefully.

### The HTTP answers

| Request | Healthy | Unhealthy, or the monitor returned an error |
|---|---|---|
| `GET` or `HEAD /health/liveness` | 200, `OK` | 503, `Service Unavailable` |
| `GET` or `HEAD /health/readiness` | 200, `OK` | 503, `Service Unavailable` |
| another method on either path | 405 | 405 |
| any other path, `/health/` itself included | 404 | 404 |

- **The body is the status text and a newline,** `OK\n` or
  `Service Unavailable\n`, with `Content-Type: text/plain; charset=utf-8`
  and `Cache-Control: no-store`. The kubelet decides on the status code
  alone, so the body says no more than which code it is, for a person with
  `curl`. A `HEAD` gets the same status and headers, and no body. A 405
  carries an `Allow` header.
- **A monitor's error is never in the body.** It can name a host or a
  credential's user, and a probe port is reachable by anything that can
  reach the pod. The error is in the log, as [Telemetry](#telemetry)
  describes.
- **The paths are humus's from before the reset.** No path is a standard
  for workloads: `/livez` and `/readyz` are what Kubernetes' own components
  serve.

### The gRPC answers

| Request | `Check` | `Watch` |
|---|---|---|
| service `""` | readiness | readiness |
| service `readiness` | readiness | readiness |
| service `liveness` | liveness | liveness |
| any other service | `NotFound` | one `SERVICE_UNKNOWN`, and the stream stays open |
| the monitor is healthy | `SERVING` | `SERVING` |
| it is unhealthy, or returns an error | `NOT_SERVING`; an error is logged | `NOT_SERVING`; an error is logged when the status changes |

- **`""` is readiness.** It is the overall status, which a `grpc` probe
  without `service`, `grpc-health-probe` and grpc-go's client-side health
  checking all ask for. `liveness` and `readiness` are the names Kubernetes
  suggests for answering both probes on one port.
- **Any other name is `NotFound`,** the name of a service registered on the
  same server, such as `cart.v1.CartService`, included: health has no
  status per gRPC service.
- **`Watch` polls once a second.** Monitors are checked when asked and send
  no changes, so a `Watch` sends the current status at once, and then each
  change of it. Every `Watch` of one name on one server shares one poller,
  which runs while the name has watchers, so a hundred clients watching
  `""` cost one check a second, not a hundred. Each poll checks with a
  context that is done after a second, and a check that runs out of time is
  `NOT_SERVING`. With a one-second poll, a client using client-side health
  checking learns that a pod is draining within a second of shutdown
  starting, well within `grpcserver.Main`'s 5s drain.
- **`List` answers the three names,** each with its current status.

### Manifests

An HTTPS API with the handler attached is probed on its own port:

```yaml
livenessProbe:
  httpGet:
    path: /health/liveness
    port: 8443 # the API's port
    scheme: HTTPS
readinessProbe:
  httpGet:
    path: /health/readiness
    port: 8443
    scheme: HTTPS
```

A pod with an HTTP probes server is probed on that port, here named
`probes` in the container's `ports`:

```yaml
ports:
  - name: probes
    containerPort: 8081
livenessProbe:
  httpGet:
    path: /health/liveness
    port: probes
    scheme: HTTPS
readinessProbe:
  httpGet:
    path: /health/readiness
    port: probes
    scheme: HTTPS
```

A gRPC service with a plaintext probes port names the probe in its
`service` field:

```yaml
livenessProbe:
  grpc:
    port: 8081 # the probes port; the end user's services are served over TLS on their own
    service: liveness
readinessProbe:
  grpc:
    port: 8081
    service: readiness
```

On a cluster with `GRPCContainerProbeTLS` on, the same `grpc` probes go to
the `grpcserver.Service`'s own port instead, where `grpchealth.NewServer`
is registered, with the TLS setting the gate adds; see Kubernetes'
[Use TLS with gRPC probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/#grpc-probe-tls)
and [gRPC services](/humus/pr-preview/pr-1237/grpc/#health-probes).

### Startup probes

**There is no startup probe of its own, and `Probes` has no field for
one.** A probes server answers only once its `Serve` runs, and the App
calls `Serve` only after `Init` has returned. While `Init` runs, a probe's
connection waits in the listener's backlog, or is refused if the listener
isn't open yet, and the probe times out or fails. So a startup probe on the
liveness path succeeds exactly when `Init` is done, which is Kubernetes'
own advice:

```yaml
startupProbe:
  httpGet:
    path: /health/liveness
    port: probes
    scheme: HTTPS
  failureThreshold: 30 # 30 × 2s: Init has a minute
  periodSeconds: 2
```

A service whose `Init` is slow, such as one that waits for a database or
runs migrations, sets a startup probe, so liveness doesn't restart it
before it has started. One that starts fast needs none.

### Checks

- **No timeout of the handler's own, and no caching.** Each probe request
  checks its monitor once, with the request's context. The kubelet's
  `timeoutSeconds` closes the connection, which cancels an HTTP request's
  context, and sets a gRPC call's deadline, so a monitor that
  [honours its context](#monitors) is bounded by the probe. The kubelet
  checks each probe every `periodSeconds`, 10s by default, so a database
  ping in readiness costs one ping per pod every 10 seconds.
- **A monitor that is too expensive to check that often caches its own
  result,** in the end user's `MonitorFunc`. A cache in humus would need a
  setting for its age, and would answer a probe with a result older than
  the probe asked for. The one shared check is `Watch`'s poller, above.
- **A probes port isn't for the public.** A probes server answers anyone
  who can reach the port, and every request checks a monitor. The port is
  the container's, for the kubelet, and isn't routed by a Kubernetes
  Service or an ingress.

### Telemetry

- **A monitor's error is one ERROR record,** through the Setup's
  `LoggerProvider`, under the package's import path,
  `github.com/z5labs/humus/httphealth` or
  `github.com/z5labs/humus/grpc/grpchealth`, in the request's context. Its
  body is the error, and its attribute `humus.health.probe` is `liveness`
  or `readiness`. A monitor that is unhealthy without an error, such as the
  health Service's own readiness once the App starts shutting down, logs
  nothing.
- **Probes servers record no spans and no metrics,** and extract no trace
  context: a probe every few seconds would be most of a quiet service's
  traces. They log `listening` at INFO when they start, and the HTTP
  probes server logs each line of `net/http`'s error log, such as a TLS
  handshake error, at ERROR.
- **Attached probes are traced like any request.** The handler mounted on
  an `httpserver.Service`, or the server registered on a
  `grpcserver.Service`, sits inside that Service's instrumentation, which
  records every request, and a monitor's ERROR record carries the trace of
  the probe's span. A team that doesn't want the probes' spans drops them
  in its sampler.

### A consumer's readiness

**A Kafka consumer is ready once it has joined its group.**
[`Joined`](/humus/pr-preview/pr-1237/kafka/#readiness) returns a `health.Monitor`
that is unhealthy until franz-go first hands the member an assignment,
empty or not, and healthy from then until `Serve` returns. A consumer's pod
has no server of its own, so its probes go on a probes server, with
`health.Readiness(consumer.Joined())`:

```go
consumer, err := kafka.NewService(s, settings, nop)
if err != nil {
	return nil, err
}
probes, err := httphealth.NewService(s, probeSettings, health.Readiness(consumer.Joined()))
if err != nil {
	return nil, err
}
return []humus.Service{consumer, probes}, nil
```

Nothing routes traffic to a consumer, so its readiness gates rollouts and
disruption budgets. A rollout whose new pods can't reach the brokers then
stalls at its progress deadline, with the old pods still consuming, rather
than replacing them with pods that never consume. A broker outage after the
first join doesn't make the consumer unready: it hits every pod at once, and
would only block a rollout that might be the fix.

**A RabbitMQ consumer is ready once it consumes.**
[`Consuming`](/humus/pr-preview/pr-1237/rabbitmq/#readiness) returns a `health.Monitor`
that is unhealthy until the broker first answers the Service's
`basic.consume`, and healthy from then until `Serve` returns, through any
lost connection. It goes in the same place, as
`health.Readiness(consumer.Consuming())`, for the same reasons.

### A gRPC dependency's readiness

**A service that calls a gRPC dependency is ready once its Client has
connected.** [`Connected`](/humus/pr-preview/pr-1237/grpc-client/#readiness-connected)
returns a `health.Monitor` that is unhealthy until the
[`grpcclient`](/humus/pr-preview/pr-1237/grpc-client/) Client's connection to its
`Addr` is first ready, its TLS handshake done, and healthy from then until
the connection is closed. It makes no RPC. Here a gRPC service, on a cluster
whose `grpc` probes can't use TLS, puts it in the readiness of its probes
port:

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

`NewClient` dials nothing, so without it a release with a wrong address, a
CA that doesn't trust the dependency, or a dependency it can't reach turns
ready, replaces the pods that worked, and fails every RPC. With it, the
rollout stalls at its progress deadline, with the old pods still serving.
Its first check dials, and waits until the connection is ready or the
probe's own timeout ends the check.

**`Connected` latches,** as `Joined` does, for the same reason. Once the
connection has been ready, the monitor stays healthy whatever the
connection's state, through a GOAWAY, a lost connection or an outage of the
dependency. An outage hits every pod at once: turning them all unready would
take the service out of its load balancer, so its callers would get no
answer instead of its own error, and the parts of it that don't need the
dependency would stop too, and it would block a rollout that might be the
fix. A monitor that calls the dependency's `grpc.health.v1` service is the
end user's own, and turns unhealthy in exactly that outage, as
[Calling gRPC services](/humus/pr-preview/pr-1237/grpc-client/#a-monitor-of-the-dependencys-grpchealthv1)
shows. **`Connected` is for readiness only:** restarting the pod doesn't
bring a dependency back.

**`Joined` is for readiness only.** In liveness, it would restart a pod
that starts while the brokers are unreachable, before it has ever joined,
and the kubelet's restart backoff would then hold it down after the brokers
come back. A liveness monitor that turned unhealthy whenever the member is
out of its group would do the same to every pod in a broker outage, while
franz-go rejoins on its own. The errors a restart would fix already end
`Serve`, so the pod exits and is restarted without a probe.

### Jobs

**A Kubernetes Job's pod usually needs no probes.** Nothing routes to it,
and a job that hangs is ended by its `activeDeadlineSeconds`, when it sets
one. So a job's `Init` returns the job and nothing else, and needs no health
Service: nothing in the lifecycle asks for readiness.

A long-running job that wants a liveness probe returns
`httphealth.NewService`'s Service next to its
[`job.Service`](/humus/pr-preview/pr-1237/jobs/#more-than-the-job), and stays on
`humus.Main`. The probe is only as good as its liveness monitor: with none,
it checks only that the process answers, so a job that can hang gives it a
monitor of its own progress, here `progress`, which the run keeps healthy
while it makes progress:

```go
probes, err := httphealth.NewService(s, probeSettings, health.Liveness(progress))
if err != nil {
	return nil, err
}
return []humus.Service{
	probes,
	job.NewService(s, run),
}, nil
```

When the job finishes, the App ends, and the probes server with it.

### Testing the probes

A probes server is tested in a whole App, run with
[`humustest.Start`](/humus/pr-preview/pr-1237/testing/) on a humustest Host, under
`testing/synctest`, with a certificate from `humustest.NewCA`. This is
`httphealth`'s own test that an API's pod with a probes port is not ready
during the drain:

```go
func TestInit_is_not_ready_during_the_drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)

		// apiInit returns an API on :8443 and a probes server on :8081, and
		// no health Service: the probes server owns readiness.
		apiInit := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			settings := httpserver.DefaultSettings()
			settings.Addr = ":8443"
			settings.TLS = &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}
			api, err := httpserver.NewService(s, settings, http.NotFoundHandler())
			if err != nil {
				return nil, err
			}
			probes, err := httphealth.NewService(s, probeSettings(t, ca))
			if err != nil {
				return nil, err
			}
			return []humus.Service{api, probes}, nil
		}

		p, status := humustest.Start(t, apiInit,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder), httpserver.Defaults()),
		)
		<-h.Listening("localhost:8081")
		synctest.Wait()

		c := kubelet(h, ca)
		require.Equal(t, http.StatusOK, probe(t, c, readinessURL))

		p.Signal(syscall.SIGTERM)
		time.Sleep(time.Second) // inside the 5s drain
		require.Equal(t, http.StatusServiceUnavailable, probe(t, c, readinessURL))
		require.Equal(t, http.StatusOK, probe(t, c, livenessURL))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())
	})
}
```

The API stands in for one that requires client certificates, the case for
a probes port next to an API; one that doesn't attaches its probes, and is
tested the same way, with the probes requested on the API's port.
`probeSettings` returns `httphealth.Settings` on `:8081` with a certificate
for `localhost` from the CA, and `probe` makes the request, reads and
closes the body, and returns the status code. `kubelet` is the Host's HTTP
client, given the CA's pool as its roots, speaking HTTP/1.1 alone, as the
kubelet does:

```go
// kubelet returns h's HTTP client, trusting only ca, and speaking HTTP/1.1
// alone, as the kubelet does.
func kubelet(h *humustest.Host, ca *humustest.CA) *http.Client {
	c := h.HTTPClient()
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: ca.Pool(), NextProtos: []string{"http/1.1"}}
	var protos http.Protocols
	protos.SetHTTP1(true)
	tr.Protocols = &protos
	return c
}
```

A gRPC probes port is asked with
[`grpcservertest.NewClient`](/humus/pr-preview/pr-1237/grpc/#a-plaintext-server),
given `insecure.NewCredentials()` and `nil` roots, since the port serves
plain text, as the kubelet's probes speak it:

```go
// plaintext returns a client of the health server on address in h that
// speaks plain text, as the kubelet's grpc probes do.
func plaintext(t *testing.T, h *humustest.Host, address string) healthpb.HealthClient {
	t.Helper()
	return healthpb.NewHealthClient(grpcservertest.NewClient(t, h, address, nil,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	))
}
```

An attached server is asked the same way, with the CA's pool in place of
`nil` and no transport credentials, over TLS on the Service's port.

Both probes servers pass the Service contract, checked with
[`humustest.TestService`](/humus/pr-preview/pr-1237/testing/#checking-a-service). They
keep serving after `Serve`'s context is done, so its subtest for `Shutdown`
after `Serve` has returned on its own reports SKIP:

```go
func TestService(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		cert := humustest.NewCA(t).Issue(t, "localhost")
		svc, err := httphealth.NewService(s, httphealth.Settings{
			Addr: ":8081",
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		})
		require.NoError(t, err)
		return svc
	})
}
```

### Migration from `rest.Liveness`, `rest.Readiness` and `internal/grpchealth`

| Before the reset | Now |
|---|---|
| `GET /health/liveness` and `/health/readiness` on every `rest.Api`, 200 by default | `httphealth.NewHandler` attached to the API's mux, or `httphealth.NewService` on a port of its own, answering from a health Service |
| `rest.Liveness(h)`, `rest.Readiness(h)` with a handler of the end user's | `health.Liveness(m)`, `health.Readiness(m)` with a `Monitor`, given to the probes server, or to the health Service when the probes are attached |
| `grpc.Api.RegisterService` health-checking an implementation that is a `health.Monitor` | nothing implicit: the monitor goes in the health Service's readiness |
| `internal/grpchealth` | `grpchealth.NewServer`, or `grpchealth.NewService` on a plaintext port |
| no answer for the service `""` | `""` is readiness, and `liveness` and `readiness` are named |
| a monitor's error as `SERVICE_UNKNOWN` | `NOT_SERVING`, and the error logged |
| `Watch` polling every 10s, per stream | every second, once per server and name |
| the global `humus.Logger` | `s.Telemetry.LoggerProvider` |

## Testing

A health Service is tested in a whole App, with
[bedrocktest](https://pkg.go.dev/github.com/z5labs/bedrock/bedrocktest)'s
fake process and a [humustest](/humus/pr-preview/pr-1237/testing/) Host, under
[`testing/synctest`](https://pkg.go.dev/testing/synctest), so the drain
passes in fake time and readiness can be checked while it runs. This is
`health`'s own test that the Service is not ready during the drain:

```go
func TestServiceIsNotReadyDuringTheDrainDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := health.NewService()
		readiness := hs.Probes().Readiness
		api := newServer() // keeps serving, so the drain lasts its whole delay
		app := humus.App{
			Host:                humustest.NewHost(t).Host(),
			Telemetry:           telemetry.NoopBuilder,
			ShutdownGracePeriod: 10 * time.Second,
			Drain:               humus.DrainDelay(5 * time.Second),
			Init: func(context.Context, *humus.Setup) ([]humus.Service, error) {
				return []humus.Service{api, hs}, nil
			},
		}

		p := bedrocktest.NewProcess(t)
		status := make(chan bedrock.ExitStatus, 1)
		go func() { status <- app.Run(t.Context(), p.Process()) }()

		<-p.Notified(syscall.SIGTERM)
		synctest.Wait() // Serve has started
		ready, err := readiness.Healthy(t.Context())
		require.NoError(t, err)
		require.True(t, ready)

		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		synctest.Wait() // shutdown has started; the drain is running
		ready, err = readiness.Healthy(t.Context())
		require.NoError(t, err)
		require.False(t, ready)

		require.Zero(t, (<-status).Code())
		require.Equal(t, 5*time.Second, time.Since(sigterm))
	})
}
```

`<-p.Notified(syscall.SIGTERM)` waits until the App listens for signals, and
`synctest.Wait` until every goroutine in the test, `Serve` included, is
blocked, so the first check sees the Service serving. After the SIGTERM,
`synctest.Wait` returns while the App waits out its five-second drain, and
readiness is already false. The drain ends once every `Serve` has returned,
and the health Service's `Serve` returns as soon as shutdown starts, so the
test runs it next to `api`, a Service that keeps serving until `Shutdown` as
an HTTP or gRPC Service does: that is what keeps the drain running for its
whole five seconds. A test of an App's own readiness monitors works the
same way: take the probes from the Service, run the App, and check them at
each point of the lifecycle.
