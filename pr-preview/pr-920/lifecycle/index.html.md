# The App lifecycle

Every humus application is a [`bedrock.App`](https://pkg.go.dev/github.com/z5labs/bedrock#App),
and the root package's [`humus.App`](https://pkg.go.dev/github.com/z5labs/humus#App)
is the lifecycle every humus service runs. It reads its settings, listens for
signals, builds its telemetry and the user's dependencies, runs one or more
Services, drains and shuts them down within a grace period, releases what was
built, shuts its telemetry down and turns the outcome into an exit status.

A service's `main` is `func main() { humus.Main(app.Init) }`, where
`app.Init` is the end user's `Init`. [`humus.Main`](#main) builds the
`humus.App` from its [defaults](#the-defaults) and that `Init`, applies the
options it is given, and runs the App on the OS process. Options change the
App's fields, so everything this page says about `humus.App` is what `Main`
runs. The runtime packages — REST, gRPC, jobs, queues — export Services for
`Init` to return, not Apps, and one whose Services need a drain wraps `Main`
with defaults of its own: see [Runtime packages](#runtime-packages). An App
that mixes runtimes, such as a REST API and a gRPC service in one process,
needs nothing more.

Telemetry is part of the lifecycle: the App builds it before `Init`, logs
through it, and shuts it down last. See [Telemetry](#telemetry) below, and
the [Telemetry](/humus/pr-preview/pr-920/telemetry/) page for what a service does with
it. Health is a Service like any other: an App's `Init` returns the `health`
package's Service next to its other Services, and
[Health](/humus/pr-preview/pr-920/health/) explains what it does to readiness through
the lifecycle.

The design behind everything on this page is in
[Proposal: the humus App lifecycle](https://github.com/z5labs/humus/discussions/630#discussioncomment-18964173)
and [Proposal: humus.Main](https://github.com/z5labs/humus/discussions/687),
with its [revision 1](https://github.com/z5labs/humus/discussions/687#discussioncomment-18983867)
and [revision 2](https://github.com/z5labs/humus/discussions/687#discussioncomment-18986870).
The telemetry integration is in
[Proposal: OpenTelemetry without globals, Revision 2](https://github.com/z5labs/humus/discussions/632#discussioncomment-18966249).

## Main

[`humus.Main(init, opts...)`](https://pkg.go.dev/github.com/z5labs/humus#Main)
runs an App on the OS process and exits the process with the App's status.
It never returns, and only `init` is required. This `main` passes the end
user's `Init` and one option, for its own telemetry:

```go
humus.Main(Init, humus.WithTelemetry(buildTelemetry))
```

`Main` builds the App before it runs anything:

1. It starts from `Main`'s [defaults](#the-defaults) for `Host`,
   `Telemetry`, `ShutdownGracePeriod` and `Drain`.
2. It sets `Init` to `init`, so an option sees the end user's `Init` and may
   wrap it, for example to add a Service next to the end user's.
3. It applies `opts` in order, so a later option wins over an earlier one.

It is then `bedrock.Main` of that App, the only place a humus program touches
the real process. It panics if `init` or one of `opts` is nil.

An option is a [`humus.Option`](https://pkg.go.dev/github.com/z5labs/humus#Option),
a `func(*humus.App)`. humus has one for each field `Main` defaults:

| Option | Sets |
|---|---|
| `humus.WithHost(h)` | `App.Host` |
| `humus.WithTelemetry(b)` | `App.Telemetry`, the App's `telemetry.Builder` |
| `humus.WithShutdownGracePeriod(d)` | `App.ShutdownGracePeriod`, the grace period when `HUMUS_SHUTDOWN_GRACE_PERIOD` is not set |
| `humus.WithDrain(d)` | `App.Drain`, the [drain](#the-drain) when `HUMUS_SHUTDOWN_DRAIN_DELAY` is not set. `humus.WithDrain(nil)` removes a drain that an earlier option set |

An option changes exported fields, so an end user or a runtime package can
write one of their own as a function literal, and one option may set several
fields.

`buildTelemetry` is the end user's `Builder`. It starts from the telemetry
package's defaults, which write log records to standard output, and names the
service:

```go
func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "greeter"
	return telemetry.New(ctx, s, dial, p.Stdout)
}
```

A `main` that wants none of `Main`'s defaults can still write
`bedrock.Main(humus.App{…})`, and sets every field itself:

```go
bedrock.Main(humus.App{
	Host:                humus.OSHost(),
	ShutdownGracePeriod: 30 * time.Second,                  // when HUMUS_SHUTDOWN_GRACE_PERIOD is not set
	Drain:               humus.DrainDelay(5 * time.Second), // when HUMUS_SHUTDOWN_DRAIN_DELAY is not set
	Telemetry:           buildTelemetry,
	Init:                Init,
})
```

## The defaults

| Field | `Main`'s default | Why |
|---|---|---|
| `Host` | `humus.OSHost()` | `Main` runs only in `main`. A test runs its App with `humustest.Start`, which uses a test Host. |
| `Telemetry` | `telemetry.New` with `telemetry.DefaultSettings()`, the Host's `Dial` and the Process's stdout | Logs go to stdout as JSON lines, and spans get real IDs without being exported. A service that exports anything passes its own `Builder` with `humus.WithTelemetry`. |
| `ShutdownGracePeriod` | 20s | It fits inside Kubernetes' default `terminationGracePeriodSeconds` of 30s, with telemetry's 5s shutdown after it and a margin for the process to exit. |
| `Drain` | none | Only a Service that a load balancer routes to, and that keeps serving until `Shutdown`, needs a drain, and the package that provides it sets one. |

The worst case from a signal to the exit is 20s of grace period and 5s of
telemetry shutdown, `telemetry.Settings.ShutdownTimeout`: 25s, inside
Kubernetes' 30s. A drain comes out of the same budget, so a runtime package
that sets one shortens its grace period to fit.

The defaults live in `Main`, not in `App`. `humus.App`'s fields keep their
meaning: a zero `ShutdownGracePeriod` fails startup, and a nil `Drain` doesn't
wait. An operator overrides the grace period and the drain through their
[variables](#settings), whatever `Main` or an option set, without a rebuild.

## Runtime packages

A [job](/humus/pr-preview/pr-920/jobs/), a queue consumer, of
[Kafka](/humus/pr-preview/pr-920/kafka/) or [RabbitMQ](/humus/pr-preview/pr-920/rabbitmq/),
and the health Service stop on `Serve`'s context, so they run on `humus.Main`'s defaults and need no `Main` of their own. A
runtime package whose Services need a drain, such as an HTTP or gRPC server
that keeps serving until `Shutdown`, wraps `Main` instead:

- It exports its defaults as one `humus.Option`, so that a test runs with
  them too.
- Its `Main` applies those defaults and then the end user's options, and
  calls `humus.Main`. The end user's options come last, so they win over the
  package's.

The HTTP server's `Main` and defaults are `httpserver.Main` and
`httpserver.Defaults`, with a 5s drain and a 10s grace period, which fit
inside the 25s budget with telemetry's 5s:
[Serving HTTP](/humus/pr-preview/pr-920/http/#the-defaults) explains them. The gRPC
server's are `grpcserver.Main` and `grpcserver.Defaults`, with a 5s drain and
a 15s grace period, which use the whole 25s budget with telemetry's 5s:
[gRPC services](/humus/pr-preview/pr-920/grpc/#the-defaults) explains them. The root
package's tests don't import a runtime package, so the samples on this page
use stand-ins of the same shape:

```go
// httpDefaults stands in for a runtime package's defaults, such as
// httpserver.Defaults: a 5s drain and a 10s grace period over humus.Main's.
func httpDefaults() humus.Option {
	return func(a *humus.App) {
		a.Drain = humus.DrainDelay(5 * time.Second)
		a.ShutdownGracePeriod = 10 * time.Second
	}
}

// httpMain stands in for a runtime package's Main, such as httpserver.Main:
// humus.Main with the package's defaults applied before the end user's
// options, so that the end user's options win.
func httpMain(init func(ctx context.Context, s *humus.Setup) ([]humus.Service, error), opts ...humus.Option) {
	humus.Main(init, append([]humus.Option{httpDefaults()}, opts...)...)
}
```

The end user's `main` calls the package's `Main`, and its test passes the
package's defaults with `humustest.WithApp(httpDefaults())`, as
[Testing](/humus/pr-preview/pr-920/testing/#runtime-packages) shows, or with the
package's own `Start`, such as `httpservertest.Start` or
`grpcservertest.Start`:

```go
httpMain(Init, humus.WithTelemetry(buildTelemetry))
```

**A process picks its `Main` by what a load balancer routes to.** It uses an
HTTP or gRPC package's `Main` when that server is one a load balancer routes
to, as an API next to a job is. A job or a consumer next to a probes or
metrics server stays on `humus.Main`: nothing routes to that server, and a
drain would make a finished job wait for the whole delay.

**In a mixed process, the package's budget is everyone's.** A job or a
consumer next to a server that is routed to runs on that server's package's
`Main`, so its work in flight at a signal gets that package's drain and grace
period: a 5s drain and a 10s grace period on `httpserver.Main`'s defaults, and
a 5s drain and a 15s grace period on `grpcserver.Main`'s, rather than
`humus.Main`'s 20s grace period with no drain. An end user who needs more
passes `humus.WithShutdownGracePeriod` after the package's defaults. On
`httpserver.Main`'s defaults that fits within the 25s budget up to a 15s grace
period. `grpcserver.Main`'s defaults already use the whole 25s, so a longer
grace period there needs `terminationGracePeriodSeconds` raised to match, as
[gRPC services](/humus/pr-preview/pr-920/grpc/#terminationgraceperiodseconds)
describes. An HTTP and a gRPC server in one process use either package's `Main`.
Each package's defaults set both the drain and the grace period, so passing
the other package's defaults as well replaces both: the last one wins.

## App

An App has five fields:

| Field | Required | Meaning |
|---|---|---|
| `Host` | yes | the network and filesystem the App runs on: `humus.OSHost()` under `Main`, and a Host the test controls in a test |
| `ShutdownGracePeriod` | yes, unless `HUMUS_SHUTDOWN_GRACE_PERIOD` is set | the grace period when `HUMUS_SHUTDOWN_GRACE_PERIOD` is not set: 20s under `Main`. It must be positive, so the zero value fails startup |
| `Drain` | no | the [drain](#the-drain) when `HUMUS_SHUTDOWN_DRAIN_DELAY` is not set: a `humus.Drain`, such as `humus.DrainDelay(5 * time.Second)`, that runs between shutdown starting and `Shutdown` being called on the Services, and whose context is done once every `Serve` has returned. A nil `Drain`, `Main`'s default, means no pause |
| `Telemetry` | yes | the [`telemetry.Builder`](https://pkg.go.dev/github.com/z5labs/humus/telemetry#Builder) that builds the App's telemetry: under `Main`, its [default](#the-defaults) or the end user's own `Builder`, and in tests also `telemetry.NoopBuilder` or the `Builder` of humustest's recorder, `humustest.Telemetry` |
| `Init` | yes | builds the App's dependencies and returns the Services to run |

`ShutdownGracePeriod` and `Drain` are defaults: an operator overrides
either through its [variable](#settings) without a rebuild. How long to
drain and how long to allow for shutdown depend on what the App runs — a
REST or gRPC App needs time for a load balancer to stop sending it
connections, and a job does not — so `Main` has opinions about them, the
runtime packages that need other ones set their own, and the App literal has
none.

## Service

A [`Service`](https://pkg.go.dev/github.com/z5labs/humus#Service) is a
long-running part of an App: an HTTP server, a gRPC server, a
queue consumer, of [Kafka](/humus/pr-preview/pr-920/kafka/) or
[RabbitMQ](/humus/pr-preview/pr-920/rabbitmq/), or a job. It has two methods, `Serve` and `Shutdown`, and the App gives it two
shutdown signals, at different times:

1. **`Serve`'s context is done** as soon as the App starts shutting down,
   before the drain.
2. **`Shutdown` is called** after the drain, with the grace period as
   its context's deadline.

A Service may stop on the first, as a
[job](/humus/pr-preview/pr-920/jobs/#the-two-signals), a
queue consumer, of [Kafka](/humus/pr-preview/pr-920/kafka/#shutdown-and-rebalances)
or [RabbitMQ](/humus/pr-preview/pr-920/rabbitmq/#shutdown), or the
[health Service](/humus/pr-preview/pr-920/health/#the-health-service) does: the health
Service turns not ready then, so a load balancer stops sending the App new
traffic. Or it may keep serving until the second, as an HTTP or gRPC Service
does, so that a load balancer still draining connections towards it is
answered. This
Service keeps serving until `Shutdown`:

```go
type server struct {
	out     io.Writer
	ready   chan struct{}
	stopped chan struct{}
}

func newServer(out io.Writer) *server {
	return &server{
		out:     out,
		ready:   make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func (s *server) Serve(ctx context.Context) error {
	_, err := fmt.Fprintln(s.out, "serve: serving")
	close(s.ready)
	if err != nil {
		return err
	}
	<-s.stopped
	return nil
}

func (s *server) Shutdown(ctx context.Context) error {
	close(s.stopped)
	_, err := fmt.Fprintln(s.out, "shutdown: stopped serving")
	return err
}
```

Closing `ready` tells the [example](https://pkg.go.dev/github.com/z5labs/humus#example-App)
that runs it when it is serving. `Serve` returns `nil` when `Shutdown` stopped it or when it finished on its
own. Any Service returning from `Serve` ends the App, so a job is just a
Service that returns, and an App is never partly up. `Shutdown` is called on
every Service, including one whose `Serve` has already returned, so it must be
safe to call then. If its context is done before the Service has stopped,
`Shutdown` stops it at once and returns an error that wraps the context's
error.

## The drain

The drain is the pause between shutdown starting and `Shutdown` being called
on the Services, so that load balancers stop sending the App new work before
its servers stop. It is a function,
[`humus.Drain`](https://pkg.go.dev/github.com/z5labs/humus#Drain), so that
what it waits for is the end user's choice, and so that it can stop waiting
once nothing is left to drain: a
`func(ctx context.Context, s *humus.Setup) error`. `App.Drain` holds it. `Main` sets none, and a nil `Drain` means no pause.

**It ends once every `Serve` has returned.** Run calls the Drain after
`Serve`'s context is done, when it hasn't yet received every `Serve`'s
result, with the `Setup` that `Init` was given. The Drain's context is done
once every Service's `Serve` has returned, because nothing is left to drain.
A Service that stops on `Serve`'s context returns at once or once its work in
flight is done, and a Service that keeps serving until `Shutdown` doesn't
return until after the drain. So, for an App with a Drain set:

| The App's Services | The drain lasts |
|---|---|
| a job, finished on its own | nothing: Run doesn't call the Drain |
| a job, finished on its own, next to the health Service | nothing: Run calls the Drain, and its context is done at once |
| a job, interrupted by a signal | until the run returns, at most the Drain's delay |
| a queue consumer, of [Kafka](/humus/pr-preview/pr-920/kafka/#shutdown-and-rebalances) or [RabbitMQ](/humus/pr-preview/pr-920/rabbitmq/#shutdown) | until its messages in flight are done, at most the Drain's delay |
| an HTTP or gRPC server, with or without the health Service | the Drain's whole delay |
| a job and an HTTP server | the Drain's whole delay, unless both return first |

On `humus.Main`'s defaults there is no Drain, so none of these drain. An
`Init` that returns no Services ends the App without a drain too, because
there is nothing to drain.

**`humus.DrainDelay(d)` is the usual kind of drain.** It waits `d`, or until
its context is done, and returns nil either way. A runtime package that needs
a drain sets one in its defaults, and an end user passes one with
`humus.WithDrain(humus.DrainDelay(5 * time.Second))`. It doesn't look at the
shutdown's cause: after a Service returned on its own, it still gives load
balancers its delay to notice that the pod is no longer ready. It panics if
`d` is negative.

**A Drain returns nil when its context is done.** A done context is how a
Drain learns that nothing is left to drain, which is a normal end, not a
failure: a Drain that returns `ctx.Err()` then fails the App, with
`drain: context canceled` in the cause. Run may call a Drain whose context is
done at once, as for a finished job next to the health Service, so a Drain
checks its context before it acts. A Drain returns when it is done or when
its context is done, whichever comes first.

**The Drain is not bounded by the grace period.** Run waits for it to return,
and the grace period starts after it. A Drain that ignores its context holds
the App up until a second signal, or Kubernetes' SIGKILL, ends the process.
Its context keeps the values of Run's context but is not cancelled with it.
An error it returns is logged at ERROR, joined into the App's cause as
`drain: ` and the error, and exits 1, and `Shutdown` is still called on every
Service.

**A Drain of the end user's own checks `ShutdownCause` before it waits on a
load balancer.** [`humus.ShutdownCause(ctx)`](https://pkg.go.dev/github.com/z5labs/humus#ShutdownCause)
says why the shutdown started: after a signal, it is the signal's cause, and
Kubernetes is terminating the pod and removing it from load balancers. When a
Service returned on its own first, such as a job next to an API, it wraps
`humus.ErrServiceReturned`, and nothing outside is draining the App. A Drain
that waited for deregistration then would wait for something nothing
triggers. This Drain polls a load balancer through `s.Host.Dial`, so that it
runs on a test Host too, and only after a signal:

```go
func deregisteringDrain(ctx context.Context, s *humus.Setup) error {
	if errors.Is(humus.ShutdownCause(ctx), humus.ErrServiceReturned) {
		return nil // no signal came, so nothing is deregistering the pod
	}
	return waitUntilDeregistered(ctx, s.Host.Dial)
}
```

`waitUntilDeregistered` returns nil when `ctx` is done. The end user passes
the Drain with `humus.WithDrain`:

```go
humus.Main(Init, humus.WithDrain(deregisteringDrain))
```

The Drain gets the `Setup`, not what `Init` built, because it is chosen in
`main`, before `Init` runs. Work at shutdown that needs `Init`'s dependencies,
such as deregistering from service discovery, belongs in a Service that stops
on `Serve`'s context: it sees shutdown start at the same moment, and the Drain
then waits for whatever has to follow it.

**`HUMUS_SHUTDOWN_DRAIN_DELAY` replaces any Drain.** When it is set, Run uses
`humus.DrainDelay` of its value in place of `App.Drain`, whatever that is, and
adds a drain to an App that has none. An operator can always replace a Drain
that misbehaves, from the same manifest that sets
`terminationGracePeriodSeconds`. See [Settings](#settings).

## Host

A [`Host`](https://pkg.go.dev/github.com/z5labs/humus#Host) is what humus
hands an App beyond its process: `Listen`, `Dial` and a filesystem, `FS`. Every
field is required. Under `humus.Main` an App runs on `humus.OSHost()`, which is
`net.Listen`, a `net.Dialer`'s `DialContext`, and an `FS` that calls package
`os` directly. In a test it runs on a Host whose listeners, dialler and
filesystem the test controls, so that a whole App runs without touching the
real host.

`Host` is meant for user code as well as for humus. Hand `Host.Dial` to a
client that takes a dial function, such as pgx's `DialFunc`, and open files
through `Host.FS` rather than through package `os`.

### Why `FS` hands out `*os.File`

`FS` names files by OS path and opens them as `*os.File`, rather than through
an in-memory filesystem:

- Libraries that need a real file or a path keep working in a test: SQLite
  drivers, archive libraries, programs started with `os/exec`.
- There is no fake filesystem to build and keep in line with the real one.
- It is a one-way decision. An `*os.File` is always an OS handle, so moving to
  an in-memory filesystem later would change every signature.

`OSHost`'s `FS` is not `os.OpenRoot("/")`, because an `os.Root` refuses to
follow absolute symbolic links even when it is rooted at `/`, and paths on a
real host routinely pass through them.

What it costs a test: the rule is that it opens no file outside its own
temporary directory; it cannot make a write fail with a full disk, so code
that needs that test puts an `io.Writer` seam in its own code; and file
modification times are real under `synctest`, so code that compares
`ModTime()` with `time.Now()` needs `Chtimes` in its tests.

## Setup and OnStop

`Init` is given a [`*humus.Setup`](https://pkg.go.dev/github.com/z5labs/humus#Setup)
holding the `bedrock.Process` the App is run with, the App's `Host` and its
telemetry, `Setup.Telemetry`. `Init` reads its configuration from
`Setup.Process`, builds its dependencies on `Setup.Host`, takes their tracers,
meters and loggers from `Setup.Telemetry`, and returns the Services to run. Its context is done when the
App receives an interrupt or SIGTERM, so a slow `Init`, such as one retrying a
database connection, is cancelled by a signal.

`Setup.OnStop` registers a function that releases something `Init` built. The
functions work like `t.Cleanup`: they run in reverse order of registration
once every Service has stopped, and they run when `Init` fails partway
through too. So register each release as soon as the thing it releases is
built:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	out := s.Process.Stdout
	s.OnStop(func(context.Context) error {
		_, err := fmt.Fprintln(out, "on stop: close the cache")
		return err
	})
	s.OnStop(func(context.Context) error {
		_, err := fmt.Fprintln(out, "on stop: close the queue")
		return err
	})

	if _, err := config.Env(s.Process.Environment, "DATABASE_URL").Read(ctx); err != nil {
		return nil, err // exit 78: init: config DATABASE_URL: not found
	}
	return []humus.Service{newServer(out)}, nil
}
```

Run without `DATABASE_URL`, this App closes the queue and then the cache, logs
`init: config DATABASE_URL: not found` at ERROR through its telemetry, and
exits 78.

The OnStop functions share the grace period with the Services' `Shutdown`:
the context each is given is done when the grace period runs out.

## What Run does, in order

1. Reads the lifecycle's [settings](#settings) from the process's
   environment, defaulting each to its App field: under `Main`, that is
   `Main`'s default, or what an option set. A malformed one, or a bad
   default, exits 78 before the App listens for signals, and `Init` is never
   called.
2. Listens for an interrupt and SIGTERM through the process's
   `NotifyContext`, before the telemetry and `Init`, so a signal during a slow
   `Builder` or a slow `Init` cancels it.
3. Builds the [telemetry](#telemetry) by calling `App.Telemetry`. If it
   fails, `Init` is never called, and the App exits 78 when a `*config.Error`
   is in the error chain, or 1 otherwise.
4. Sets OpenTelemetry's process-wide error handler, which writes each export
   error to standard error.
5. Calls `Init`. If it fails, runs the OnStop functions registered so far,
   within the grace period, and exits 78 when a `*config.Error` is in `Init`'s
   error chain, or 1 otherwise.
6. Calls `Serve` on every Service concurrently, and logs `serving`.
7. Waits for a signal, or for the first Service to return.
8. Stops listening for signals, so that a second interrupt or SIGTERM takes
   its default action and ends the process at once, cancels the context
   passed to `Serve`, and logs `shutting down`.
9. When it hasn't yet received every `Serve`'s result, calls the
   [drain](#the-drain), with the `Setup` that `Init` was given:
   `humus.DrainDelay` of `HUMUS_SHUTDOWN_DRAIN_DELAY` when that is set, and
   `App.Drain` otherwise. Its context keeps the values of Run's context but
   is not cancelled with it, is done once every `Serve` has returned, and
   gives `humus.ShutdownCause` why the shutdown started. Run collects the
   `Serve` results that arrive in the meantime and waits for the drain to
   return. A drain error is logged at ERROR and joined into the cause as
   `drain: ` and the error.
10. Calls `Shutdown` on every Service concurrently, waits for every `Serve` to
    return, and then runs the OnStop functions in reverse order of
    registration. All of this happens within one grace period,
    `HUMUS_SHUTDOWN_GRACE_PERIOD`, which starts after the drain. Then it
    logs `stopped`.
11. Shuts the telemetry down.
12. Returns the [exit status](#exit-codes).

An `Init` that returns no Services, and a Service that returns on its own
when it is the only one, end the App without calling the drain, as there is
nothing to drain. A cancelled root context still gets a
graceful shutdown: the grace period keeps the context's values but not its
cancellation. Every failure is the cause of the exit status. Once the
telemetry is built, each failure is logged through it, one record per error;
before that, it is written to standard error, one line per error. The
lifecycle never recovers a panic.

## Telemetry

`App.Telemetry` is the App's
[`telemetry.Builder`](https://pkg.go.dev/github.com/z5labs/humus/telemetry#Builder),
and it is required. `Main`'s [default](#the-defaults) calls `telemetry.New`
with `telemetry.DefaultSettings()`, so log records go to standard output as
JSON lines and nothing is exported. A service that exports anything passes
its own `Builder` with `humus.WithTelemetry`, such as `buildTelemetry`
[above](#main), which sources
[`telemetry.Settings`](https://pkg.go.dev/github.com/z5labs/humus/telemetry#Settings)
however it likes and calls `telemetry.New`. A test run with `humustest.Start`
keeps `Main`'s default unless it passes `telemetry.NoopBuilder`, or the
`Builder` of humustest's recorder, `humustest.Telemetry`, when the test
asserts on what the App emits.
[Telemetry](/humus/pr-preview/pr-920/telemetry/) explains how to write the `Builder`,
what the defaults are, the console log format, and how logs get their trace
context.

**Telemetry is built after `NotifyContext` and before `Init`.** `Run` calls
`App.Telemetry` with the context that a signal cancels, the `Process`, and the
Host's `Dial`, so the OTLP exporters reach the collector through the `Host`. A
`Builder` that runs a resource detector can be slow, because a cloud detector
calls its metadata server, so a signal cancels it as it cancels a slow
`Init`. A failure exits 78 when a `*config.Error` is in its chain, such as a
malformed variable the `Builder` read, and 1 otherwise. A `Builder` that
returns an SDK with a nil field fails too, and exits 1. In every case `Init`
is never called. `Init` gets the SDK in `Setup.Telemetry`, and hands it on to
its Services.

**Failures are logged through telemetry.** The lifecycle logs with the
OpenTelemetry Logs API, through
`Setup.Telemetry.LoggerProvider.Logger("github.com/z5labs/humus")`: its own
records, `serving`, `shutting down` and `stopped`, at INFO, and every failure
after the telemetry is built at ERROR, one record per error, so an operator
sees them in the same log stream as everything else. humus has no logging
bridge of its own. Standard error still gets what telemetry cannot report
itself:

- a failure to build telemetry, and a malformed lifecycle setting, which come
  before there is any telemetry;
- export errors, each on one line starting with `telemetry: `. `Run` sets
  OpenTelemetry's process-wide error handler to write them there, because a
  failing exporter cannot report its own failure;
- a failure to shut telemetry down.

**Telemetry shuts down last:** after the OnStop functions, or after a failed
`Init` and its OnStop functions. So the spans and logs that record a slow or
failed shutdown are still exported. Its context keeps the root context's
values but not its cancellation, and it is separate from the grace period:
`telemetry.New` bounds it with `Settings.ShutdownTimeout`. A failure to shut
telemetry down is joined into the cause of the exit status, and it turns an
exit 0 into 1.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `HUMUS_SHUTDOWN_GRACE_PERIOD` | `App.ShutdownGracePeriod`: `Main`'s 20s, unless an option sets another | bounds the whole shutdown: every Service's `Shutdown` and every OnStop function share it. A [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) string, such as `30s`, that must be positive. |
| `HUMUS_SHUTDOWN_DRAIN_DELAY` | `App.Drain`: none under `Main`, unless an option sets one | the drain, in place of `App.Drain`, whatever that is, including none: `humus.DrainDelay` of a `time.ParseDuration` string, such as `5s`, that must not be negative; `0s` means no pause. |

These are the only variables the lifecycle reads, and the root package is the
one place humus reads them. Each value is checked after its default, so a bad
default fails the same way as a bad variable: an App whose
`ShutdownGracePeriod` is zero, run without `HUMUS_SHUTDOWN_GRACE_PERIOD`,
exits 78 with `config HUMUS_SHUTDOWN_GRACE_PERIOD: must be positive`. When both
values are bad, both are reported, one line each.

## Exit codes

| Code | When | Cause |
|---|---|---|
| 0 | a signal stopped the App and it shut down in time | the signal |
| 0 | a Service returned `nil` on its own, and the App shut down in time | none |
| 1 (`humus.ExitFailure`) | the telemetry could not be built, or its SDK has a nil field | the error |
| 1 (`humus.ExitFailure`) | `Init`, a Service, a Service's `Shutdown`, an OnStop function or the telemetry's shutdown failed | every error, joined |
| 1 (`humus.ExitFailure`) | the shutdown ran past the grace period | a `*humus.GracePeriodError` |
| 2 | never set by humus: Go's exit status for an unrecovered panic | |
| 78 (`humus.ExitConfig`) | a setting is missing or malformed, including a config value read by the telemetry `Builder` or by `Init` | a `*config.Error` in the chain |

78 is `EX_CONFIG` from `sysexits.h`. It tells an operator "fix the
configuration" apart from "the service failed". Everything else that fails
exits 1, and a test tells those failures apart by their cause:
`errors.AsType[*config.Error]` for a config error, and
`errors.AsType[*humus.GracePeriodError]` for a shutdown that ran out of time.
A flag usage error is an `Init` failure and exits 1.

## Testing a whole App

An App runs whole inside a test.
[`humustest.Start`](https://pkg.go.dev/github.com/z5labs/humus/humustest#Start)
runs the App that `Main` would run for an `Init`, with `Main`'s defaults, on a
test Host in place of `humus.OSHost()` and on a fake process from
[bedrocktest](https://pkg.go.dev/github.com/z5labs/bedrock/bedrocktest),
whose arguments and environment the test sets, whose output the test reads,
and to which the test sends fake signals. Options given with
`humustest.WithApp` change the App as they would in `main`.
[`testing/synctest`](https://pkg.go.dev/testing/synctest) runs it in fake
time, so a five-second drain passes at once and is measured exactly.
The lifecycle's own tests run it with fake Services:

```go
type fakeService struct {
	serve    func(ctx context.Context) error
	shutdown func(ctx context.Context) error

	mu            sync.Mutex
	shutdownCalls int
}

func (s *fakeService) Serve(ctx context.Context) error {
	if s.serve == nil {
		<-ctx.Done()
		return nil
	}
	return s.serve(ctx)
}

func (s *fakeService) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shutdownCalls++
	s.mu.Unlock()
	if s.shutdown == nil {
		return nil
	}
	return s.shutdown(ctx)
}
```

This test runs an App with an HTTP-like Service and a consumer through a
whole lifecycle, from SIGTERM to the OnStop functions, with a 5s drain:

```go
func TestStart_a_whole_App(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// api keeps serving until Shutdown, as an HTTP server does.
		stopped := make(chan struct{})
		var shutdownAt time.Time
		api := &fakeService{
			serve: func(context.Context) error {
				<-stopped
				return nil
			},
			shutdown: func(context.Context) error {
				shutdownAt = time.Now()
				close(stopped)
				return nil
			},
		}

		// consumer stops as soon as Serve's context is done, as a queue
		// consumer does.
		var consumerStoppedAt time.Time
		consumer := &fakeService{serve: func(ctx context.Context) error {
			<-ctx.Done()
			consumerStoppedAt = time.Now()
			return nil
		}}

		var poolClosedAt time.Time
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			s.OnStop(func(context.Context) error {
				poolClosedAt = time.Now()
				return nil
			})
			return []humus.Service{api, consumer}, nil
		}

		p, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithDrain(humus.DrainDelay(5*time.Second))),
		)
		<-p.Notified(syscall.SIGTERM)
		synctest.Wait() // every Serve has started
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if !errors.Is(st.Cause(), context.Canceled) {
			t.Errorf("cause = %v, want the signal", st.Cause())
		}
		if got := consumerStoppedAt.Sub(sigterm); got != 0 {
			t.Errorf("consumer stopped %s after SIGTERM, want at once", got)
		}
		if got := shutdownAt.Sub(sigterm); got != 5*time.Second {
			t.Errorf("Shutdown called %s after SIGTERM, want after the 5s drain", got)
		}
		if api.calls() != 1 || consumer.calls() != 1 {
			t.Errorf("Shutdown called %d and %d times, want once each", api.calls(), consumer.calls())
		}
		if poolClosedAt.IsZero() || poolClosedAt.Before(shutdownAt) {
			t.Errorf("OnStop ran at %v, want it after Shutdown at %v", poolClosedAt, shutdownAt)
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
}
```

`synctest` reports a deadlock when a goroutine the App started outlives the
test body, which makes it a leak detector for Services. For an HTTP Service
the cost is that handlers must honour `r.Context()`, because a handler that
ignores it after the server force-closes the connection outlives the test.

[Testing a humus App](/humus/pr-preview/pr-920/testing/) covers `humustest.Start` in
full, and runs Apps that serve real HTTP, on a humustest Host whose network
and filesystem the test controls.

## Ambient leaks

Some dependencies read the real process behind an App's back, so they reach
around the `bedrock.Process` and the `Host` an App is given, and a test can
neither control nor see what they read. Avoid them, or configure them
explicitly from `Setup`:

- **pgx** reads the real process. `pgx.ParseConfig` and
  `pgxpool.ParseConfig` take every setting the connection string leaves out
  from `PGHOST`, `PGUSER`, `PGPASSWORD` and the other `PG*` variables of the
  real environment, and default the user to the OS user. They read the
  password file that `PGPASSFILE` names, or `~/.pgpass`, from the real disk
  and use it when the connection string sets no password, and they pick up
  client certificates from `~/.postgresql`. The config they return dials the
  real network: set `cfg.ConnConfig.DialFunc = s.Host.Dial` before opening the
  pool. Its `LookupFunc` still resolves host names with the real resolver.
- `flag.CommandLine` and `flag.ExitOnError` read `os.Args` and exit the
  process. Parse `p.Args[1:]` with a `flag.FlagSet` of your own, using
  `flag.ContinueOnError`.
- An `exec.Cmd` with a nil `Env` inherits the real environment. Build its
  `Env` from the keys you know, reading each through `p.Environment`.
- `http.ProxyFromEnvironment`, the `Proxy` of `http.DefaultTransport`, reads
  the proxy variables of the real environment, and `http.DefaultClient`
  dials the real network rather than through `Host.Dial`.
- **`crypto/x509`'s system roots.** A TLS config with no `RootCAs` verifies
  the server against the system's roots, which `crypto/x509` reads from the
  files and directories `SSL_CERT_FILE` and `SSL_CERT_DIR` name in the real
  environment, and from the OS's certificate files on the real disk, not
  through the Host. An `httpclient` client, or a `grpcclient` Client, whose
  `Settings.TLS` has no `RootCAs` reads them. A service that calls only its
  own services sets `RootCAs` to its CA, and a test sets it to the pool of a
  CA from `humustest.NewCA`, as
  [Calling HTTP services](/humus/pr-preview/pr-920/http-client/#tls-and-plain-text)
  and [Calling gRPC services](/humus/pr-preview/pr-920/grpc-client/#tls-and-plain-text)
  describe.
- `slog.Default` writes to the real standard error unless it is replaced, and
  replacing it is process-wide state.
- OpenTelemetry's process-wide error handler, which `Run` sets. In a process
  with several Apps, the last App to start gets every App's export errors, so
  a test that asserts on export errors does not run in parallel.
- OpenTelemetry's internal logger, which writes its own errors to the real
  standard error.
- `OTEL_GO_X_*`, OpenTelemetry's experimental features, which the SDK reads
  from the real environment.
- Any resource detector the end user opts into in `Settings.ResourceOptions`,
  which reads the real host, such as `/etc`, `/proc` and cgroups, or dials a
  metadata server rather than through `Host.Dial`.
- **grpc-go's logger.** Its `grpclog` package reads
  `GRPC_GO_LOG_SEVERITY_LEVEL`, `GRPC_GO_LOG_VERBOSITY_LEVEL` and
  `GRPC_GO_LOG_FORMATTER`, and its `internal/envconfig` package reads its
  other `GRPC_*` variables, from the real environment, and it writes to the
  real standard error. Replacing it means `grpclog.SetLoggerV2`, which is
  process-wide state, so `grpcserver` sets none: an end user who wants
  grpc-go's logs in their telemetry sets the logger in `main`, as
  [gRPC services](/humus/pr-preview/pr-920/grpc/#grpc-gos-logger) describes.
- **amqp091's logger.** amqp091-go, RabbitMQ's Go client, logs through
  `amqp.Logger`, a package-level variable that `amqp.SetLogger` replaces,
  and that logs nothing until it is. Replacing it is process-wide state,
  shared by every App and every test in the process, and it writes wherever
  the logger it is given writes, not through the Setup's `LoggerProvider`.
  So `queue/rabbitmq` never sets it, as
  [Consuming and publishing with RabbitMQ](/humus/pr-preview/pr-920/rabbitmq/#amqp091s-logger)
  describes, and an end user who wants its messages sets it once, in
  `main`.

`flag`, `exec.Cmd`, `http.ProxyFromEnvironment` and `slog.Default` are on
[bedrock's own list](https://pkg.go.dev/github.com/z5labs/bedrock#hdr-Ambient_leaks).
A dependency found reading the real process belongs on this list.
