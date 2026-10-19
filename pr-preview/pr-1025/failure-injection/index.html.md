# Failure injection

Failure injection lets one request fail on purpose, in production or in a
test, without failing anyone else's. A request carries a failure scenario in
its OpenTelemetry baggage, and every injection point the request passes
through, such as a call to a dependency, looks itself up in the scenario and
learns whether to fail or slow down. The end user then sees how the whole
service copes with a dependency that is down or slow: its retries, its
breakers and its fallbacks.

The [`fault`](https://pkg.go.dev/github.com/z5labs/humus/fault) package
builds an App's failure injection, and
[`fault/jsonscenario`](https://pkg.go.dev/github.com/z5labs/humus/fault/jsonscenario)
is a ready-made codec for its scenarios. Both are built on bedrock's
[`fit`](https://pkg.go.dev/github.com/z5labs/bedrock/fit). Every humus client
is an injection point, and every humus server strips scenarios from callers
it doesn't trust. A [Kubernetes controller](/humus/pr-preview/pr-1025/kubernetes/#failure-injection)
answers no request, so an annotation of the object it reconciles carries a
scenario into its reconciles, and every request of its Client is an
injection point.

The design behind everything on this page is in
[Proposal: failure injection](https://github.com/z5labs/humus/discussions/869),
with its [revision 1](https://github.com/z5labs/humus/discussions/869#discussioncomment-19020328),
[revision 2](https://github.com/z5labs/humus/discussions/869#discussioncomment-19021074)
and [revision 3](https://github.com/z5labs/humus/discussions/869#discussioncomment-19022262).

## The model

A scenario is a `fit.Scenario`: an ordered list of rules, each naming an
injection point and the `fit.Fault` it emulates, a `Delay`, an abort, or
both. The first rule whose point matches decides, and a point no rule
matches is spared. A rule's point is a point's name, a prefix followed by
`*`, such as `http:*`, or a lone `*`.

A request carries its scenario in its baggage, so it travels with the
request from service to service, as the trace context does, through the
propagator `telemetry.New` builds. Each injection point evaluates itself
against the scenario of the request it serves. A request that carries no
scenario is never touched, and costs a point nothing but a lookup.

Three parties own the parts of it:

- **`fit` owns the model:** `Scenario`, `Rule` and `Fault`, the split of a
  codec into a `Decoder` and an `Encoder`, and the `Evaluator`, which
  decodes a request's scenario at each point and clamps every `Delay` to a
  maximum.
- **humus owns the conventions:** the `fault.Injector` an App builds before
  `Init`, the point in every humus client and what an abort looks like in
  each protocol, stripping scenarios at every server and in every client,
  the span events, and `*fault.InjectedError`.
- **The end user owns the rest:** the wire format, which their edge and
  every service of theirs agree on; which requests are under test, decided
  at their edge; which callers each server trusts; and which dependencies a
  scenario goes on to.

## Turning it on

A service turns failure injection on by passing a function to its `Main`
with `humus.WithFaults`. The function returns the `fault.Settings`, and the
App builds the `fault.Injector` from them:

```go
httpserver.Main(Init, humus.WithFaults(Faults))
```

`fault.Settings` has two fields. `Decoder` is the codec that reads a scenario
from a request's baggage, and strips it. It has no default, so it is always
set, in code. `MaxDelay` is the longest `Delay` any point waits: a longer
one is clamped to it, so services with different maximums still agree on
the rest of a scenario. It defaults to 10s and must be positive.
`fault.CheckMaxDelay` checks it, and the end user sources it as any other
setting, here from `FAULT_MAX_DELAY`:

```go
// Faults is the orders service's failure injection: a scenario is JSON in
// the baggage member acme.fault, and an operator sets MaxDelay with
// FAULT_MAX_DELAY.
func Faults(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
	codec, err := jsonscenario.New("acme.fault")
	if err != nil {
		return fault.Settings{}, err
	}
	d := fault.DefaultSettings()
	return config.Struct(func(r *config.Resolver) fault.Settings {
		c := d
		c.Decoder = codec
		c.MaxDelay = r.Get(config.Env(s.Process.Environment, "FAULT_MAX_DELAY").
			Parse(time.ParseDuration).Default(d.MaxDelay).Validate(fault.CheckMaxDelay))
		return c
	}).Read(ctx)
}
```

**The App calls the function after the telemetry and before `Init`,** with
the `Setup` that `Init` is then given, builds the `Injector` with
`fault.New`, and puts it in `Setup.Faults`. Every humus server and client
takes the `Injector` from the `Setup` it is built with, so none can miss
it, and `Init` hands it on by handing on the `Setup`. The function is given
the `Setup`, so a `Decoder` can load what it needs through `s.Host`, such as
a registry of scenarios from a file, and register its release with
`s.OnStop`.

**A failure fails startup** as a failure of `Init` does, without calling
`Init`. The App logs it as `failure injection: ` and the error, and exits 78
when a `*config.Error` is in its chain, such as a `FAULT_MAX_DELAY` of `0s`,
or 1 otherwise, such as a `Settings` with no `Decoder`.
[The App lifecycle](/humus/pr-preview/pr-1025/lifecycle/#what-run-does-in-order) shows
where it comes in the startup order.

**Without `humus.WithFaults`, failure injection is off.** `Setup.Faults` is
then the zero `Injector`, which evaluates nothing and strips nothing, so a
scenario in a request's baggage goes on to every dependency as any baggage
member does. There is no setting that turns failure injection on or off at
run time: a `Decoder` that rejects every scenario, such as a registry with
no entries, turns it off in effect, without a rebuild.

## Choosing a codec

The codec owns the wire format: which baggage members carry a scenario,
their grammar and size, and whether a scenario is signed or expires. The
edge that writes scenarios and every service that reads them agree on it,
and humus imposes none, because an organisation's edge, such as Envoy, Kong
or Zuul, is often owned by another team.

### `jsonscenario`

`jsonscenario.New(key)` returns a codec that carries a scenario as JSON in
one baggage member, whose key the end user names. There is no default key.
The member's value is the scenario, percent-encoded as W3C Baggage requires:

```text
acme.fault={"rules":[{"point":"http:payments:8443","abort":true},{"point":"*","delay_ms":250}]}
```

- `point` is required.
- `delay_ms` is a non-negative whole number of milliseconds, which any
  language can write. It defaults to 0.
- `abort` defaults to false.
- An unknown field is an error, so a typo such as `"abrot"` is reported in
  the request's telemetry rather than read as a rule that spares its point.
- `Encode` rejects a member over 2048 bytes percent-encoded, a quarter of
  the 8192 bytes W3C Baggage guarantees a whole header, since OpenTelemetry
  drops all of a header's baggage when it is over the limit. It rejects a
  `Delay` that isn't whole milliseconds too. `Decode` reads a member of any
  size.

It has no signature and no expiry. Anyone who can set a request's baggage
can write a scenario in it, so trust comes from where a request came from:
each server's [`TrustScenario`](#trust-at-each-server), and the edge
stripping any scenario a request arrived with before it writes its own.

### A codec of your own

An end user who needs something else, such as a signed format, scenarios
that expire, or a registry of scenario IDs, writes a `fit.Decoder`: `Decode`
reads a scenario from baggage, reporting whether there was one, and `Strip`
removes exactly the members `Decode` reads. This one carries the ID of a
scenario that the service holds, and rejects an ID it doesn't hold:

```go
// Registry is an end user's fit.Decoder: the baggage member acme.scenario
// carries the ID of one of its scenarios, which an edge of theirs sets on
// the requests under test. An ID it doesn't hold is rejected.
type Registry map[string]fit.Scenario

// Decode reads the scenario of the ID in b.
func (r Registry) Decode(b baggage.Baggage) (fit.Scenario, bool, error) {
	m := b.Member("acme.scenario")
	if m.Key() == "" {
		return fit.Scenario{}, false, nil
	}
	s, ok := r[m.Value()]
	if !ok {
		return fit.Scenario{}, false, fmt.Errorf("unknown scenario %q", m.Value())
	}
	return s, true, nil
}

// Strip removes the ID from b.
func (Registry) Strip(b baggage.Baggage) baggage.Baggage {
	return b.DeleteMember("acme.scenario")
}
```

A `Decoder` is shared by every request, so it must be safe for concurrent
use. A scenario it rejects gives the zero `Fault`, and the request goes on
unharmed, with the error in the request's [telemetry](#telemetry).

### Writing a scenario

The edge, or a test, writes a scenario into a request's baggage with
`fit.ContextWithScenario` and a codec that is also a `fit.Encoder`, as
`jsonscenario`'s is, and the propagator carries it on:

```go
ctx, err = fit.ContextWithScenario(ctx, codec, fit.Scenario{Rules: []fit.Rule{
	{Point: "http:payments:8443", Fault: fit.Fault{Abort: true}},
	{Point: "*", Fault: fit.Fault{Delay: 250 * time.Millisecond}},
}})
```

humus doesn't decide which requests are under test, whether by account, by
device or by a percentage of traffic. That is the edge's decision, and
[Containing a scenario in production](#containing-a-scenario-in-production)
is about making it.

## Injection points

Every humus client is an injection point, named after its dependency, from
the attribute OpenTelemetry already records of it. So a rule can target one
dependency, such as `http:payments:8443`, or every dependency of one kind,
such as `http:*`, and a scenario's rules read like the trace they fail. Each
attempt evaluates its point once, inside the client's circuit breaker where
it has one, so an attempt the breaker rejects evaluates nothing:

| Client | Point | An abort | A delay |
|---|---|---|---|
| [`httpclient`](/humus/pr-preview/pr-1025/http-client/) | `http:<host>:<port>` of `BaseURL`, with the scheme's port when the URL has none, such as `http:payments:8443` | a transport error: the call returns an `*url.Error` wrapping the `*fault.InjectedError`, with no response, and nothing is sent | waited inside `AttemptTimeout`, so it is in the attempt's span and in `http.client.request.duration` |
| [`grpcclient`](/humus/pr-preview/pr-1025/grpc-client/) | `grpc:<service>/<method>`: the full method without its leading slash, as `rpc.method` writes it, such as `grpc:payments.v1.Payments/Charge` | status `Unavailable`, wrapping the `*fault.InjectedError`. grpc-go isn't called, so the Client records the attempt's span itself | waited before grpc-go is called, inside `Timeout`. Cut short, it ends as grpc-go would, `DeadlineExceeded` or `Canceled` |
| The Kafka [`Producer`](/humus/pr-preview/pr-1025/kafka/#producing) | `kafka:<topic>`, one evaluation per record, such as `kafka:orders` | every record of the call fails unsent. A call is one unit, as the breaker counts it, so one record's abort fails them all | the call waits the longest `Delay` of its records, inside `DeliveryTimeout` |
| The RabbitMQ [`Publisher`](/humus/pr-preview/pr-1025/rabbitmq/#publishing) | `rabbitmq:<exchange>:<routing key>`, leaving out what is empty, as `messaging.destination.name` writes it, such as `rabbitmq:orders:accepted` | the attempt fails with nothing sent, marked as a lost connection's error is: `rabbitmq: publishing to …: fit: injected failure at …` | waited inside `Timeout`, before the attempt takes a channel |
| The `kube` [`Client`](/humus/pr-preview/pr-1025/kubernetes/#injection-points) | `kube:<resource>[.<group>][/<subresource>]:<verb>`, one evaluation per request to the API server, which names what the request does to which resource as the API server's audit log does, such as `kube:deployments.apps:patch` | a transport error: client-go returns an error wrapping the `*fault.InjectedError`, with no response, nothing sent and no call to `Connection.Token` | waited before the request is sent, inside its span and in `http.client.request.duration` |

An abort emulates a dependency that can't be reached, in every client: a
transport error, `Unavailable`, a lost connection. A delay is waited, and
then the attempt goes on as usual. A delay that the attempt's deadline or
its caller cuts short ends the attempt.

Either way, an attempt that a fault ended fails with a
[`*fault.InjectedError`](https://pkg.go.dev/github.com/z5labs/humus/fault#InjectedError)
naming its point. An abort's matches `fit.ErrInjected` with `errors.Is`. A
delay cut short's holds what cut it short, such as `context.DeadlineExceeded`,
which it matches, and doesn't match `fit.ErrInjected`. One exception: an
`httpclient` call that its `Timeout` ends returns net/http's timeout error,
which matches `context.DeadlineExceeded` but holds no `*fault.InjectedError`,
since net/http replaces the error the transport returned. So may a `kube`
request whose delay is cut short by the `Timeout` of a `rest.Config` the end
user set, since client-go then gives the `http.Client` that `Timeout` too.
`Client.Config()` sets none, so a controller's own requests always fail with
the `*fault.InjectedError`. The attempts' spans still carry the injected
error's `error.type`.

The probes servers, the OTLP exporters and `grpcclient`'s `Connected` serve
no request, and are no injection point. A Kubernetes controller's informers,
leader election and check that its resource is served run on `Serve`'s
context, which never carries a scenario, so their requests are evaluated
without one.

### Points of your own

The end user's own code can be an injection point too, such as a database
query, by evaluating a point with `s.Faults.Eval`. Name it
`<system>:<name>`, after the protocol's `*.system` attribute, such as
`postgresql:orders` or `redis:sessions`, as humus's own points are named.
This one fails as humus's clients do, with a `*fault.InjectedError`:

```go
// queryOrders is an injection point of the end user's own, named
// <system>:<name>. A fault aborts it with an *fault.InjectedError, as
// humus's clients fail.
func queryOrders(ctx context.Context, faults fault.Injector) error {
	const point = "postgresql:orders"
	f := faults.Eval(ctx, point)
	if err := f.Wait(ctx); err != nil {
		return &fault.InjectedError{Point: point, Err: err}
	}
	if f.Abort {
		return &fault.InjectedError{Point: point}
	}
	return nil // query the database
}
```

`f.Wait` waits the fault's `Delay`, and returns the context's cause if the
context is done first. A point that has no failure more realistic to emulate
can call `f.Inject(ctx)` instead, which waits and then fails with
`fit.ErrInjected`.

## Trust at each server

A scenario is an instruction to fail, so a service must decide whom it takes
that instruction from. Every humus server strips the scenario a request
carries, right after it extracts the request's baggage and before any of the
end user's code runs, unless its `TrustScenario` setting trusts the caller:

| Server | `TrustScenario` is given |
|---|---|
| [`httpserver`](/humus/pr-preview/pr-1025/http/) | the `*http.Request`, as it arrived |
| [`grpcserver`](/humus/pr-preview/pr-1025/grpc/) | the RPC's context, which holds its peer and its incoming metadata, and its full method |
| The Kafka [consumer](/humus/pr-preview/pr-1025/kafka/) | the record's `kafka.Message` |
| The RabbitMQ [consumer](/humus/pr-preview/pr-1025/rabbitmq/) | the delivery's `rabbitmq.Message` |
| A Kubernetes [controller](/humus/pr-preview/pr-1025/kubernetes/#trusting-an-objects-annotation-trustscenario), with the `kube.TrustScenario` part | the object, whose annotation carries the scenario into the object's reconciles |

- **A nil `TrustScenario` trusts no caller,** and is the default. A server
  the end user hasn't decided about is a trust boundary, so an API on the
  internet strips every scenario without any setting, and only a server the
  end user marks takes them. A controller without the `TrustScenario` part
  reads no annotation.
- **A stripped request keeps every other baggage member,** and its span gets
  a `humus.fit.strip` event.
- **`TrustScenario` is asked only about a request that carries a scenario,**
  and only when the App has failure injection, so it costs nothing on any
  other request. It is set in code, not sourced: whom a server trusts is the
  end user's design decision, not an operator's.
- **A panic in it is handled** as its server handles a panic of the end
  user's code: `httpserver` and `grpcserver` fail that request, the
  consumers recover nothing, as for a `Processor`, and a controller recovers
  it as a Reconciler's panic, and doesn't retry the object.

Trust needs nothing from [authentication](/humus/pr-preview/pr-1025/auth/), which runs
later, in the end user's handlers and interceptors. A request already
carries what is needed to trust it: a verified client certificate, a
listener only internal callers reach, a header or metadata that a gateway
sets and strips from outside callers, a topic, or a publisher's user. A
`TrustScenario` is the end user's code, and may call whatever they have,
such as a verify function of their own.

### mTLS

A server behind a service mesh trusts a caller whose client certificate the
mesh's CA verified. With `httpserver`, the certificate is in
`r.TLS.VerifiedChains`:

```go
api.TLS.ClientAuth = tls.VerifyClientCertIfGiven
api.TLS.ClientCAs = meshCAs
api.TrustScenario = func(r *http.Request) bool {
	return r.TLS != nil && len(r.TLS.VerifiedChains) > 0
}
```

With `grpcserver`, it is in the `credentials.TLSInfo` of the RPC's peer:

```go
settings.TLS.ClientAuth = tls.VerifyClientCertIfGiven
settings.TLS.ClientCAs = meshCAs
settings.TrustScenario = func(ctx context.Context, _ string) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	return ok && len(info.State.VerifiedChains) > 0
}
```

`tls.VerifyClientCertIfGiven` still serves a caller with no certificate,
such as the internet behind a load balancer: its scenario is stripped, and
its request is served.

### Kafka: the topic

Who can produce to a topic is the cluster's to decide, with its ACLs, so a
consumer trusts a record by its topic. This one consumes a topic only the
end user's own services can write to:

```go
settings.TrustScenario = func(m kafka.Message) bool {
	return m.Topic == "orders"
}
```

### RabbitMQ: `UserID`

RabbitMQ [validates a message's `user-id`](https://www.rabbitmq.com/docs/validated-user-id)
against the user of the publishing connection, and refuses a publish that
claims another, so `Message.UserID` names the publisher. This consumer
trusts the end user's own services, which connect as `orders-api`:

```go
settings.TrustScenario = func(m rabbitmq.Message) bool {
	return m.UserID == "orders-api"
}
```

The `user-id` comes only from the publisher. RabbitMQ adds none to a
message published without one, so nothing the consumer sees tells it who
published that message, and its empty `UserID` isn't trusted by that func.
A humus service's messages carry one when each `Publish` sets
`Publishing.UserID` to the user its `Publisher`'s `Connection` logs in as:
the login it sources for `Connection.SASL`, or the user its client
certificate maps to. This `Processor` of `orders-api` publishes each order
on to the next service, with a `Publisher` whose `ForwardScenario` is set:

```go
// relay is a Processor that publishes each order on to the next of the end
// user's services. Each message names the user its Publisher logs in as,
// which RabbitMQ validates, so the next service's TrustScenario can trust
// the scenario the Publisher forwards.
type relay struct {
	pub  *rabbitmq.Publisher // built with ForwardScenario set
	user string              // the user pub logs in as, such as orders-api
}

// Process publishes the order m carries on, with the user-id r.user.
func (r relay) Process(ctx context.Context, m rabbitmq.Message) error {
	return r.pub.Publish(ctx, rabbitmq.Publishing{
		Exchange:   "orders",
		RoutingKey: "accepted",
		MessageID:  m.MessageID,
		UserID:     r.user,
		Body:       m.Body,
	})
}
```

A `UserID` that isn't that user is refused by the broker with
`406 PRECONDITION_FAILED`, which `Publish` returns at once, with no retry.

### Kubernetes: the object's annotation

A controller's reconciles answer no request, so a scenario reaches them in
an annotation of the object they reconcile, which the end user names, such
as `acme.example.com/failure-scenario`. Who may set it is the cluster's to
decide, with RBAC and admission, so `kube.TrustScenario`'s `trust` is handed
the object, and reads what those decisions leave visible on it. This one
trusts every object in the staging namespace, where an admission policy lets
only the chaos harness's ServiceAccount set the annotation:

```go
kube.TrustScenario(scenarioAnnotation, func(obj metav1.Object) bool {
	return obj.GetNamespace() == "staging"
}),
```

The annotation's value is W3C baggage that carries a scenario as the App's
`Decoder` reads it, so a scenario written for a request's `baggage` header
goes on an object unchanged. Only the `Decoder`'s members join the
reconcile's context: any other member of the annotation is dropped. The
reconcile's Reconciler, a `Finalize`'s function, the controller's own
writes, and every request and point they reach then see the scenario. An
untrusted object's scenario, and a value that isn't W3C baggage, are
stripped on the reconcile's span. Adding the annotation starts an
experiment, and removing it ends one, at once, as
[Starting and ending an experiment](/humus/pr-preview/pr-1025/kubernetes/#starting-and-ending-an-experiment)
shows.

### A job trusts its `BAGGAGE`

A [job](/humus/pr-preview/pr-1025/jobs/#joining-the-trace-that-started-the-job) reads
its baggage from the `BAGGAGE` variable in its environment, and trusts it.
Whoever starts the job, with its pod spec or otherwise, already sets every
one of its settings, so a scenario there is set by the same hand. A job
started by a service hands on that service's baggage, which that service's
own server has already stripped. So `job` has no `TrustScenario`, and strips
nothing.

The probes servers of `httphealth` and `grpchealth` extract no baggage, and
need nothing either. A probe handler attached to a Service is behind that
Service's strip, like every other request.

## Sending a scenario on: `ForwardScenario`

Every humus client strips the scenario from what it sends, after its point
has read it, unless `ForwardScenario` is set in its settings:
`httpclient.Settings`, `grpcclient.Settings`, `kafka.ProducerSettings` and
`rabbitmq.PublisherSettings`. A dependency the end user hasn't decided about
is a trust boundary too: it may be a third party that would only log the
scenario, or a service of another team, with another codec. Every other
baggage member, and the trace context, still goes, and each strip adds a
`humus.fit.strip` event to the attempt's span.

Set `ForwardScenario` for a dependency that takes part in failure injection
with the same codec, such as another service of the end user's, so that a
scenario can reach the points that dependency calls in turn:

```go
payments.ForwardScenario = true // payments is ours, and honors acme.fault too
client, err := httpclient.NewClient(s, payments)
if err != nil {
	return nil, err
}
```

A scenario aimed at a service's own dependencies needs no `ForwardScenario`:
the points are in the service's own clients, which evaluate it before they
strip it. Only its server's trust is needed. For a Kafka `Producer` or a
RabbitMQ `Publisher`, the dependency is every consumer of the topic or the
destination, and so is the trust boundary.

**The `kube` Client has no `ForwardScenario`.** The API server takes no part
in failure injection, so the Client strips the scenario from every request
it sends. In Kubernetes, sending a scenario on means carrying it on to the
reconciled object's children instead: in a reconcile that carries a
scenario, the Client sets the `TrustScenario`'s annotation in every
server-side apply of an object whose controller owner reference is the
reconciled object. So one experiment on the top object of an operator of
several controllers reaches every controller below it, as
[An operator of two controllers](/humus/pr-preview/pr-1025/kubernetes/#an-operator-of-two-controllers)
shows. A create, an update, and a patch that isn't an apply are sent as
they are, since only an apply takes the annotation off again once the
experiment ends.

Forwarding puts the scenario on what is sent, and the next service still
decides whether to trust it, as a child's controller does with a
`TrustScenario` of its own. A RabbitMQ consumer that trusts by `UserID`
keeps a forwarded scenario only from a `Publish` that sets
`Publishing.UserID` to the user its `Publisher` logs in as, as
[RabbitMQ: `UserID`](#rabbitmq-userid) shows.

## A fault is a real failure

Retries and breakers know nothing of failure injection. Every client decides
an attempt that a fault ended with its own classifier, the end user's
`Classify` included, as it decides the failure the fault emulates. So the
attempt is retried, and the breaker counts it, as that failure would be:

| Client | An abort | A delay cut short |
|---|---|---|
| `httpclient` | a transport error: `Retry`, `Failure`, under the default `Classify` | `Retry`, `Failure`, as `AttemptTimeout`'s own error is |
| `grpcclient` | `Unavailable`: `Retry`, `Failure`, under the default `Classify` | `DeadlineExceeded`: `Stop`, `Failure`. `Canceled`: `Stop`, `Ignored` |
| Kafka `Producer` | `Failure`. The Producer retries nothing of its own, and franz-go is never reached | by `DeliveryTimeout`: `Failure`, as records not stored in time are |
| RabbitMQ `Publisher` | as a lost connection: `Retry`, `Failure` | as an attempt whose context ended: no further attempt, and `Failure` when the deadline passed, `Ignored` when the caller cancelled |
| `kube` `Client` | returned by client-go at once, since it retries only a connection that was reset, ended early or lost, and a response with `Retry-After`, not even a GET's. Returned by the Reconciler, it retries the object with the work queue's backoff, and the reconcile's outcome is `error` | returned by client-go at once, and retried with the object, as an abort is. A delay that isn't cut short only slows the request, and the reconcile |

- **A retried attempt meets the same fault.** Each attempt evaluates the
  point again, and the scenario hasn't changed. So a call that a scenario
  aborts fails as a call to a dependency that is down does: its attempts run
  out, or the retry budget or the breaker stops them.
- **The breaker opens for everyone.** Under the default policy, five failed
  attempts in a row open the client's breaker. It then sheds every caller's
  requests to that dependency until it probes again, not only the requests
  under test. The retries draw on the retry budget that real traffic shares.
  That is what a dependency that is down does to the service, and seeing it
  is the point of the scenario.
- **humus's own instruments count these attempts** like any other, such as
  `humus.circuit.calls` and `humus.retry.decisions`.
- **A controller has no breaker, and its Client retries nothing.** Its work
  queue retries the object instead, and the scenario stays as long as the
  object's annotation, so every retry meets it again: an object whose
  reconcile a scenario aborts backs off to `RetryMaxDelay`, as it would
  while the API server is down. The controller's own writes, its status and
  its finalizer, are requests like any other, so a scenario can fail them
  too.

FIT works this way at Netflix: injected latency exhausted a Hystrix
command's thread pool, its fallbacks were served, and its circuit opened,
which is how a timeout set too high for its pool was found
([Basiri et al.](https://arxiv.org/abs/1905.04648)).

An end user who wants a client's breaker spared writes a `Classify` that
singles out a `*fault.InjectedError` with `errors.As`, in `httpclient` or
`grpcclient`. This one answers `Stop`, `Ignored` for it, which keeps the
breaker closed and makes one attempt per call, and hands everything else to
the default:

```go
classify := httpclient.DefaultSettings().Classify
spare := func(resp *http.Response, err error) httpclient.Classification {
	var ie *fault.InjectedError
	if errors.As(err, &ie) {
		return httpclient.Classification{Retry: retry.Stop, Breaker: circuit.Ignored}
	}
	return classify(resp, err)
}
```

humus doesn't spare the breaker of its own accord.
[Calling HTTP services](/humus/pr-preview/pr-1025/http-client/#writing-a-classify) and
[Calling gRPC services](/humus/pr-preview/pr-1025/grpc-client/#writing-a-classify)
explain `Classify`.

## Containing a scenario in production

A scenario decides which requests carry a fault, and what that fault then
does to state those requests share with others is real: a breaker that
opens sheds every caller's requests, and retries draw on a budget everyone
shares. Containing that is the deployment's job, not humus's, and the end
user has two levers.

**Choose the requests under test.** The edge decides which requests carry a
scenario, whether by an employee's account, a test device or a small share of
traffic, and strips any scenario a request arrived with before it writes its
own. A scenario aimed at one dependency, with one rule, leaves the service's
other dependencies alone. A scenario short enough not to open a breaker,
such as a delay under `AttemptTimeout`, shows its latency without failing
anything. `MaxDelay` bounds what any scenario can make a point wait, and an
operator lowers it with `FAULT_MAX_DELAY`.

**Choose where they run.** Route the requests under test to instances of
their own, so the breakers and retry budgets they open and draw down are
theirs alone. Netflix's ChAP did this: it routes an experiment's users to a
canary cluster of their own, 1% of the original cluster's size, beside a
baseline cluster for a control group
([Basiri et al.](https://arxiv.org/abs/1905.04648)). With humus, those
instances are the same service deployed again, and the edge routes the
requests it puts under test to them.

A client whose breaker must never open for a scenario, wherever it runs,
can have the end user's [`Classify`](#a-fault-is-a-real-failure) spare it.
Failing a dependency for every request, without a scenario, as Envoy's fault
filter does, is a chaos experiment rather than failure injection, and humus
doesn't do it.

## Telemetry

On a request that carries a scenario, every evaluation adds a
`humus.fit.evaluation` event to the span in the point's context, modelled on
OpenTelemetry's `feature_flag.evaluation` event:

| Attribute | When | Value |
|---|---|---|
| `humus.fit.point` | always | the point evaluated |
| `humus.fit.rule` | a rule matched | the rule's point |
| `humus.fit.delay` | a rule matched | the `Delay`, clamped to `MaxDelay`, in seconds |
| `humus.fit.abort` | a rule matched | whether the point aborts |
| `error.type`, `error.message` | the `Decoder` rejected the scenario | the `Decoder`'s error |

An unmatched evaluation still names its point, which is how the points a
request passes through are discovered: a scenario with no rules injects
nothing and reports every point. A request with no scenario adds no event.
In `httpclient` and the Kafka `Producer`, the span is the attempt's or the
record's own. In `grpcclient` it is the caller's, since the attempt's span
doesn't exist yet. The RabbitMQ `Publisher`'s is the `Publish`'s span. The
`kube` Client's is the request's own span, from `otelhttp`, except a
watch's: the Client doesn't trace a watch, so its event lands on the span in
its context, such as a reconcile's.

**A strip is a `humus.fit.strip` event,** with no attributes, on the span
where it happens: a server's span when a server strips an untrusted caller,
a client's span when a client strips what it sends, and a reconcile's span
when a controller drops an object's annotation. The span's kind tells a
server's from a client's.

**One strip has attributes:** a reconcile's strip of an annotation that
isn't W3C baggage has `error.type` and `error.message`. A server's
propagator drops baggage it can't parse before humus sees it, so a server's
strip has nothing to explain. A controller parses the annotation itself, so
only it can tell an annotation it can't read from an object it doesn't
trust. Setting a child's annotation adds no event: the apply's span has its
evaluation and its strip, and the child's reconciles show the scenario they
carry.

**An injected failure's `error.type` is
`github.com/z5labs/humus/fault.InjectedError`,** an abort or a delay cut
short, in every client's spans and in `http.client.request.duration`. One
value lets a query find every injected failure across protocols, and tell
one from a real failure or a real timeout.

Failure injection emits no log records and records no instruments of its
own. Each evaluation is on its request's trace, and the failure an abort
causes is already recorded by the client's own span, and by its instruments.

## Testing a scenario

A test turns failure injection on as `main` does, with
`humustest.WithApp(humus.WithFaults(…))`, and runs the end user's whole App
with [`humustest.Start`](/humus/pr-preview/pr-1025/testing/). The test is the edge: it
writes the scenario into a request's baggage itself.

### Through a humustest `Host`

This test runs the orders service's `Init` and `Faults`, from above, next to
a fake payments dependency on the same humustest `Host`, inside
`testing/synctest`. `edge` sends orders a request whose baggage carries a
scenario, as a caller in the mesh when it is given a certificate from the
mesh's CA:

```go
// edge sends GET /orders/7/payment to orders, as a caller in the mesh when
// cert is set, its baggage carrying s, written with the service's codec.
func edge(t *testing.T, h *humustest.Host, ca *humustest.CA, cert *humustest.Certificate, s fit.Scenario) int {
	t.Helper()
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	ctx, err := fit.ContextWithScenario(t.Context(), codec, s)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://orders:9443/orders/7/payment", nil)
	require.NoError(t, err)
	propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(req.Header))

	c := h.HTTPClient()
	cfg := &tls.Config{RootCAs: ca.Pool()}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{cert.TLS()}
	}
	c.Transport.(*http.Transport).TLSClientConfig = cfg
	resp, err := c.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}
```

A caller in the mesh aborts payments, and its call sends nothing. The same
scenario from a caller outside the mesh is stripped, and its call is
served. A trusted scenario that spares payments is forwarded to it:

```go
p, status := humustest.Start(t, Init,
	humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
	humustest.WithApp(humus.WithFaults(Faults)),
	humustest.WithEnv(ordersEnv),
)
<-h.Listening("orders:9443")

abort := fit.Scenario{Rules: []fit.Rule{{Point: "http:payments:8443", Fault: fit.Fault{Abort: true}}}}
spare := fit.Scenario{Rules: []fit.Rule{{Point: "http:ledger:8443", Fault: fit.Fault{Abort: true}}}}
mesh := ca.Issue(t, "checkout")

require.Equal(t, http.StatusBadGateway, edge(t, h, ca, mesh, abort), "a caller in the mesh aborts payments")
require.Empty(t, payments.seen(), "an aborted call reached payments")

require.Equal(t, http.StatusOK, edge(t, h, ca, nil, abort), "the scenario of a caller outside the mesh is stripped")
require.Equal(t, http.StatusOK, edge(t, h, ca, mesh, spare))
require.Equal(t, []bool{false, true}, payments.seen(), "only the trusted scenario is forwarded")
```

Fake time means the aborted call's retries and backoffs cost the test
nothing. A test that asserts on the events and the `error.type` records the
App's telemetry with [`humustest.Telemetry`](/humus/pr-preview/pr-1025/testing/) in
place of `telemetry.NoopBuilder`, and reads the spans' events from it.

### A job's `BAGGAGE`

A job reads its scenario from `BAGGAGE`, so a test sets the variable with
`humustest.WithEnv`, its value a W3C baggage header's. `queue/kafka`'s tests
write it with the codec's `Encode`:

```go
// scenarioBaggage returns the baggage tenant=acme with s written into it,
// as a W3C baggage header's value.
func scenarioBaggage(t *testing.T, s fit.Scenario) string {
	t.Helper()
	m, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(m)
	require.NoError(t, err)
	b, err = faultsCodec(t).Encode(b, s)
	require.NoError(t, err)
	return b.String()
}
```

and run a job that produces, under it:

```go
_, status := humustest.Start(t, produces(settings, run),
	humustest.WithApp(opts...),
	humustest.WithEnv(map[string]string{"BAGGAGE": bag}),
)
```

### A Kafka record's `baggage` header

A Kafka record carries its baggage in a `baggage` header. A test of a
consumer produces records whose header carries a scenario, with kfake, as
[Consuming and producing Kafka](/humus/pr-preview/pr-1025/kafka/#testing) describes,
here one with a scenario and one without:

```go
produceRecords(t, h, c, []*kgo.Record{
	{Topic: topic, Value: []byte("0"), Headers: scenarioHeaders(t, abortOrders)},
	{Topic: topic, Value: []byte("1"), Headers: producerHeaders(t, producerSpanContext(t))},
})
```

```go
// scenarioHeaders returns a producer's headers whose baggage is tenant=acme
// and s.
func scenarioHeaders(t *testing.T, s fit.Scenario) []kgo.RecordHeader {
	t.Helper()
	return []kgo.RecordHeader{{Key: "baggage", Value: []byte(scenarioBaggage(t, s))}}
}
```

A test of a `Producer` reads the header of what it stored, to see whether
the scenario went on:

```go
stored := readAll(t, h, c, topic)
require.Len(t, stored, 1)
b := storedBaggage(t, stored[0])
require.Equal(t, "acme", b.Member("tenant").Value())
require.Equal(t, forward, b.Member(scenarioKey).Key() != "", "the scenario member")
```

### A Kubernetes object's annotation

A test of a controller is the harness: it writes the scenario into an
object's annotation. Inside `synctest`, on client-go's fakes, as
[Testing](/humus/pr-preview/pr-1025/kubernetes/#the-reconciler-as-a-function) on the
Kubernetes page describes, `kube`'s own test of the staging controller
starts an experiment on a Memcached in staging and on one elsewhere, with
the harness's `setScenario`, and sees only the first carry the scenario:

```go
staging := fc.dyn.Resource(Memcached.GVR()).Namespace("staging")
elsewhere := fc.dyn.Resource(Memcached.GVR()).Namespace("default")
require.NoError(t, setScenario(t.Context(), staging, "cache", value))
require.NoError(t, setScenario(t.Context(), elsewhere, "cache", value))
time.Sleep(settle)

require.Equal(t, []bool{false, true}, rec.of("staging/cache"), "the annotation starts the experiment at once")
require.Equal(t, []bool{false, false}, rec.of("default/cache"), "only the staging namespace runs experiments")
```

`rec`'s Reconciler records, for each reconcile, whether
`s.Faults.Eval(ctx, "kube:deployments.apps:patch")` aborts. client-go's
fakes never see a request's context, so the points of the Client's own
requests, and the annotation carried on to children, are tested against
k3s, as
[The whole App, against k3s](/humus/pr-preview/pr-1025/kubernetes/#the-whole-app-against-k3s)
describes.

### A unit test

A unit test of the end user's code that takes a `Setup` sets `Setup.Faults`
from `fault.New`, with the codec of its choice:

```go
faults, err := fault.New(settings)
require.NoError(t, err)
s := &humus.Setup{Faults: faults}
```
