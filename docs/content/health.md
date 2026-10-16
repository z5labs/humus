---
title: Health
description: >
  How a humus service reports its health: monitors and how they combine,
  what liveness and readiness each check, and the health Service that owns an
  App's readiness through the lifecycle.
type: docs
weight: 12
---

A service reports its health as [monitors](#monitors) it combines into two
[probes](#liveness-and-readiness), liveness and readiness, and an App's
readiness is owned by a [health Service](#the-health-service) it returns
from `Init` like any other Service. All of it is in the
[`health`](https://pkg.go.dev/github.com/z5labs/humus/health) package, which
depends on nothing but the standard library.

The design behind this page is in
[Proposal: health monitors and probes, Revision 2](https://github.com/z5labs/humus/discussions/634#discussioncomment-18976159).

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
the pod out of the load balancer until the database is back.

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
is a [Service]({{< relref "lifecycle#service" >}}) that owns an App's
readiness. The end user's `Init` builds one with
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

The Service's own readiness follows the [lifecycle]({{< relref "lifecycle#what-run-does-in-order" >}}):

1. **Not ready until it is serving.** It is not ready from `NewService`
   until the App calls its `Serve`.
2. **Ready while it serves**, as long as its readiness monitors are healthy.
3. **Not ready from the moment shutdown starts, through the drain delay.**
   The App cancels `Serve`'s context as soon as it starts shutting down,
   before the drain delay, and the Service turns not ready then, for good. A
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

## Jobs

A job needs no health Service. `humus.App` runs only the Services `Init`
returns, and nothing in the lifecycle asks for readiness, so a job's `Init`
returns the job and nothing else. A job runs to completion rather than
serving traffic, so there is nothing for a load balancer to route to it.

## Exposing the probes

The health Service serves no protocol: nothing in the `health` package opens
a port or answers an orchestrator's probes. Its `Probes` are monitors, and
whatever exposes them calls `Healthy` on them. Serving them over HTTP and
gRPC is planned in wave 3 of
[the charter](https://github.com/z5labs/humus/discussions/629#discussioncomment-18979083),
and isn't available yet.

## Testing

A health Service is tested in a whole App, with
[bedrocktest](https://pkg.go.dev/github.com/z5labs/bedrock/bedrocktest)'s
fake process and a [humustest]({{< relref "testing" >}}) Host, under
[`testing/synctest`](https://pkg.go.dev/testing/synctest), so the drain delay
passes in fake time and readiness can be checked while it runs. This is
`health`'s own test that the Service is not ready during the drain delay:

```go
func TestServiceIsNotReadyDuringTheDrainDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hs := health.NewService()
		readiness := hs.Probes().Readiness
		app := humus.App{
			Host:                humustest.NewHost(t).Host(),
			Telemetry:           telemetry.NoopBuilder,
			ShutdownGracePeriod: 10 * time.Second,
			DrainDelay:          5 * time.Second,
			Init: func(context.Context, *humus.Setup) ([]humus.Service, error) {
				return []humus.Service{hs}, nil
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

		p.Signal(syscall.SIGTERM)
		synctest.Wait() // shutdown has started; the drain delay is running
		ready, err = readiness.Healthy(t.Context())
		require.NoError(t, err)
		require.False(t, ready)

		require.Zero(t, (<-status).Code())
	})
}
```

`<-p.Notified(syscall.SIGTERM)` waits until the App listens for signals, and
`synctest.Wait` until every goroutine in the test, `Serve` included, is
blocked, so the first check sees the Service serving. After the SIGTERM,
`synctest.Wait` returns while the App waits out its five-second drain delay,
and readiness is already false. A test of an App's own readiness monitors
works the same way: take the probes from the Service, run the App, and check
them at each point of the lifecycle.
