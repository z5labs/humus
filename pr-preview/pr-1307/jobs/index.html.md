# Jobs

A job is a process that does one piece of work and exits. The
[`job`](https://pkg.go.dev/github.com/z5labs/humus/job) package turns that
work, the run, into a `humus.Service` that runs it once, stops it on either
shutdown signal and records one span around it. The job runs on
`humus.Main`'s defaults, and the run's error is the exit status, so
Kubernetes retries a job that failed or was interrupted and completes one
that finished.

The package is `job.NewService` and nothing else: no App, no `Main`, no
`Settings`. The design behind everything on this page is in
[Proposal: jobs](https://github.com/z5labs/humus/discussions/682) and
[its revision](https://github.com/z5labs/humus/discussions/682#discussioncomment-18988201).

## The run

A job's work is its run: a plain `func(ctx context.Context) error`. A type
with a `Run` method passes its method value, `e.Run`, so the package has no
interface to implement and no adapter to wrap it in. This run exports to a
bucket, and returns early with its context's error when the context is done:

```go
// exporter is the end user's job. Its Run method is the run.
type exporter struct {
	bucket string
	out    io.Writer
}

// Run does the job's work once, and returns early with ctx's error when ctx
// is done, so that an interrupted run exits 1.
func (e *exporter) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(e.out, "exported to", e.bucket)
	return err
}
```

The end user's `Init` builds the run's dependencies, releases them with
`s.OnStop`, and returns `job.NewService(s, run)` to `humus.Main`. Here the
dependency is a buffered writer, flushed once the job has stopped:

```go
// Init is the end user's Init: it builds the run's dependencies, releases
// them with s.OnStop, and returns the job's Service.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	bucket, err := config.Env(s.Process.Environment, "EXPORT_BUCKET").Read(ctx)
	if err != nil {
		return nil, err // exit 78
	}
	out := bufio.NewWriter(s.Process.Stdout)
	s.OnStop(func(context.Context) error { return out.Flush() })

	e := &exporter{bucket: bucket, out: out}
	return []humus.Service{job.NewService(s, e.Run)}, nil
}
```

The job's `main` is one call to `humus.Main`, here with a telemetry
`Builder` that names the service:

```go
humus.Main(Init, humus.WithTelemetry(buildTelemetry))
```

The Service's `Serve` calls the run once, on `Serve`'s own goroutine, and
returns when the run does. A Service returning on its own ends the App, so
the job finishing ends the process. A `Serve` called a second time, or after
`Shutdown`, returns `nil` without running the job again.

## The defaults

A job runs on `humus.Main`'s [defaults](/humus/pr-preview/pr-1307/lifecycle/#the-defaults),
which are a job's, so `job` has no `Main` and no defaults of its own:

- **A grace period of 20s.** It fits inside Kubernetes' default
  `terminationGracePeriodSeconds` of 30s, with room for the telemetry's 5s
  shutdown after it.
- **No drain.** Nothing routes traffic to a job, so there is nothing to wait
  for: after a signal, `Shutdown` is called on the job straight away.

An operator changes the grace period with `HUMUS_SHUTDOWN_GRACE_PERIOD`, such
as `HUMUS_SHUTDOWN_GRACE_PERIOD=45s`, without a rebuild, and
`HUMUS_SHUTDOWN_DRAIN_DELAY` adds a drain, as for any App. A `main` that wants
a different default in code passes `humus.WithShutdownGracePeriod` to
`humus.Main`; the variable still wins over it.
[The lifecycle's settings](/humus/pr-preview/pr-1307/lifecycle/#settings) describes
both variables.

## Exit statuses

`job` adds no exit codes. The [lifecycle's](/humus/pr-preview/pr-1307/lifecycle/#exit-codes)
apply, and the run decides which:

| The run | Exit | Cause |
|---|---|---|
| returns `nil` | 0 | none |
| returns an error | 1 | `serve: ` and the run's error |
| is interrupted by a signal, and returns ctx's error | 1 | `serve: context canceled`, or the signal when the run returns `context.Cause(ctx)` |
| is interrupted by a signal, but returns `nil` | 0 | the signal |
| ignores its context past the grace period | 1 | a `*humus.GracePeriodError` |
| panics | 2 | none: Go's runtime prints the panic and exits |
| `Init` fails reading a setting | 78 | the `*config.Error` |

**A run that a signal interrupts exits 1 unless it returns `nil`.** So
Kubernetes counts the pod as failed and retries the unfinished work, rather
than completing the Job. A run that stops early because its context is done
returns `ctx.Err()`, as almost all Go code already does, and as `Run` above
does: the cause is then `serve: context canceled`. A run that wants the
signal in its cause returns `context.Cause(ctx)` instead, and the cause is
then, for example, `serve: terminated signal received`.

The fourth row is the run's own decision. A run that returns `nil` after a
signal has either finished its work or chosen to treat stopping early as
done, and the Job completes.

## Kubernetes

A Kubernetes [Job](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
reads the exit code of its pod's container, and the exit statuses above are
chosen for what it does with each.

### `backoffLimit` and `restartPolicy`

**Exit 0 completes the Job.** The container terminated successfully, so the
pod is `Succeeded` and counts towards the Job's `completions`, one by
default.

**Every other code is a failure,** 1, 2 and 78 alike. The Job retries it
until it has failed `backoffLimit` times, 6 by default, with an exponential
back-off between attempts, and then marks the Job failed. How it retries
depends on the pod template's `restartPolicy`, which must be `OnFailure` or
`Never` for a Job:

- **`OnFailure`:** the kubelet restarts the container in the same pod, so
  the job's next attempt reuses the pod and its volumes.
- **`Never`:** the pod stays `Failed`, and the Job starts a new pod for the
  next attempt. A pod failure policy, below, needs `Never`.

So with the defaults alone, a job whose configuration is wrong is retried
six times, and a job that an eviction interrupted counts that interruption
against the same limit as a real failure.

### A pod failure policy

A [`podFailurePolicy`](https://kubernetes.io/docs/concepts/workloads/controllers/job/#pod-failure-policy)
tells those cases apart. This one fails the Job at once on exit 78, since a
retry reads the same bad setting, and ignores disruptions, the evictions,
preemptions and deletions that Kubernetes marks with the `DisruptionTarget`
pod condition. An ignored failure is retried with a new pod and is not
counted against `backoffLimit`:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: exporter
spec:
  backoffLimit: 6
  podFailurePolicy:
    rules:
      - action: FailJob
        onExitCodes:
          containerName: exporter
          operator: In
          values: [78]
      - action: Ignore
        onPodConditions:
          - type: DisruptionTarget
  template:
    spec:
      restartPolicy: Never
      terminationGracePeriodSeconds: 30
      containers:
        - name: exporter
          image: registry.example.com/exporter:1.0.0
          env:
            - name: EXPORT_BUCKET
              value: nightly
```

The rules are checked in order, and the first that matches applies. A pod
whose run an eviction interrupted exits 1, which the first rule does not
match, and has the `DisruptionTarget` condition, which the second does. Any
other failure, such as a run that returned an error or panicked, matches
neither and counts against `backoffLimit` as before. That is why humus has
no exit code of its own for an interruption: the condition already tells an
interruption apart from a failure.

### `terminationGracePeriodSeconds`

When Kubernetes stops a pod, it sends the container SIGTERM, and then
SIGKILL once `terminationGracePeriodSeconds` have passed, 30 by default. A
job has to be gone by then. On `humus.Main`'s defaults, the worst case from
SIGTERM to the exit is the 20s grace period and the telemetry's shutdown,
`telemetry.Settings.ShutdownTimeout`, 5s: 25s, inside Kubernetes' 30s.

A job that raises `HUMUS_SHUTDOWN_GRACE_PERIOD` raises
`terminationGracePeriodSeconds` with it, to at least the grace period plus
the telemetry's shutdown plus a margin for the process to exit. Otherwise
SIGKILL ends the job first: its container exits 137, the process is killed
before it runs its OnStop functions, and the telemetry it had not yet
exported is lost.

## The two signals

The App gives a Service [two signals](/humus/pr-preview/pr-1307/lifecycle/#service) to
stop on, and the run stops on either:

- **`Serve`'s context.** The run's context is made from `Serve`'s, which is
  done as soon as the App starts shutting down. So a signal cancels the run
  at once, and the run has the grace period to stop.
- **`Shutdown`.** `Shutdown` cancels the run's context too, and waits for
  `Serve` to return. In an App, `Serve`'s context is always done first, so
  this matters only when a caller calls `Shutdown` first, as
  `humustest.TestService` does. `Serve` then returns `nil` for a run that
  returns a cancellation, because the Service contract says a Service that
  `Shutdown` stopped returns `nil`.

Which signal came first decides what `Serve` returns. After a signal, it
returns the run's error, unchanged, and the App exits 1. After a bare
`Shutdown`, it returns `nil` when the run's error is a cancellation, and
the run's error otherwise.

## Telemetry

### The `job` span

Each run has one span, named `job`, of kind internal, from the
`TracerProvider` of the `Setup`'s telemetry. When the run returns an error,
the span's status is `Error` with the error's text, including when `Serve`
reports a run that `Shutdown` stopped as `nil`: the span records what the run
returned.

The run's context carries the span. So every span the run starts from that
context is its child, and every record it logs with that context carries
its trace ID. This `Init` takes a tracer and a logger from `s.Telemetry`,
and its run uses both with the run's context:

```go
tracer := s.Telemetry.TracerProvider.Tracer("exporter")
logger := otelslog.NewLogger("exporter", otelslog.WithLoggerProvider(s.Telemetry.LoggerProvider))
return []humus.Service{job.NewService(s, func(ctx context.Context) error {
	_, span := tracer.Start(ctx, "export")
	defer span.End()
	logger.InfoContext(ctx, "exported")
	return nil
})}, nil
```

The `export` span is a child of `job`, and the `exported` record carries the
`job` span's trace ID. With `telemetry.DefaultSettings()` traces are not
exported, but spans still get real IDs, so one job's console logs share a
trace ID without a collector. `job` writes no log records and records no
metrics of its own: the lifecycle already logs `serving`, `shutting down`,
`stopped` and a failed `Serve`, and the orchestrator sees every run,
including the ones that never started.

### Joining the trace that started the job

A job is often started by a process that is itself traced, such as a
workflow engine or a CI step. OpenTelemetry's
[environment-variable carriers](https://opentelemetry.io/docs/specs/otel/context/env-carriers/)
have that process pass its trace context to the job in environment
variables, and the run's span joins it.

`Serve` extracts a trace context from the Process's environment with the
`Builder`'s propagator, `s.Telemetry.Propagator`, and the run's span is that
context's child, still of kind internal. Each of the propagator's keys is
read under the name the specification gives it: ASCII letters uppercased,
and every character but an ASCII letter, digit or `_` replaced with `_`. So:

- **With `telemetry.New`'s propagator,** W3C trace context and baggage, the
  variables are `TRACEPARENT`, `TRACESTATE` and `BAGGAGE`.
- **With a propagator of another kind,** from a `Builder` of your own, such
  as B3, they are that propagator's keys, read the same way: `x-b3-traceid`
  as `X_B3_TRACEID`.

humus defines none of these names: the propagator's keys and the
specification do. The extracted context is the parent of the run's span and
of nothing else, so the spans `Init` starts stay in traces of their own. Its
baggage is on the run's context, and the run reads it with
`baggage.FromContext(ctx)`:

```go
schedule = baggage.FromContext(ctx).Member("schedule").Value()
```

### A new trace when none is set

When none of the variables is set, or the trace context in them is
malformed, the run's span is the root of a new trace, with no error and
nothing logged, as for an HTTP request with a bad `traceparent` header.

**Kubernetes sets none of these variables.** A Job or a CronJob's run starts
a new trace on every run, unless the pod spec sets `TRACEPARENT` itself or
whatever creates the Job passes its own.

## Failure injection

A run's `BAGGAGE` is trusted: a failure scenario in it reaches the run's
context unstripped, and the points of the clients the run calls with that
context. Whoever starts the job, with its pod spec or otherwise, already
sets every one of its settings, so a scenario there is set by the same
hand. A job started by a service hands on that service's baggage, which
that service's own server has already stripped of any scenario from a
caller it doesn't trust. So `job` has no setting for failure injection.
[Failure injection](/humus/pr-preview/pr-1307/failure-injection/) explains the model,
and how a test sets a scenario in `BAGGAGE` with `humustest.WithEnv`.

## More than the job

A process that runs a job next to other Services, such as a probes server
or a metrics server, returns them all from the end user's `Init`, with the
job's among them. A Kubernetes Job's pod usually needs no probes, but a
long-running job that wants a liveness probe returns
[`httphealth.NewService`](https://pkg.go.dev/github.com/z5labs/humus/httphealth#NewService)'s
Service next to the job, which owns the App's readiness, so it needs no
health Service. Its liveness monitor, here `progress`, is the job's own: with
none, the probe checks only that the process answers:

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

Any Service returning ends the App, so the job finishing ends the process,
and the other Services with it. [Health](/humus/pr-preview/pr-1307/health/#jobs)
describes a job's probes.

Which `Main` the process uses depends on what a load balancer routes to, as
[the lifecycle's runtime packages](/humus/pr-preview/pr-1307/lifecycle/#runtime-packages)
explains for mixed processes:

- **Next to a probes or metrics server,** a job stays on `humus.Main`.
  Nothing routes to that server, and a drain would make a finished job wait
  for the whole delay.
- **Next to a server a load balancer routes to,** such as an API, the
  process uses that server's package's `Main`. After a signal, the run then
  gets that package's drain and grace period rather than `humus.Main`'s.

## What `job` leaves to you

- **Retries inside a run.** Kubernetes retries a failed pod under
  `backoffLimit`. A run that wants to retry one step in the process, such as
  a call to a flaky API, uses whatever it likes inside the run, such as
  bedrock's `retry` module. `job` doesn't wrap it, so the retries stay
  visible in your code.
- **Idempotency across Kubernetes' retries.** An interrupted or failed run
  is run again from the start, in a new process, so a run that half
  finished must be safe to run again: write with upserts, skip what is
  already done, or record progress somewhere the next attempt reads it.
- **Panics.** `job` recovers no panic. A run that panics crashes the process
  with Go's exit status 2 and the stack trace on stderr, which Kubernetes
  counts as a failure and retries under `backoffLimit`. A run that wants a
  panic reported differently recovers it itself.

The [billing job walkthrough](/humus/pr-preview/pr-1307/walkthroughs/job/billing/)
builds a job that handles the first two: `httpclient` retries each charge
inside the run, and each invoice's ID is its charge's idempotency key, so a
run that Kubernetes retries charges nothing twice.

## Testing

A job is tested whole, with
[`humustest.Start`](https://pkg.go.dev/github.com/z5labs/humus/humustest#Start),
inside `testing/synctest`, as [Testing a humus App](/humus/pr-preview/pr-1307/testing/)
describes for any App. The tests below use a helper, `runs`, which returns
an `Init` whose only Service is a job that runs the given run:

```go
// runs returns an Init whose only Service is a job that runs run.
func runs(run func(context.Context) error) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		return []humus.Service{job.NewService(s, run)}, nil
	}
}
```

### A whole job, and SIGTERM

`humustest.Start` returns the test's process, and `p.Signal` sends it a
signal, as Kubernetes sends SIGTERM. `synctest.Wait` lets the run block on
its context first, and fake time means the grace period costs the test
nothing:

```go
func TestService_SIGTERM_interrupts_the_run_and_exits_1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, runs(func(ctx context.Context) error {
			<-ctx.Done()
			return context.Cause(ctx)
		}), humustest.WithApp(humus.WithTelemetry(tel.Builder())))
		synctest.Wait() // the run is blocked on its context
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorContains(t, st.Cause(), "serve: terminated signal received")
		spans := tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, codes.Error, spans[0].Status().Code)
		require.Equal(t, "terminated signal received", spans[0].Status().Description)
	})
}
```

The same test runs the end user's own `Init` in place of `runs`, with the
variables it reads set in `humustest.WithEnv`, such as `EXPORT_BUCKET` for
the `Init` above.

### Setting `TRACEPARENT`

The run reads its trace context through the Process's environment, so a
test sets `TRACEPARENT`, `TRACESTATE` and `BAGGAGE` with `humustest.WithEnv`,
and reads the span's trace ID and parent from the recorder:

```go
_, status := humustest.Start(t, runs(func(ctx context.Context) error {
	schedule = baggage.FromContext(ctx).Member("schedule").Value()
	return nil
}),
	humustest.WithApp(humus.WithTelemetry(tel.Builder())),
	humustest.WithEnv(map[string]string{
		"TRACEPARENT": traceparent,
		"BAGGAGE":     "schedule=nightly",
	}),
)

require.Zero(t, (<-status).Code())
spans := tel.Spans()
require.Len(t, spans, 1)
span := spans[0]
require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", span.SpanContext().TraceID().String())
require.Equal(t, "00f067aa0ba902b7", span.Parent().SpanID().String())
require.True(t, span.Parent().IsRemote())
require.Equal(t, trace.SpanKindInternal, span.SpanKind())
require.Equal(t, "nightly", schedule)
```

### The Service contract

`job`'s own tests hold `job.Service` to the Service contract with
[`humustest.TestService`](/humus/pr-preview/pr-1307/testing/#checking-a-service), and a
Service of your own that wraps a job is checked the same way. `TestService`
needs a Service that runs until it is stopped, so the run blocks on its
context:

```go
func TestService_meets_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		return job.NewService(s, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
	})
}
```

That exercises both stop paths: `TestService`'s rule that `Serve` returns
`nil` after `Shutdown` relies on a bare `Shutdown` being reported as `nil`,
and its rule for `Shutdown` after `Serve` has returned on its own cancels
`Serve`'s context first.
