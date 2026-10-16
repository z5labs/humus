---
title: The App lifecycle
linkTitle: App lifecycle
description: >
  How humus.App reads its settings, builds its telemetry and its
  dependencies, runs its Services, shuts them down within a grace period and
  turns the outcome into an exit status.
type: docs
weight: 10
---

Every humus application is a [`bedrock.App`](https://pkg.go.dev/github.com/z5labs/bedrock#App),
and the root package's [`humus.App`](https://pkg.go.dev/github.com/z5labs/humus#App)
is the lifecycle every humus app type is built on. It reads its settings,
listens for signals, builds its telemetry and the user's dependencies, runs
one or more Services, shuts them down within a grace period, releases what was
built, shuts its telemetry down and turns the outcome into an exit status.

Most applications will not use `humus.App` directly. Each runtime package —
REST, gRPC, jobs, queues — provides an App of its own, built on `humus.App`
with that package's defaults, and that is the API most users touch. Using
`humus.App` directly, for example to run a REST API and a gRPC service in one
process, is the advanced path. Read this page before taking it; read it, too,
to understand what the App a runtime package builds for you does.

Telemetry is part of the lifecycle: the App builds it before `Init`, logs
through it, and shuts it down last. See [Telemetry](#telemetry) below, and
the [Telemetry]({{< relref "telemetry" >}}) page for what a service does with
it. Health is not
part of the lifecycle yet. It is added by
[Proposal: health monitors and probes](https://github.com/z5labs/humus/discussions/634),
which integrates it with the lifecycle when it lands.

The design behind everything on this page is in
[Proposal: the humus App lifecycle](https://github.com/z5labs/humus/discussions/630#discussioncomment-18964173),
and the telemetry integration is in
[Proposal: OpenTelemetry without globals, Revision 2](https://github.com/z5labs/humus/discussions/632#discussioncomment-18966249).

## App

An App has five fields:

| Field | Required | Meaning |
|---|---|---|
| `Host` | yes | the network and filesystem the App runs on: `humus.OSHost()` in `main`, and a Host the test controls in a test |
| `ShutdownGracePeriod` | yes, unless `HUMUS_SHUTDOWN_GRACE_PERIOD` is set | the grace period when `HUMUS_SHUTDOWN_GRACE_PERIOD` is not set. It must be positive, so the zero value fails startup |
| `DrainDelay` | no | the drain delay when `HUMUS_SHUTDOWN_DRAIN_DELAY` is not set: the pause between shutdown starting and `Shutdown` being called on the Services. It must not be negative; zero means no pause |
| `Telemetry` | yes | the [`telemetry.Builder`](https://pkg.go.dev/github.com/z5labs/humus/telemetry#Builder) that builds the App's telemetry: the end user's own `Builder` in `main`, and `telemetry.NoopBuilder` or the `Builder` of humustest's recorder, `humustest.Telemetry`, in tests |
| `Init` | yes | builds the App's dependencies and returns the Services to run |

Its `main` is one call to `bedrock.Main`, the only place a humus program
touches the real process:

```go
bedrock.Main(humus.App{
	Host:                humus.OSHost(),
	ShutdownGracePeriod: 30 * time.Second, // when HUMUS_SHUTDOWN_GRACE_PERIOD is not set
	DrainDelay:          5 * time.Second,  // when HUMUS_SHUTDOWN_DRAIN_DELAY is not set
	Telemetry:           buildTelemetry,
	Init:                Init,
})
```

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

`ShutdownGracePeriod` and `DrainDelay` are defaults: an operator overrides
either through its [variable](#settings) without a rebuild. humus has no
default grace period of its own, so an App sets `ShutdownGracePeriod` unless
its environment always sets the variable; a `DrainDelay` left at zero means no
pause. How long to pause and how long to allow for shutdown depend on the kind
of App — a REST or gRPC App needs time for a load balancer to stop sending it
connections, and a job does not — so each runtime package's App sets its own.

## Service

A [`Service`](https://pkg.go.dev/github.com/z5labs/humus#Service) is a
long-running part of an App: an HTTP server, a gRPC server, a queue consumer
or a job. It has two methods, `Serve` and `Shutdown`, and the App gives it two
shutdown signals, at different times:

1. **`Serve`'s context is done** as soon as the App starts shutting down,
   before the drain delay.
2. **`Shutdown` is called** after the drain delay, with the grace period as
   its context's deadline.

A Service may stop on the first, as a job or a queue consumer does. Or it may
keep serving until the second, as an HTTP, gRPC or health Service does, so
that a load balancer still draining connections towards it is answered. This
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

## Host

A [`Host`](https://pkg.go.dev/github.com/z5labs/humus#Host) is what humus
hands an App beyond its process: `Listen`, `Dial` and a filesystem, `FS`. Every
field is required. In `main` an App runs on `humus.OSHost()`, which is
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
   environment, defaulting each to its App field. A malformed one, or a bad
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
9. Waits the drain delay: `HUMUS_SHUTDOWN_DRAIN_DELAY`, defaulting to
   `App.DrainDelay`.
10. Calls `Shutdown` on every Service concurrently, waits for every `Serve` to
    return, and then runs the OnStop functions in reverse order of
    registration. All of this happens within one grace period,
    `HUMUS_SHUTDOWN_GRACE_PERIOD`, which starts after the drain delay. Then it
    logs `stopped`.
11. Shuts the telemetry down.
12. Returns the [exit status](#exit-codes).

An `Init` that returns no Services ends the App at once, without a drain
delay, as there is nothing to drain. A cancelled root context still gets a
graceful shutdown: the grace period keeps the context's values but not its
cancellation. Every failure is the cause of the exit status. Once the
telemetry is built, each failure is logged through it, one record per error;
before that, it is written to standard error, one line per error. The
lifecycle never recovers a panic.

## Telemetry

`App.Telemetry` is the App's
[`telemetry.Builder`](https://pkg.go.dev/github.com/z5labs/humus/telemetry#Builder),
and it is required. In `main` it is the end user's own `Builder`, such as
`buildTelemetry` [above](#app), which sources
[`telemetry.Settings`](https://pkg.go.dev/github.com/z5labs/humus/telemetry#Settings)
however it likes and calls `telemetry.New`. In a test it is
`telemetry.NoopBuilder`, or the `Builder` of humustest's recorder,
`humustest.Telemetry`, when the test asserts on what the App emits.
[Telemetry]({{< relref "telemetry" >}}) explains how to write the `Builder`,
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
| `HUMUS_SHUTDOWN_GRACE_PERIOD` | `App.ShutdownGracePeriod` | bounds the whole shutdown: every Service's `Shutdown` and every OnStop function share it. A [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) string, such as `30s`, that must be positive. |
| `HUMUS_SHUTDOWN_DRAIN_DELAY` | `App.DrainDelay` | the pause between shutdown starting and `Shutdown` being called on the Services. A `time.ParseDuration` string, such as `5s`, that must not be negative; `0s` means no pause. |

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

An App runs whole inside a test. [bedrocktest](https://pkg.go.dev/github.com/z5labs/bedrock/bedrocktest)
hands it a fake process, whose arguments and environment the test sets, whose
output the test reads, and to which the test sends fake signals; and
[`testing/synctest`](https://pkg.go.dev/testing/synctest) runs it in fake
time, so a five-second drain delay passes at once and is measured exactly.
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
whole lifecycle, from SIGTERM to the OnStop functions:

```go
func TestApp_Run_a_whole_App(t *testing.T) {
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
		app := humus.App{
			Host:                humus.OSHost(), // the fake Services never use it
			ShutdownGracePeriod: 10 * time.Second,
			DrainDelay:          5 * time.Second,
			Telemetry:           telemetry.NoopBuilder,
			Init: func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				s.OnStop(func(context.Context) error {
					poolClosedAt = time.Now()
					return nil
				})
				return []humus.Service{api, consumer}, nil
			},
		}

		p := bedrocktest.NewProcess(t)
		status := make(chan bedrock.ExitStatus, 1)
		go func() { status <- app.Run(t.Context(), p.Process()) }()

		<-p.Notified(syscall.SIGTERM)
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
			t.Errorf("Shutdown called %s after SIGTERM, want after the 5s drain delay", got)
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

[Testing a humus App]({{< relref "testing" >}}) runs Apps that serve real
HTTP, on a humustest Host whose network and filesystem the test controls.

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

`flag`, `exec.Cmd`, `http.ProxyFromEnvironment` and `slog.Default` are on
[bedrock's own list](https://pkg.go.dev/github.com/z5labs/bedrock#hdr-Ambient_leaks).
A dependency found reading the real process belongs on this list.
