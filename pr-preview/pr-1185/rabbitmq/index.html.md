# Consuming and publishing with RabbitMQ

The [`queue/rabbitmq`](https://pkg.go.dev/github.com/z5labs/humus/queue/rabbitmq)
package consumes a RabbitMQ queue as a `humus.Service`, with
[amqp091-go](https://github.com/rabbitmq/amqp091-go), RabbitMQ's own Go
client. The Service dials the broker through the Setup's Host, consumes one
queue with manual acks, and hands each delivery to the end user's
`queue.Processor`, in the order the queue delivered them. The end user's
`Init` sources its `Settings`, builds the Service with `rabbitmq.NewService`,
and returns it to `humus.Main`, whose defaults are a consumer's.

The package publishes too. A `rabbitmq.Publisher`, which the end user's
`Init` builds with `rabbitmq.NewPublisher`, publishes to the broker with
publisher confirms for any Service of the process: a `Processor` that
publishes an event, an HTTP handler, a job. [Publishing](#publishing)
describes it.

[`queue`](https://pkg.go.dev/github.com/z5labs/humus/queue), in the core
module, is the contract between the end user's code and any broker.
`queue/rabbitmq` is in the nested module `github.com/z5labs/humus/queue/rabbitmq`,
so a service that doesn't use RabbitMQ doesn't get amqp091-go in its module
graph. The package has no App, no `Main`, no defaults of its own, no test
package and no fake broker. The design behind it is in
[Proposal: RabbitMQ](https://github.com/z5labs/humus/discussions/765), with
its [revision 1](https://github.com/z5labs/humus/discussions/765#discussioncomment-19009926).

## The contract

The end user writes the same thing for RabbitMQ as for any broker: a
[`queue.Processor`](https://pkg.go.dev/github.com/z5labs/humus/queue#Processor),
whose `Process(ctx, msg)` processes one message and returns an error when it
couldn't. `queue.ProcessorFunc` turns a function into one, and for RabbitMQ
the message is a `rabbitmq.Message`: the queue it came from, the exchange and
routing key it was published with, whether it was `Redelivered`, its
headers, its properties, such as `MessageID` and `CorrelationID`, and its
body.

A [`queue.Delivery`](https://pkg.go.dev/github.com/z5labs/humus/queue#Delivery)
says when a message is acknowledged, `queue.AtLeastOnce` after `Process` has
returned `nil` for it, or `queue.AtMostOnce` before `Process` is called. The
rules every broker package's Service keeps, and why a `Processor` under
`AtLeastOnce` must be idempotent, are on the Kafka page, under
[The contract](/humus/pr-preview/pr-1185/kafka/#the-contract); they hold here
unchanged. What RabbitMQ adds is in [Delivery](#delivery).

**So a `Processor` under `AtLeastOnce` must be idempotent.** This one
stores each order once, by the message's ID, and publishes an
`order.accepted` event for it:

```go
// orders is the end user's Processor: it stores each order once, by the
// message's ID, so an order delivered again is stored once, and publishes
// an order.accepted event for it.
type orders struct {
	mu     sync.Mutex
	stored map[string][]byte
	events events
}
```

Its `Handle` method, the `Processor`'s function, stores the order and then
publishes the event:

```go
// Handle stores the order m carries, and publishes an order.accepted event
// for it, with the order's ID as the event's message ID. It publishes on
// every delivery, a redelivery included: an event whose publish failed is
// published when the order comes again, and an event published twice is
// told apart by its ID, as any consumer under queue.AtLeastOnce does.
func (o *orders) Handle(ctx context.Context, m rabbitmq.Message) error {
	if m.MessageID == "" {
		return errors.New("an order has no ID")
	}
	o.store(m)
	// Returns once the broker has confirmed the event, so the order is acked only after.
	return o.events.Publish(ctx, rabbitmq.Publishing{
		Exchange:    "orders",
		RoutingKey:  "order.accepted",
		MessageID:   m.MessageID,
		ContentType: m.ContentType,
		Body:        m.Body,
	})
}
```

`store` is the idempotent part: an order already stored is skipped. A real
`Processor` stores orders in a database, where the same idea is an upsert,
or a unique key that turns a second insert into a no-op. `events` is an
interface of the end user's own over `Publish`, which
[`*rabbitmq.Publisher`](#publishing) satisfies and the `Processor`'s unit
tests fake, as [A Processor as a unit](#a-processor-as-a-unit) shows:

```go
// events is what the orders Processor publishes with: the one method of
// *rabbitmq.Publisher it calls, behind an interface of the end user's own,
// so that its unit tests can fake it.
type events interface {
	Publish(ctx context.Context, m rabbitmq.Publishing) error
}
```

An order with no ID fails on every delivery. Under `AtLeastOnce` that stops
the Service each time, until the queue's delivery limit dead-letters the
message, as [Dead letters](#dead-letters) describes.

## A consumer

The end user's `Init` sources `rabbitmq.Settings` with
[`config.Struct`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Struct),
as [Configuring a humus service](/humus/pr-preview/pr-1185/configuration/) describes,
builds the `Publisher` and the `Processor`, and returns the Service that
`rabbitmq.NewService` builds:

```go
// Init is the end user's Init: it sources the Settings with config.Struct,
// builds the Publisher and the Processor, and returns the Service from
// NewService.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := rabbitmq.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) rabbitmq.Settings {
		k := d
		// No .Default: the address, the login and the queue are required.
		k.Connection.Addr = r.Get(env("RABBITMQ_ADDR").Validate(rabbitmq.CheckAddr))
		k.Connection.SASL = &amqp.PlainAuth{
			Username: r.Get(env("RABBITMQ_USERNAME")),
			Password: r.Get(env("RABBITMQ_PASSWORD")),
		}
		k.Queue = r.Get(env("RABBITMQ_QUEUE").Validate(rabbitmq.CheckQueue))
		k.Prefetch = r.Get(env("RABBITMQ_PREFETCH").Parse(strconv.Atoi).Default(d.Prefetch).Validate(rabbitmq.CheckPrefetch))
		return k
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	pub, err := rabbitmq.NewPublisher(s, publisherOn(settings.Connection)) // opens no connection: the first Publish does
	if err != nil {
		return nil, err
	}
	h := &orders{stored: map[string][]byte{}, events: pub}

	svc, err := rabbitmq.NewService(s, settings, queue.ProcessorFunc[rabbitmq.Message](h.Handle))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}
```

`env` is the end user's own helper, which names a variable, and `amqp` is
`github.com/rabbitmq/amqp091-go`. The variable names are the service's own:
humus defines none of them. `publisherOn` is the end user's helper too, as
[Building a Publisher](#building-a-publisher) shows.

The consumer's `main` is one call to `humus.Main`, here with a telemetry
`Builder` that names the service, as
[Telemetry](/humus/pr-preview/pr-1185/telemetry/#writing-the-builder) describes:

```go
humus.Main(Init, humus.WithTelemetry(buildTelemetry))
```

**`NewService` opens no connection.** It runs every check on the `Settings`
again, rejects a `Concurrency` above `Prefetch` and a nil `Processor`, and
builds the Service's instruments. It registers no `OnStop` function, so an
`Init` that fails after it leaks nothing. `Serve` connects, as
[Connecting](#connecting) describes, and consumes. Dialling through the Host
is what lets a test put the App on a network of its own, with no test-only
seam.

**A Service consumes one queue,** with one `Delivery` and one `Processor`,
and its acks are on the one channel it consumes on, so a failure stops only
its own queue. `Init` returns it along with any other Service the process
runs, such as a second consumer or a probes server, as
[Readiness](#readiness) shows. Any Service returning ends the App.

## Settings

`rabbitmq.Settings` follows the
[settings convention](/humus/pr-preview/pr-1185/configuration/#what-a-package-with-settings-exports):
plain Go values, a check for each field, and `rabbitmq.DefaultSettings()` for
the defaults. Every field's zero value means zero, never "use the default",
so an `Init` starts from `DefaultSettings()` and changes the fields it
sources. The package reads no environment variable and no file, and defines
no variable name. How the broker is reached is a `rabbitmq.Connection` of
its own, in `Settings.Connection`, which the
[`Publisher`'s settings](#building-a-publisher) take too.

| Field | Default | Valid | Check |
|---|---|---|---|
| `Connection.Addr` | none: required | `host:port`, with a host and a port from 1 to 65535 | `CheckAddr` |
| `Connection.Vhost` | `/` | not empty | `CheckVhost` |
| `Connection.TLS` | `nil`, which means plain text | any | none |
| `Connection.SASL` | none: required | not nil | `CheckSASL` |
| `Connection.Heartbeat` | 10s | a whole number of seconds, from 1s to 65535s | `CheckHeartbeat` |
| `Connection.ConnectTimeout` | 30s | positive | `CheckConnectTimeout` |
| `Queue` | none: required | 1 to 255 bytes | `CheckQueue` |
| `Delivery` | `queue.AtLeastOnce` | `queue.AtLeastOnce` or `queue.AtMostOnce` | `queue.CheckDelivery` |
| `Prefetch` | 10 | 1 to 65535 | `CheckPrefetch` |
| `Concurrency` | 1 | 1 to `Prefetch` | `CheckConcurrency`, and `NewService` for the bound of `Prefetch` |

`Heartbeat` and `ConnectTimeout` keep amqp091's defaults. Heartbeats can't
be turned off: a dead connection would then go unnoticed until TCP gave up.
The broker's heartbeat, if shorter, wins, and a connection silent for 1.5
intervals is dead. `ConnectTimeout` bounds each dial, its TLS handshake and
its AMQP handshake together.

`Prefetch` is 10 and `Concurrency` is 1, so by default a queue's deliveries
are processed in the order the broker sent them, the round trip of each ack
is hidden behind nine more deliveries, and at most ten messages are held
back from the queue's other consumers. `Concurrency` above `Prefetch` would
leave workers that never get a delivery, so `NewService` rejects it.

### Required fields

**`Connection.Addr`, `Connection.SASL` and `Queue` are required.** None has
a default: humus imposes no broker, no port and no queue, and AMQP has no
anonymous login, so there is no login to default to. `DefaultSettings()`
leaves them `""`, `nil` and `""`, which their checks reject, and the end
user's chains for them have no `.Default`, as in `Init` above, so a
variable that isn't set fails as not found. The App exits 78, and
`config.Struct` names every variable at fault in one error. With none of
`Init`'s set:

```
init: config RABBITMQ_ADDR: not found
config RABBITMQ_USERNAME: not found
config RABBITMQ_PASSWORD: not found
config RABBITMQ_QUEUE: not found
```

A value that fails its check fails the same way, naming the variable.
`CheckAddr` never quotes the value: the likeliest wrong value is an AMQP
URI, whose login would otherwise reach the logs. With
`RABBITMQ_ADDR=amqp://orders:…@rabbit:5672/`:

```
init: config RABBITMQ_ADDR: is a URI, not host:port
```

`NewService` runs the same checks itself, so a `Settings` built in code with
a bad field fails `Init` too, naming every field at fault, such as
`rabbitmq: NewService: Settings.Queue must not be empty`. That error is no
`*config.Error`, so the App exits 1: the fault is in the code, not in the
configuration. [How a bad value fails](/humus/pr-preview/pr-1185/configuration/#how-a-bad-value-fails)
explains the exit codes.

### The `Delivery` setting

**`Delivery` is set in code.** Whether a message may be processed twice or
lost is a property of the `Processor`, not of a deployment, so it is the end
user's design decision rather than an operator's, and most `Init`s don't
source it. `DefaultSettings()` gives the safe one, `queue.AtLeastOnce`. An
`Init` that wants at-most-once sets `settings.Delivery = queue.AtMostOnce`
on the `Settings` it builds. A Service has one `Delivery`: two queues that
need different ones are
[two Services](#two-queues-two-services).

### TLS and plain text

`Connection.TLS` is the `*tls.Config` the broker is dialled with, and `nil`
means plain text: humus makes a service as secure as it can, but it can't
control what the service connects to. The Service runs the TLS handshake
itself over the connection `s.Host.Dial` returns, and an empty `ServerName`
defaults to `Addr`'s host, on a clone of the config, so the end user's
config is never changed. RabbitMQ serves AMQP over TLS on 5671 by default,
and plain text on 5672.

The end user decodes the certificates from files, read through `s.Host.FS`
so that a test reads them from its own directory and production from the
real disk, as [Reading a file](/humus/pr-preview/pr-1185/configuration/#reading-a-file)
describes. This chain reads the broker's CA from the PEM file
`RABBITMQ_TLS_CA_FILE` names, and the client's certificate chain and private
key from the one `RABBITMQ_TLS_CERT_FILE` names, and logs in with the client
certificate:

```go
// The CA's file is read first, and the client's certificate only once it has been.
k.Connection.TLS = r.Get(env("RABBITMQ_TLS_CA_FILE").Parse(readFile(s.Host.FS)).FlatMap(func(ca []byte) config.Value[*tls.Config] {
	return env("RABBITMQ_TLS_CERT_FILE").Parse(readFile(s.Host.FS)).Parse(clientTLS(ca))
}))
// The client certificate is the login: RabbitMQ's rabbitmq_auth_mechanism_ssl plugin takes it.
k.Connection.SASL = &amqp.ExternalAuth{}
```

`readFile` and `clientTLS` are the end user's own helpers, the same ones the
Kafka page shows under
[TLS and mTLS](/humus/pr-preview/pr-1185/kafka/#tls-and-mtls): `Connection.TLS` is a
plain `*tls.Config`, as Kafka's `Settings.TLS` is. A missing variable, a
missing file and a file that holds no certificate each fail as a config
error with exit 78, named after its variable. A broker whose certificate a
public CA signed needs no `RootCAs`, and a config with no `RootCAs` reads the
system's roots, which are an
[ambient leak](/humus/pr-preview/pr-1185/lifecycle/#ambient-leaks). A broker that
needs TLS but no client certificate takes a config with only `RootCAs`, and
a login of the next section.

### Logins

`Connection.SASL` is how the connection authenticates. It is amqp091's own
`amqp.Authentication`, so the end user picks one of amqp091's mechanisms and
supplies its credentials from wherever they keep them: humus decides nothing
about secrets.

- **`&amqp.PlainAuth{Username, Password}`** is RabbitMQ's usual login, as in
  [`Init`](#a-consumer). It sends the password over the connection, so it
  belongs over TLS outside a cluster's own network.
- **`&amqp.AMQPlainAuth{Username, Password}`** is RabbitMQ's older
  `AMQPLAIN` mechanism, for a broker that offers only it.
- **`&amqp.ExternalAuth{}`** logs in with the client certificate in
  `Connection.TLS`, as the chain above does, on a broker with the
  `rabbitmq_auth_mechanism_ssl` plugin enabled.

An OAuth 2 token is a `PlainAuth` password, sent when the connection opens;
refreshing one on an open connection, with `connection.update-secret`, is
out of scope. A broker that refuses the login ends the Service with exit 1, rather than the
Service retrying a password that will never be accepted, as
[Failures](#failures) lists.

### An AMQP URI

**humus takes no URL.** It builds none, and never calls amqp091's `Dial`,
`DialConfig` or `DialTLS`, which take one, so the password in `SASL` can't
reach a log in one. An end user who keeps an AMQP URI, such as
`amqps://orders:…@rabbit.internal/shop`, parses it with `amqp.ParseURI` in
their `Init` and fills the `Connection` from it. This parser does that, for
`config.Value.Parse`:

```go
// fromURI is a parser for config.Value.Parse that fills DefaultSettings'
// Connection from an AMQP URI, for an end user who keeps one. Its errors
// never quote the URI, which holds the password.
func fromURI(uri string) (rabbitmq.Connection, error) {
	u, err := amqp.ParseURI(uri)
	if err != nil {
		return rabbitmq.Connection{}, errors.New("is not an AMQP URI")
	}
	if u.Username == "guest" {
		// ParseURI fills in guest when the URI has no login, and guest is no login to deploy with.
		return rabbitmq.Connection{}, errors.New("has no login of its own")
	}
	c := rabbitmq.DefaultSettings().Connection
	c.Addr = net.JoinHostPort(u.Host, strconv.Itoa(u.Port)) // brackets an IPv6 host
	c.Vhost = u.Vhost
	c.SASL = u.PlainAuth()
	if u.Scheme == "amqps" {
		// A nil TLS would dial the TLS port, 5671 by default, in plain text.
		c.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return c, nil
}
```

and a chain sources the whole `Connection` from one variable with it:

```go
// The address, the vhost, the login and whether to dial TLS, from one variable.
k.Connection = r.Get(env("RABBITMQ_URI").Parse(fromURI))
```

`ParseURI` fills in `guest` and `guest` when the URI carries no login, so a
parser that requires one checks `u.Username` itself. An `amqps` URI's port
defaults to 5671, so it means `TLS` must be set: a nil `TLS` would dial a
TLS port in plain text. `ParseURI`'s own errors quote the URI, and with it
the password, so `fromURI` returns one of its own.

## Topology

**The Service and the Publisher declare nothing.** The queue, its exchange,
bindings, type, dead-lettering and delivery limit are the end user's
infrastructure, set up before the App starts, by RabbitMQ's definitions, an
operator, or the team that owns the exchange, as `queue/kafka` creates no
topics. The Service is purely a consumer: a queue that doesn't exist stops
it with `404 NOT_FOUND`, and the App exits 1. The Publisher publishes only to
exchanges that exist: a missing exchange fails a `Publish` with
`404 NOT_FOUND`.

Declaring them from `Settings` would grow into RabbitMQ's whole topology
API, queue types, arguments, exchanges and dead-lettering, and would put the
App in charge of something a deployment already owns.

The [mailer walkthrough](/humus/pr-preview/pr-1185/walkthroughs/mailer/topology/)
keeps its topology in a definitions file that the broker's team imports,
with its delays and limits in policies.

## Delivery

**The Service never consumes with auto-ack, even under `AtMostOnce`.**
RabbitMQ ignores the prefetch count of an auto-ack consumer and sends as
fast as TCP allows, and amqp091 buffers every delivery without a bound: a
backlog would fill the process's memory, and a crash or a rollout would lose
everything buffered, already acked. So every delivery is acked by the
Service, and `Prefetch` always bounds what it holds.

What each `Delivery` acks, and when:

- **`AtLeastOnce`:** a delivery is acked once `Process` has returned `nil`
  for it. A delivery whose processing failed, or was cut short by a crash or
  by the grace period running out, is delivered again.
- **`AtMostOnce`:** the Service takes a delivery and every delivery already
  buffered behind it, up to `Prefetch`, acks them with one `basic.ack` with
  `multiple` set, and waits for the reply to a `basic.qos` on the same
  channel. RabbitMQ handles a channel's methods in order, so the reply means
  it has the ack, which has no reply of its own. Only then does the Service
  hand the batch to its workers, and it processes all of it. A message is
  never processed twice: if the connection drops before the reply, the
  batch isn't processed, and the broker has either dropped it or requeues
  it. A crash loses at most one batch, and a clean shutdown loses nothing.

### A processing error

**Under `AtLeastOnce`, a processing error stops the Service:**

- the delivery is rejected with requeue, with `basic.reject`;
- the Service stops as on shutdown: the other workers finish their
  deliveries and ack them, and every delivery received but not processed is
  nacked with requeue, in one `basic.nack`;
- `Serve` returns an error that wraps the `Processor`'s and names the queue,
  and the message's ID when it has one, such as
  `rabbitmq: processing message 3 from orders: cannot process`, so the App
  exits 1, and the broker delivers the message again when it restarts.

**Under `AtMostOnce`, a processing error is logged at ERROR,** with the
queue and the message's ID, and processing goes on. The message was already
acked, so stopping wouldn't bring it back.

### The delivery limit

**A failure rejects the one delivery and nacks the rest** because RabbitMQ
counts a `basic.reject`, and a channel or connection that closes with a
delivery unacked, toward a quorum queue's delivery limit, and doesn't count
a `basic.nack`. So exactly the message that failed is counted, and a message
that was merely prefetched when another failed never is. A quorum queue
reports the count in each message's `x-delivery-count` header.

Which deliveries count toward the limit:

| What happened to the delivery | Counts |
|---|---|
| `Process` returned an error for it, under `AtLeastOnce` | yes: it was rejected |
| it was prefetched behind one that failed, or unprocessed at a clean shutdown | no: it was nacked |
| it was unacked when the grace period ran out, the process crashed, or the connection was lost | yes: its connection closed with it unacked |

A message that always fails crash-loops the pod until the queue's delivery
limit dead-letters or drops it: 20 deliveries on a quorum queue, by default
since RabbitMQ 4.0. A classic queue has no delivery limit, and the crash
loop is what shows the failure.

Which messages are processed again, by `Delivery`:

| | `AtLeastOnce` | `AtMostOnce` |
|---|---|---|
| `Process` returns an error | that message, when the App restarts. The other deliveries in flight finish and are acked. | none: the error is logged, and processing goes on |
| the process crashes, or panics | the deliveries in flight | none: the rest of the acked batch is lost |
| the connection is lost | the deliveries in flight, once the Service reconnects: they finish, but their acks fail | none: the acked batch is processed whole |
| SIGTERM | none: the deliveries in flight finish and are acked | none: the batch in flight finishes |
| the grace period runs out during a delivery | the deliveries in flight | the rest of the batch is lost |

What was received but never handed to the `Processor` is never processed
twice: the broker delivers it again, under either `Delivery`, and the
Service processes it then for the first time.

## Order and concurrency

**A queue delivers its messages to one consumer in order.** The Service
hands them, in that order, to `Concurrency` workers, each of which processes
one at a time, on the one channel it consumes on, so the Service has one
prefetch window and one stream of acks. With `Concurrency` 1, the default,
messages are processed in the order the queue delivered them.

Three things break that order, as for any RabbitMQ consumer: a redelivery,
more than one replica, and `Concurrency` above 1. A `Processor` that needs
the messages of a key processed in order uses a
[single-active-consumer](https://www.rabbitmq.com/docs/consumers#single-active-consumer)
queue, which is topology the end user sets up, and `Concurrency` 1: the
broker then delivers to one replica at a time, and fails over to another
when it goes. A `Processor` that doesn't care about order raises
`Concurrency`, up to `Prefetch`, to process more at once.

## Failures

No panic is recovered: a panicking `Processor` crashes the process. Every
other failure follows this table:

| | `AtLeastOnce` | `AtMostOnce` |
|---|---|---|
| `Process` returns an error | rejected, which counts toward the delivery limit; the Service stops as on shutdown, and `Serve` returns the error, naming the queue and the message's ID; exit 1 | already acked; logged at ERROR, with the queue and the message's ID; processing goes on |
| `Process` panics | the process crashes; the broker requeues every unacked delivery, and each counts | the process crashes; the batch acked is lost |
| the broker is unreachable | the Service keeps trying to connect, logging each attempt at WARN | the same |
| the broker refuses the login, the vhost or the queue | the Service stops, and `Serve` returns the error; exit 1 | the same |
| the connection is lost after consuming started | the deliveries in flight finish, their acks fail, and the broker delivers them again; the Service logs it at WARN and reconnects | the same; a batch whose ack was confirmed is processed |
| the broker closes the channel with `406 PRECONDITION_FAILED` | the Service stops, and `Serve` returns the error; exit 1 | the same |

The login or the vhost is refused with `ACCESS_REFUSED`, or amqp091's
`ErrCredentials`, `ErrSASL` or `ErrVhost`. The queue is refused with
`404 NOT_FOUND`, `403 ACCESS_REFUSED`, or `405 RESOURCE_LOCKED` for a queue
another connection consumes exclusively. An error that ends the Service is
returned rather than logged, and the lifecycle logs it.

Stopping is loud on purpose, as for Kafka. Retrying in place would hold the
delivery with no limit, and skipping would lose it. A message that always
fails shows up as a crash loop with its error in the logs, until the
delivery limit takes it out of the queue.

### Dead letters

**Dead letters are the queue's or the `Processor`'s.** humus has no
dead-letter exchange, retry queue or skip policy of its own, and the
contract has no way to dead-letter a message on purpose. There are two ways
to get one:

- **The queue's.** A quorum queue with a `delivery-limit` and a
  `dead-letter-exchange`, set by a policy or by its arguments, dead-letters a
  message once it has failed that many times, as
  [The delivery limit](#the-delivery-limit) counts them. The App needs
  nothing for it: each failure stops the Service, the pod restarts, and the
  last failure moves the message aside. That costs a restart per failure.
- **The `Processor`'s.** A `Processor` that would rather skip a message that
  always fails than stop on it publishes it to a dead-letter exchange with
  a [`Publisher`](#publishing), or stores it wherever it keeps dead letters,
  and returns `nil`. The message is then acked like any other. Under
  `AtLeastOnce` it is acked only once `Publish` has returned `nil`, which is
  once the broker has confirmed the dead letter, so a dead letter that fails
  stops the Service rather than losing the message.

Which messages are worth stopping for, and where the others go, is the end
user's decision.

The [mailer walkthrough](/humus/pr-preview/pr-1185/walkthroughs/mailer/retries/) uses
both: its `Processor` parks a request that failed for now in a delay queue
and dead-letters one that never will succeed, and its queue's delivery limit
dead-letters a request that stops the Service.

## Shutdown

A Service gets the lifecycle's [two signals](/humus/pr-preview/pr-1185/lifecycle/#service).
When `Serve`'s context is done, or `Shutdown` is called, the Service:

1. cancels its consumer, with `basic.cancel`, so the broker sends nothing
   more;
2. hands out no more deliveries, except the rest of an `AtMostOnce` batch it
   already acked;
3. waits for the deliveries in flight to finish, and acks them;
4. nacks with requeue, in one `basic.nack` with `multiple` set, every
   delivery it received but didn't process, which counts nothing against
   them;
5. closes the channel and the connection, and `Serve` returns `nil`.

**`Process`'s context is not cancelled when shutdown starts.** It keeps the
values of `Serve`'s context and carries the delivery's span, but not
`Serve`'s cancellation. So a `Processor` that honours its context doesn't
fail the delivery in flight on every SIGTERM, which under `AtLeastOnce`
would make every rollout end in exit 1.

**It is cancelled when the grace period runs out.** `Shutdown`'s context
carries the grace period as its deadline. When it is done before the
deliveries in flight are, `Shutdown` cancels `Process`'s context, closes the
connection at once, waits for `Serve` to return, and returns an error
wrapping its context's error, which the lifecycle reports as a
`*humus.GracePeriodError`, with exit 1. The broker then requeues every
delivery that wasn't acked, and each counts toward a quorum queue's delivery
limit.

## Connection loss

A connection is lost when the network drops it, when it is silent for 1.5
heartbeat intervals, or when the broker closes it. **A node that shuts down
closes every connection with `320 CONNECTION_FORCED`, so a rolling upgrade
of RabbitMQ loses every consumer's connection once per node.** The broker
cancelling the consumer, because its queue was deleted, is handled as a lost
connection too. When the connection or the channel closes, the Service:

1. lets the deliveries in flight finish. Their acks fail in the client, and
   the Service drops those errors: the broker already requeued the
   deliveries, and delivers them again;
2. hands out nothing more: what the consumer had buffered was requeued too,
   except the rest of an `AtMostOnce` batch whose ack was confirmed, which
   is processed;
3. logs the lost connection at WARN, with the AMQP reply code and reason
   when the broker gave one;
4. connects again, as [Connecting](#connecting) describes, with the same
   backoff, logging each failed attempt at WARN, and consumes again.

**So the Service rides out a rolling upgrade.** It connects to a node that
is up, or waits for one, and its pod neither restarts nor lands in
Kubernetes' crash-loop backoff, of up to five minutes once restarts repeat.
What a lost connection costs is redeliveries: every delivery that was unacked
is delivered again, with `Redelivered` set, and RabbitMQ counts each one
toward a quorum queue's delivery limit, as it does for any connection that
closes with deliveries unacked. A delivery is never processed twice by one
connection: the Service connects again only once the old one's deliveries
are done.

**Some errors aren't fixed by connecting again,** and stop the Service
instead, with `Serve` returning the error, so the App exits 1:

- the refusals of [Connecting](#connecting): a login, a vhost, or a queue
  the broker refuses, such as the `404 NOT_FOUND` the reconnect after a
  deleted queue meets;
- a channel the broker closes with `406 PRECONDITION_FAILED`, as it does for
  a delivery held unacked past its `consumer_timeout`, 30 minutes by
  default. Connecting again would only hand the same `Processor` the same
  delivery.

Shutdown while the Service connects again stops it at once, waiting for no
backoff, and `Serve` returns `nil`.

### Connecting

The Service and the Publisher connect the same way, in three steps, and
`ConnectTimeout` bounds all three together:

1. they dial `Connection.Addr` with the Setup's Host, so the broker is
   reached only through the Host;
2. with `TLS` set, they run the TLS handshake over that connection;
3. they run the AMQP handshake with `amqp.Open`, logging in with `SASL`,
   opening `Vhost` and proposing `Heartbeat`.

`Serve` makes its attempts to connect, and to consume on the connection,
with bedrock's [`retry`](https://pkg.go.dev/github.com/z5labs/bedrock/retry):
exponential backoff from 100ms up to 30s, with no budget and no limit on
attempts, so only `Serve`'s context ends them. A broker that is down at
startup is waited for. A wrong login fails with `ErrCredentials`, and a vhost
the login can't open with `ErrVhost`, and neither is retried.

amqp091's experimental recovery, `Config.Recovery` since v1.15.0, isn't
used. It is experimental, it redials through `DialConfig`'s URL, which humus
never builds, rather than through the Host, and it declares the topology
again, which is the end user's. The Service's reconnect is a short loop of
the same retry it connects with at startup, which humus tests.

## Readiness

Nothing routes traffic to a consumer, so its readiness only gates rollouts.
A login, vhost or queue the broker refuses already ends the Service, and the
pod's crash loop stops the rollout. A wrong address doesn't: the Service
keeps retrying, as it does through a broker's upgrade, and without a monitor
its pod would turn ready and replace a pod that consumed.

**A consumer is ready once it consumes.** `Consuming` returns a
`health.Monitor` that is unhealthy until the broker first answers the
Service's `basic.consume`, and healthy from then until `Serve` returns,
through any number of lost connections and reconnects. It goes in the
readiness of whatever serves the pod's probes. A consumer's pod has no
server of its own, so that is a probes server made with
`httphealth.NewService`, returned beside the consumer:

```go
probes, err := httphealth.NewService(s, probeSettings, health.Readiness(consumer.Consuming()))
if err != nil {
	return nil, err
}
return []humus.Service{consumer, probes}, nil
```

Health shows the [probes server](/humus/pr-preview/pr-1185/health/#a-consumers-readiness)
and [the manifest](/humus/pr-preview/pr-1185/health/#manifests) for a Kafka consumer,
and a RabbitMQ consumer's are the same but for the monitor. A new pod is then
held not ready until it has consumed from its queue once, so a rollout whose
new pods have a wrong broker address stalls, and is reported failed at its
progress deadline, rather than replacing every pod that consumes with pods
that don't. `Consuming` may be called in `Init`, before `Serve`, and its
monitor never returns an error.

**A lost connection after that doesn't make a consumer unready.** A broker
outage hits every pod at once, and taking them all out of service would
change nothing but block the rollout that might fix it.

**`Consuming` is for readiness only.** A liveness monitor would restart
every pod in a broker outage, and the kubelet's restart backoff, up to five
minutes by default, would then hold them down after the broker comes back.
The Service reconnects on its own, and the errors a restart would fix
already end `Serve`, as [Connection loss](#connection-loss) lists, so the
pod exits and is restarted without a probe.

## Publishing

A [`rabbitmq.Publisher`](https://pkg.go.dev/github.com/z5labs/humus/queue/rabbitmq#Publisher)
publishes to the broker, and returns once the broker has confirmed the
message. The end user's `Init` builds it with `rabbitmq.NewPublisher`, from
`rabbitmq.PublisherSettings`, and hands it to whatever publishes: a
`Processor` that publishes an event, as `orders` does, an HTTP handler, a
job. **It is not a Service.** It is called from the Services that do the
work, so it lives as long as they do and closes after them. One `Publisher`
is safe for concurrent use and serves every Service of the process.

### Building a Publisher

**`NewPublisher` belongs in `Init`, on a connection of its own.**
`NewPublisher` opens no connection. The first `Publish` connects, as
[Connecting](#connecting) describes, on a connection of the Publisher's own,
separate from every Service's. RabbitMQ applies TCP back-pressure to a
connection that publishes faster than the broker can take, and a consumer's
acks on that connection would wait behind it, so RabbitMQ advises publishing
and consuming on separate connections. Concurrent first calls connect once,
and after the connection is lost, the next `Publish` connects again.

`PublisherSettings.Connection` is a `Connection`, as the Service's is, so a
`Publisher` on the consumer's broker takes the consumer's, as
[`Init`](#a-consumer) does with its helper:

```go
// publisherOn is DefaultPublisherSettings with the consumer's
// Connection: the same broker and the same login, on a connection of the
// Publisher's own.
func publisherOn(c rabbitmq.Connection) rabbitmq.PublisherSettings {
	p := rabbitmq.DefaultPublisherSettings()
	p.Connection = c
	return p
}
```

A `Publisher` to another broker sources a `Connection` of its own, from
variables of its own. A process that publishes to two brokers builds two
`Publisher`s.

`PublisherSettings` follows the
[settings convention](/humus/pr-preview/pr-1185/configuration/#what-a-package-with-settings-exports),
like `Settings`: `rabbitmq.DefaultPublisherSettings()` gives the defaults,
with `DefaultSettings`' `Connection`, every field's zero value means zero,
and the package reads no variable of its own.

| Field | Default | Valid | Check |
|---|---|---|---|
| `Connection` | as in `Settings`; `Addr` and `SASL` required | as in `Settings` | as in `Settings` |
| `Channels` | 8 | 1 to 65535 | `CheckChannels` |
| `Timeout` | 10s | positive | `CheckTimeout` |
| `MaxAttempts` | 3 | 1 or more | `CheckMaxAttempts` |
| `BackoffBase` | 100ms | positive | `CheckBackoffBase` |
| `BackoffMax` | 5s | positive, and at least `BackoffBase` | `CheckBackoffMax`, and `NewPublisher` for the bound |
| `RetryBudget.MaxTokens` | 10 | 3 to `retry.TokenLimit` | `CheckRetryBudgetMaxTokens` |
| `RetryBudget.TokenRatio` | 0.1 | 0.001 to `retry.TokenLimit` | `CheckRetryBudgetTokenRatio` |
| `Breaker.Window` | 20 | 1 to `circuit.MaxCount` | `CheckBreakerWindow` |
| `Breaker.Probes` | 1 | 1 to `circuit.MaxCount` | `CheckBreakerProbes` |
| `Breaker.OpenFor` | 10s | positive | `CheckBreakerOpenFor` |
| `Breaker.ProbeTimeout` | 10s | positive | `CheckBreakerProbeTimeout` |
| `BreakerPolicy` | see [The circuit breaker](#the-circuit-breaker) | `Trip` and `Probe` set | none: `NewPublisher` checks it |

`NewPublisher` runs every check again, and fails naming each field at fault,
such as `rabbitmq: NewPublisher: PublisherSettings.Timeout must be positive`,
which exits 1, as a bad `Settings` given to `NewService` does. It builds the
breaker and the instruments, and registers the function that closes the
`Publisher`'s connection with `s.OnStop`.

### A call

`Publisher.Publish(ctx, m)` publishes one `rabbitmq.Publishing`. **It returns
`nil` once the broker has confirmed the message,** so a `Processor` that
publishes before it returns `nil` has its delivery acked only after the
published message is the broker's, which is what `AtLeastOnce` needs:

```go
// Returns once the broker has confirmed the event, so the order is acked only after.
return o.events.Publish(ctx, rabbitmq.Publishing{
	Exchange:    "orders",
	RoutingKey:  "order.accepted",
	MessageID:   m.MessageID,
	ContentType: m.ContentType,
	Body:        m.Body,
})
```

A `Publishing`'s zero value is persistent, with delivery mode 2, so a
message isn't lost on a broker restart because a field was left out.
`Publishing.Transient` makes it delivery mode 1. Every other field is passed
to the broker as it is, a field left `""` isn't sent, and the caller's
`Headers` map is never changed.

**`Publishing.UserID` is the one field the broker checks.** RabbitMQ
[validates a `user-id`](https://www.rabbitmq.com/docs/validated-user-id)
against the user the `Publisher`'s `Connection` logs in as, and refuses a
publish that names another with `406 PRECONDITION_FAILED`. It adds no
`user-id` of its own, so a message published without one reaches its
consumer with an empty `Message.UserID`. A consumer that trusts its
publishers by `UserID`, as [Failure injection](#failure-injection)
describes, needs each `Publish` to set it to that login, here `r.user`, the
user `r.pub` logs in as:

```go
return r.pub.Publish(ctx, rabbitmq.Publishing{
	Exchange:   "orders",
	RoutingKey: "accepted",
	MessageID:  m.MessageID,
	UserID:     r.user,
	Body:       m.Body,
})
```

Each channel the `Publisher` opens is in confirm mode and carries one
publish at a time. Each attempt of a `Publish` takes a channel, connecting
first if needed, publishes the message with `mandatory` set, so the broker
returns a message no queue is bound to receive rather than dropping it, and
waits for the broker's answer:

- **confirmed:** `Publish` returns `nil`;
- **returned:** `Publish` returns an error that matches
  `rabbitmq.ErrUnroutable`, with no retry, since another attempt would be
  routed the same way. A caller branches on it to fall back, for example to
  a dead-letter store;
- **a channel error,** such as `404 NOT_FOUND` for an exchange that doesn't
  exist, `403 ACCESS_REFUSED`, or `406 PRECONDITION_FAILED` for a `UserID`
  that isn't the user the `Publisher` logs in as: `Publish` returns the
  broker's `*amqp.Error`, with no retry, since the broker refused the
  message, and the channel is replaced;
- **nacked by the broker, a dropped connection, a failed dial or a failed
  handshake:** the broker didn't take the message, or its answer was lost,
  and another attempt, on a new channel or connection when the old one was
  lost, may well be confirmed. So another attempt follows, as
  [Retries](#timeout-retries-and-the-budget) describes;
- **the context's deadline, or the caller cancelling it:** no further
  attempt, and `Publish` returns an error wrapping the context's error.

**There is no `Classify`.** An HTTP client's `Classify` exists because each
API decides what its answers mean, but RabbitMQ's protocol fixes what a
confirm, a nack, a return and a channel error mean, so the table above is
fixed, and the `Publisher` has no `Classify` setting, as Kafka's `Producer`
has none.

`PublisherSettings.Channels` bounds the publishes in flight at once: an
attempt waits for a channel when every one is busy, and gives its channel
back before any wait for the next. When the context is done during an
attempt, the channel isn't reused, since a confirm may still arrive on it.

### `Timeout`, retries and the budget

**`Timeout`, 10s by default, bounds the whole `Publish`:** every attempt,
the waits between them and each confirm. A deadline the caller's context
already has wins when it is earlier. No attempt starts after it, and a wait
that would end after it isn't begun. Without the bound, a broker that has
stopped answering would hold a call, and the App's shutdown, until it
answered.

**A `Publish` makes up to `MaxAttempts` attempts, 3 by default.** After a
failure that may be retried, the next attempt waits for a time drawn from
bedrock's `retry.Exponential(BackoffBase, BackoffMax)`, 100ms doubling up to
5s, and `Publish` returns the last attempt's error. `MaxAttempts` 1 turns
retries off.

**The retries of every `Publish` of a `Publisher` share one budget,**
`RetryBudget`, as
[`retry.Budget`](https://pkg.go.dev/github.com/z5labs/bedrock/retry#Budget)
describes. Retries add load to a broker when it can least take it, so each
failure that would be retried takes a token, each confirm adds back
`TokenRatio` of one, and no attempt follows a failure while half the tokens
or fewer are left. While the broker keeps failing, each `Publish` makes one
attempt rather than `MaxAttempts`. A first attempt is never refused.

**Publishing is at least once.** When a connection is lost after the broker
took the message but before its confirm arrived, the attempt that follows
publishes it again. So a consumer of the message is idempotent anyway, under
`queue`'s contract, and tells a duplicate apart by its ID, as the `orders`
`Processor` publishes its events with the order's ID as their
`MessageID`.

### The circuit breaker

**A circuit breaker guards the broker,** so that while the broker keeps
failing, `Publish` fails at once rather than hold every caller for
`Timeout`. Without it, an HTTP handler that publishes an event would hold
its request for 10s during an outage before it answered 503. With it, the
first few attempts wait, and the rest fail at once until a probe gets
through. It is bedrock's
[`circuit.Breaker`](https://pkg.go.dev/github.com/z5labs/bedrock/circuit),
one per `Publisher`: one `Publisher` is one connection to one broker.

**The breaker sits inside the retries:** each attempt of a `Publish` is one
call through it. While it is open, an attempt sends nothing and fails at
once with an error wrapping a `*circuit.RejectedError`, which matches
`circuit.ErrRejected` and says how much longer the breaker stays open in its
`RetryAfter`, and no attempt follows it, since the breaker would reject that
one too. Whether to wait and try again is the caller's.

The breaker counts the broker, not the messages or the callers, so what an
attempt's result counts as is fixed too:

- **a success:** confirmed; or returned, `ErrUnroutable`, or a channel
  error, such as `404 NOT_FOUND`, `403 ACCESS_REFUSED` or
  `406 PRECONDITION_FAILED`, since the broker answered. A mistake in one
  exchange's bindings, or in one message's `UserID`, doesn't open the
  breaker for the others;
- **a failure:** nacked by the broker, a dropped connection, a failed dial
  or a failed handshake; or the context's deadline, `Timeout`'s or the
  caller's, since a broker that has stopped answering ends every attempt
  this way;
- **not counted:** the caller cancelling the context, which says nothing
  about the broker.

**The defaults:** the breaker opens after 5 failed attempts in a row, or
once half of the last 20 or more, `Breaker.Window`, failed. It stays open
for `Breaker.OpenFor`, 10s, and then turns half-open: it lets
`Breaker.Probes` attempts through at once, 1, as probes, each holding its
slot for at most `Breaker.ProbeTimeout`, 10s, which matches `Timeout`. It
opens again on a failed probe, and closes after 3 confirmed ones. `Breaker`
is bedrock's own `circuit.Config`, with a check for each field, so an
operator's bad value fails startup naming its variable, with exit 78.

**`BreakerPolicy` is set in code.** It is a `circuit.Policy`, two functions,
`Trip` and `Probe`, so it can't come from a variable. bedrock ships no
policy, so `DefaultPublisherSettings` holds humus's, and the end user
replaces it in their `Init`. A policy whose `Trip` always returns `false`
turns the breaker off, as on the Kafka page's
[circuit breaker](/humus/pr-preview/pr-1185/kafka/#the-circuit-breaker).

Each change of the breaker's state is a log record, as
[The logs](#the-logs) lists. The `Publisher` measures the breaker and the
retries too, with four instruments of humus's own: the breaker's state, its
changes of state, the attempts it counted or rejected, and what `retry`
decided after each attempt, as
[humus's own instruments](#humuss-own-instruments) lists. A rejected
`Publish` is also an error on its span.

### Shutdown and the grace period

**The `Publisher` outlives every Service that uses it.** Its connection is
closed by the function `NewPublisher` registers with `s.OnStop`, which runs
once every Service has stopped, so a `Processor` finishing its delivery
during the drain can still publish. On SIGTERM, a `Process` blocked in
`Publish` keeps its context, as [Shutdown](#shutdown) describes, so its
event is confirmed, and its delivery acked, as the Service drains. The
closing function closes the connection within the grace period, and an error
that the grace period cut it short is reported as a
`*humus.GracePeriodError`. A `Publish` after it has run, from a goroutine the
end user left running, fails at once.

If the end user's `Init` fails after `NewPublisher`, the lifecycle runs the
`OnStop` functions registered so far, so no connection outlives the App.

## The defaults

The consumer runs on `humus.Main`'s [defaults](/humus/pr-preview/pr-1185/lifecycle/#the-defaults),
which are a consumer's, so `queue/rabbitmq` has no `Main` and no defaults of
its own:

- **A grace period of 20s.** A `Processor` that can't finish its delivery in
  flight within it is cut short, and under `AtLeastOnce` the delivery is
  processed again. A `Publish` in flight at SIGTERM fits in it: its
  `Timeout` is 10s.
- **No drain.** Nothing load-balances a consumer. Its traffic stops when it
  cancels its consumer, which it does as soon as shutdown starts. The drain
  would only keep a consumer that has already stopped waiting.

### `terminationGracePeriodSeconds`

When Kubernetes stops a pod, it sends the container SIGTERM, and then
SIGKILL once `terminationGracePeriodSeconds` have passed, 30 by default. On
`humus.Main`'s defaults, the worst case from SIGTERM to the exit is the 20s
grace period and the telemetry's shutdown,
`telemetry.Settings.ShutdownTimeout`, 5s: 25s, inside Kubernetes' 30s, with a
margin for the process to exit.

A consumer that raises its grace period raises
`terminationGracePeriodSeconds` with it, to at least the grace period plus
the telemetry's shutdown plus that margin. Otherwise SIGKILL ends the
process first: its container exits 137, nothing in flight is acked, the
broker requeues it all, counting each delivery toward the delivery limit,
and the telemetry not yet exported is lost.

An operator changes the grace period with `HUMUS_SHUTDOWN_GRACE_PERIOD`, and
adds a drain with `HUMUS_SHUTDOWN_DRAIN_DELAY`, without a rebuild, as for any
App. [The lifecycle's settings](/humus/pr-preview/pr-1185/lifecycle/#settings)
describes both variables.

### Two queues, two Services

A Service consumes one queue, with one `Delivery`. Two queues, or two
`Delivery` values, are two Services, each built by `rabbitmq.NewService`
from `Settings` of its own, returned together from one `Init`. Each has its
own channel and its own acks, so an at-least-once failure stops only its own
consumer, but either Service returning ends the App, so the other stops too,
after its deliveries in flight. One `Publisher` serves both.

### Which `Main`

Which `Main` the process uses depends on what a load balancer routes to, as
[the lifecycle's runtime packages](/humus/pr-preview/pr-1185/lifecycle/#runtime-packages)
explains for mixed processes:

- **On its own, or next to a probes or metrics server,** a consumer stays on
  `humus.Main`. Nothing routes to that server, and a drain would only delay
  the exit.
- **Next to a server a load balancer routes to,** such as an API, the
  process uses that server's package's `Main`: `httpserver.Main` for
  [an HTTP server](/humus/pr-preview/pr-1185/http/#the-defaults), with a 5s drain and a
  10s grace period, or `grpcserver.Main` for
  [a gRPC server](/humus/pr-preview/pr-1185/grpc/#the-defaults), with a 5s drain and a
  15s grace period. The consumer still cancels its consumer as soon as
  shutdown starts, and its deliveries in flight then have the drain and that
  package's grace period to finish, rather than `humus.Main`'s 20s.

## Telemetry

The Service and the `Publisher` report through the Setup's telemetry,
`s.Telemetry`, and never through OpenTelemetry's globals: their tracers and
meters come from the SDK's `TracerProvider` and `MeterProvider`, under the
scope `github.com/z5labs/humus/queue/rabbitmq`, and their propagator is the
SDK's `Propagator`. Their names and attributes are OpenTelemetry's
[semantic conventions for RabbitMQ](https://github.com/open-telemetry/semantic-conventions/blob/v1.43.0/docs/messaging/rabbitmq.md),
as semconv v1.43.0 has them. The messaging conventions are still in
development, so humus pins the version it uses, and a change to these names
is a breaking change to `queue/rabbitmq`.

### The process span

Each delivery is processed under a span of its own, named `process <queue>`,
such as `process orders`, of kind `CONSUMER`. `Process` is given a context
that carries it, so every span the `Processor` starts from that context is
its child, and every record it logs with that context carries its trace ID.

**Its parent is the publisher's.** The span's parent is the trace context
the propagator extracts from the message's headers, so a publisher's trace
continues through the consumer, and the publisher's baggage, and only the
publisher's, is on `Process`'s context too. Only a header whose value is a
string is read: a `traceparent` header of any other type, such as bytes, is
ignored. A message whose headers carry no trace context starts a new trace.

Its attributes are:

- `messaging.system` (`rabbitmq`), and `messaging.operation.type` and
  `messaging.operation.name`, both `process`;
- `messaging.destination.name`, as semconv defines it for RabbitMQ:
  `{exchange}:{routing key}:{queue}`, leaving out what is empty, and the
  routing key when it equals the queue's name. So a message published to
  the queue `orders` on the default exchange is `orders`, and one published
  to the exchange `shop` with the routing key `order.created` is
  `shop:order.created:orders`;
- `messaging.rabbitmq.destination.routing_key`, when the routing key isn't
  empty, and `messaging.rabbitmq.message.delivery_tag`;
- `messaging.message.id`, and `messaging.message.conversation_id`, from the
  correlation ID, when they are set;
- `server.address` and `server.port`, from `Connection.Addr`.

The message's body is never recorded. When `Process` returns an error, under
either `Delivery`, the error is recorded on the span as an exception event,
with `error.type` and an error status.

### The send span

**Each `Publish` has a span of its own,** a child of the span on the
caller's context, named `send <exchange>`, or `send amq.default` for the
default exchange, of kind `PRODUCER`. A `Publish` has one span, whatever its
attempts. Its attributes are `messaging.system` (`rabbitmq`),
`messaging.operation.type` and `messaging.operation.name` (both `send`),
`messaging.destination.name`, as semconv defines it for a publish,
`{exchange}:{routing key}`, leaving out what is empty, so `orders:order.accepted`
for the `orders` `Processor`'s events,
`messaging.rabbitmq.destination.routing_key` when the routing key isn't
empty, `messaging.message.id` and `messaging.message.conversation_id` when
they are set, and `server.address` and `server.port`. The body is never
recorded. A failed `Publish` is recorded on its span as an exception event,
with `error.type` and an error status, a `Publish` the breaker rejected
included.

**The span's context goes into the message's headers.** The span is
started before the headers are written, and the Setup's propagator injects
its trace context, and the caller's baggage, into a copy of the headers:
with the default propagator, `traceparent`, `tracestate` and `baggage`. A
nil `Headers` map gets a new one. So the `process` span of the Service that
consumes the message is a child of its `send` span, and inside a `Process`,
the `send` span is a child of the `process` span: one trace runs from the
first publisher through every queue a message passes through, without the
end user's code touching the headers.

### The instruments

The Service records two instruments of OpenTelemetry's messaging
conventions:

| Instrument | Kind | What it records |
|---|---|---|
| `messaging.client.consumed.messages` | counter | the deliveries handed to the `Processor` |
| `messaging.process.duration` | histogram, in seconds | how long each `Process` took, with `error.type` when it failed |

Each has `messaging.system`, `messaging.operation.name` (`process`),
`messaging.destination.name`, `server.address` and `server.port`. The broker
sends no reply to an ack, so settling a delivery has no duration, and the
Service records no `messaging.client.operation.duration`.

The `Publisher` records two more, for each attempt that publishes:

| Instrument | Kind | What it records |
|---|---|---|
| `messaging.client.sent.messages` | counter | the messages published, with `error.type` when the attempt failed |
| `messaging.client.operation.duration` | histogram, in seconds | how long the broker took to answer each, from the publish to its confirm, with `error.type` when the attempt failed |

Each has `messaging.system`, `messaging.operation.name` (`send`),
`messaging.destination.name`, `server.address` and `server.port`. A return
and a channel error have the broker's AMQP reply code as `error.type`, such
as `312` for a return and `404` for an exchange that doesn't exist, and a
nack has `nacked`. A nack followed by a confirm counts two messages sent, one
with `error.type` `nacked`. An attempt that fails before it publishes,
waiting for a channel or connecting, publishes nothing, so neither
instrument records it, and neither does an attempt the breaker rejected;
`humus.circuit.calls` counts that one as `rejected`.

**A routing key that carries identifiers makes a series per key.** semconv's
destination name includes the routing key, so each routing key a message
arrives or leaves with is a series of its own in the instruments. A
publisher that puts identifiers, such as an order's ID, in its routing keys
gives every consumer a series per key, which a metrics backend may struggle
with; such identifiers belong in headers, or in the body, instead.

### humus's own instruments

The `Publisher` records four instruments of humus's own for its
[breaker](#the-circuit-breaker) and its
[retries](#timeout-retries-and-the-budget), with the Setup's
`MeterProvider`, in the scope `github.com/z5labs/humus/queue/rabbitmq`,
beside its messaging instruments:

| Instrument | Kind | Unit | Attributes | Recorded |
|---|---|---|---|---|
| `humus.circuit.state` | asynchronous UpDownCounter | `{breaker}` | `humus.circuit.state`: `closed`, `open` or `half-open` | at each collection: 1 for the breaker's state, 0 for the other two |
| `humus.circuit.transitions` | Counter | `{transition}` | `humus.circuit.state`, the state it went to, and `humus.circuit.previous_state`, the one it left | at each change of the breaker's state, beside its log record |
| `humus.circuit.calls` | Counter | `{call}` | `humus.circuit.outcome`: `success`, `failure`, `ignored` or `rejected` | once an attempt has its result, or as the breaker rejects it |
| `humus.retry.decisions` | Counter | `{decision}` | `humus.retry.decision`: `again`, `succeeded`, `stopped`, `exhausted`, `throttled` or `expired` | after each attempt, with what `retry` decided |

Every data point also carries `server.address` and `server.port`, from
`Connection.Addr`, the same as the breaker's log records, and nothing per
`Publish`, such as an exchange or a routing key: the messaging instruments
break publishes down already. Each other attribute's value is the `String()`
of bedrock's own type, `circuit.State`, `circuit.Outcome` or
`retry.Decision`.

- **`humus.circuit.state` is read, not tracked.** At each collection it reads
  the breaker's state once, so it shows `half-open` as soon as `OpenFor` has
  passed, with no `Publish` made. Two `Publisher`s of one broker add up: one
  breaker open and one closed read `closed` 1 and `open` 1. The App's last
  collection, after the `OnStop` functions, still reads it, so the final
  export has the state the App stopped in.
- **`humus.circuit.transitions` catches the short opens.** A breaker can
  open, go half-open and close between two collections, a minute apart by
  default, and `humus.circuit.state` never shows it open.
- **`humus.circuit.calls` counts an attempt when the breaker does.** One call
  is one attempt, so a `Publish` retried twice is three calls. An attempt the
  breaker rejects is `rejected`: it sent nothing, so it is in neither
  messaging instrument.
- **`humus.retry.decisions` says why a `Publish` stopped trying.** A
  `throttled` decision is a retry the `RetryBudget` refused, and an
  `exhausted` one is a `Publish` that used every one of `MaxAttempts`. An
  attempt the breaker rejected is `stopped`, as `retry` decides for it.

The Service's connection retries record none of them: they aren't on the
request path, and each failed connect is already a `connecting failed`
record.

In Prometheus, they are `humus_circuit_state`,
`humus_circuit_transitions_total`, `humus_circuit_calls_total` and
`humus_retry_decisions_total`, and each attribute is a label such as
`humus_circuit_state`. An alert on an open breaker reads the state, and one
on retries the budget throttles reads the decisions:

```promql
# A breaker is open now.
humus_circuit_state{humus_circuit_state="open"} > 0

# The retry budget refused retries in the last 5 minutes.
increase(humus_retry_decisions_total{humus_retry_decision="throttled"}[5m]) > 0
```

A View in the end user's telemetry `Builder` drops or renames any of them.
humus has no setting to turn them off. A test reads them as it reads any
metric, with `humustest.Telemetry`'s `Metrics`, filtered to the scope.
[humus's own instruments](/humus/pr-preview/pr-1185/telemetry/#humuss-own-instruments)
on the Telemetry page lists what each of humus's clients records, with the
View and the test written out.

### The logs

The Service logs through the Setup's `LoggerProvider`, under its scope:

| Record | Severity | When |
|---|---|---|
| `consuming` | INFO | the broker answered the `basic.consume`, on each connection. It carries the queue as `messaging.destination.name`. |
| `connecting failed` | WARN | an attempt to connect, or to consume on the connection, failed, and the Service will try again. It carries the error. |
| `lost the connection` | WARN | the connection or the channel closed after consuming started. It carries the AMQP reply code as `error.type` and its reason as `exception.message` when the broker gave one. |
| `processing a message failed` | ERROR | `Process` returned an error under `AtMostOnce`. It carries the delivery's span's context, the queue, the message's ID, and the error as `exception.message` and `error.type`. |

Every record carries `messaging.system`, `server.address` and
`server.port`. An error that ends the Service is returned, not logged here:
the lifecycle logs it.

The `Publisher` logs under the same scope, one record for each change of its
breaker's state, in the context of the `Publish` that made it, so it carries
that call's trace ID:

| Record | Severity | When |
|---|---|---|
| `circuit breaker open` | WARN | the [breaker](#the-circuit-breaker) opened, from closed or from half-open |
| `circuit breaker half-open` | INFO | the breaker's `OpenFor` passed, and it lets probes through |
| `circuit breaker closed` | INFO | enough probes were confirmed |

Their attributes are `server.address` and `server.port`, the state the
breaker left as `humus.circuit.previous_state`, and the counts of that state:
`humus.circuit.successes`, `humus.circuit.failures`,
`humus.circuit.consecutive_successes`, `humus.circuit.consecutive_failures`
and `humus.circuit.timed_out`. The bodies and the attributes are those of
humus's other clients' breaker records, so one query finds every breaker.
So an alert on `circuit breaker open` says the broker kept failing, and the
counts say how; an alert on a metric, `humus.circuit.state`, is in
[humus's own instruments](#humuss-own-instruments).

### amqp091's logger

**amqp091 logs through a package-level variable,** `amqp.Logger`, which
`amqp.SetLogger` replaces, and which logs nothing by default. humus never
sets it: replacing it is process-wide state, which every App and every test
in the process would share, so it is on the lifecycle's
[ambient leaks](/humus/pr-preview/pr-1185/lifecycle/#ambient-leaks), and humus's lint
forbids `SetLogger` in its own code. Left alone, amqp091's few messages,
such as a failure to answer the broker's close of a channel, are dropped,
and most of what it would log comes from its experimental recovery, which
humus doesn't use. What the Service and the `Publisher` need to say is in
their own records, above. An end user who wants amqp091's messages anyway
sets the logger in `main`, once, knowing it reaches past the Setup's
`LoggerProvider` to whatever the logger writes to.

## Failure injection

A Publisher is an injection point per destination,
`rabbitmq:<exchange>:<routing key>`, such as `rabbitmq:orders:accepted`: a
request or job whose baggage carries a failure scenario can make its
publishes to one destination fail or slow down. An abort fails the attempt
with nothing sent, marked as a lost connection's error is, so it is retried
and the breaker counts it, as for a broker that is down. The Publisher
strips the scenario from the message's headers, unless
`PublisherSettings.ForwardScenario` is set. A Service strips the scenario a
delivery carries, before `Process` sees it, unless `Settings.TrustScenario`
trusts the delivery, such as by its `UserID`, which RabbitMQ validates
against the publishing connection's user. When it is nil, the default, no
delivery is trusted. The `user-id` comes only from the publisher, so a
scenario a humus service forwards is trusted by `UserID` only when its
`Publish` sets `Publishing.UserID` to the user its `Publisher` logs in as,
as [A call](#a-call) shows. Without `humus.WithFaults`, nothing is evaluated or
stripped. [Failure injection](/humus/pr-preview/pr-1185/failure-injection/) explains
the model, whom to trust and how, and how to test a scenario.

## Testing

**humus ships no fake RabbitMQ broker, and no test package.** humus is
neither RabbitMQ's client nor its server, so it doesn't write or keep a
broker of its own: a fake's fidelity would be humus's to maintain against
every RabbitMQ release, and a test on it would show what the fake does
rather than what RabbitMQ does. No in-memory RabbitMQ exists for Go either.
So an App that uses RabbitMQ is tested in two ways:

- **its `Processor`, as a unit,** inside `testing/synctest`, with no broker;
- **the whole App, against RabbitMQ itself in a container,** in real time.

That second way departs from the charter's principle 6, which runs every App
whole inside `synctest` on a test Host: an App that uses RabbitMQ can't, and
the charter's
[revision 10](https://github.com/z5labs/humus/discussions/629#discussioncomment-19009924)
records the exception.
[Testing a humus App](/humus/pr-preview/pr-1185/testing/#runtime-packages) says the
same for every runtime package.

The [mailer walkthrough](/humus/pr-preview/pr-1185/walkthroughs/mailer/testing/)
tests its `Processor` both ways, and runs the same black-box cases against
the App in process and against its deployment on kind.

### A Processor as a unit

A `queue.Processor[rabbitmq.Message]` is a function of a plain struct, so
service code is tested by calling it with `rabbitmq.Message` values, inside
`synctest`, with its other dependencies, such as HTTP, gRPC or Kafka, on
humustest's Host. Service code that publishes takes an interface of its own
over `Publish`, as `orders` takes [`events`](#the-contract), which its unit
tests fake:

```go
// fakeEvents is the orders Processor's events in its unit tests: it records
// what is published, and fails each Publish with err while err is set.
type fakeEvents struct {
	mu        sync.Mutex
	published []rabbitmq.Publishing
	err       error
}

func (f *fakeEvents) Publish(_ context.Context, m rabbitmq.Publishing) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.published = append(f.published, m)
	return nil
}
```

This test delivers an order twice, the second time as a redelivery, and
checks that it was stored once and that each delivery published its event:

```go
func TestOrders_stores_an_order_once_and_publishes_its_event_on_each_delivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &fakeEvents{}
		h := &orders{stored: map[string][]byte{}, events: ev}
		m := rabbitmq.Message{Queue: "orders", MessageID: "order-1", ContentType: "application/json", Body: []byte(`{"total":42}`)}

		require.NoError(t, h.Handle(t.Context(), m))
		m.Redelivered = true
		require.NoError(t, h.Handle(t.Context(), m), "a redelivery is no error")

		require.Equal(t, map[string][]byte{"order-1": []byte(`{"total":42}`)}, h.stored, "stored once")
		require.Len(t, ev.published, 2, "an event on each delivery")
		for _, p := range ev.published {
			require.Equal(t, rabbitmq.Publishing{
				Exchange:    "orders",
				RoutingKey:  "order.accepted",
				MessageID:   "order-1",
				ContentType: "application/json",
				Body:        []byte(`{"total":42}`),
			}, p)
		}
	})
}
```

A `Message` is a plain struct, so a test builds whatever delivery it needs:
`Redelivered` set, headers carrying a `traceparent`, a body that isn't JSON.
A fake that returns an error matching `rabbitmq.ErrUnroutable` shows what the
`Processor` does when no queue is bound to receive its event, and one that
returns a `*circuit.RejectedError` what it does while the broker is down.

`Init` runs inside `synctest` too, as far as it goes without a broker: with
its variables unset it exits 78, naming each one, and with nothing
listening at its address it waits for the broker and exits 0 on SIGTERM.

### The whole App, against a container

**The whole App runs against RabbitMQ in a container.** Its test runs the
App with `humustest.Start` and `humus.WithHost(humus.OSHost())`, so the
Service dials the container through the real network, in real time, and
the App still reads its environment from humustest's process. This test runs
an `Init` that sources its `Settings` as [`Init`](#a-consumer) does, with a
`Processor` that fails on one message, and shows the failure and the
redelivery after it, with what a quorum queue counted:

```go
func TestInit_at_least_once_stops_on_a_failure_and_redelivers(t *testing.T) {
	t.Parallel()

	// The queue is the end user's infrastructure, so the test declares it.
	q := quorumQueue(t)
	publish(t, q, ids(5)...)

	b := rabbit(t) // RabbitMQ in a container, with a login generated for this run
	bad := errors.New("cannot process")
	run := func(fail bool) (bedrock.ExitStatus, []rabbitmq.Message) {
		var got recorder
		p, status := humustest.Start(t, initWith(func(_ context.Context, m rabbitmq.Message) error {
			got.add(m)
			if fail && m.MessageID == "3" {
				return bad
			}
			return nil
		}),
			humustest.WithApp(humus.WithHost(humus.OSHost())),
			humustest.WithEnv(map[string]string{
				"RABBITMQ_ADDR":     net.JoinHostPort(b.host, b.amqp),
				"RABBITMQ_USERNAME": b.username,
				"RABBITMQ_PASSWORD": b.password,
				"RABBITMQ_QUEUE":    q,
			}),
		)
		if !fail {
			eventually(t, func() bool { return len(got.ids()) == 2 }, "the redelivered messages were never processed")
			p.Signal(syscall.SIGTERM) // the drain acks what is in flight
		}
		return exit(t, status), got.messages()
	}

	st, seen := run(true)
	require.Equal(t, humus.ExitFailure, st.Code())
	require.ErrorIs(t, st.Cause(), bad)
	require.ErrorContains(t, st.Cause(), q)
	require.ErrorContains(t, st.Cause(), "message 3")
	require.Equal(t, []string{"0", "1", "2", "3"}, idsOf(seen))

	st, seen = run(false)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.ElementsMatch(t, []string{"3", "4"}, idsOf(seen))
	for _, m := range seen {
		switch m.MessageID {
		case "3":
			require.Equal(t, int64(1), deliveryCount(t, m.Headers), "3 was rejected, which counts once")
			require.True(t, m.Redelivered)
		case "4":
			require.Zero(t, deliveryCount(t, m.Headers), "4 was nacked, which counts nothing")
		}
	}
}
```

`initWith`, `recorder`, `eventually`, `exit`, `idsOf` and `deliveryCount`
are the test's own helpers, a few lines each: `initWith` is the end user's
`Init` with the given `Processor`, and `deliveryCount` reads a message's
`x-delivery-count` header. The first run fails on message 3, so the App exits
1 naming the queue and the message, and the broker has 3 and 4 ready again.
The second run processes them, and the counts show that 3, which was
rejected, counted once toward the delivery limit, and 4, which was nacked
behind it, didn't.

### Declaring the test's queues

**The test declares its own queues,** since the Service declares none, over
an amqp091 connection of its own, as the end user's infrastructure would in
production. A queue of its own, under a name no other test uses, lets the
tests run in parallel against one broker, and is deleted when the test
ends:

```go
// declare declares a durable queue with args, under a name of the test's
// own, and returns the name. The queue is deleted when the test ends.
func declare(t *testing.T, args amqp.Table) string {
	t.Helper()
	ch := adminChannel(t)
	name := unique("orders")
	// RabbitMQ 4 refuses transient queues that aren't exclusive.
	_, err := ch.QueueDeclare(name, true, false, false, false, args)
	require.NoError(t, err)
	t.Cleanup(func() {
		// On a connection of its own: a test may have closed every
		// connection.
		del := adminChannel(t)
		_, _ = del.QueueDelete(name, false, false, false)
	})
	return name
}
```

A test of the delivery limit declares a quorum queue, with
`amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeQuorum}`, and one of a
dead-letter exchange declares the exchange and the queue's
`x-dead-letter-exchange` and `x-delivery-limit` arguments too. A test of the
`Publisher` declares the exchange it publishes to and binds a queue to it,
which the test then reads.

### A login generated per test

**The broker's login is generated for each run, never the image's
`guest`.** The test starts the official `rabbitmq` image, pinned to an exact
release, with testcontainers' RabbitMQ module, and gives it a login made
with `crypto/rand.Text`:

```go
broker.username = "humus-" + strings.ToLower(rand.Text())
broker.password = rand.Text()
```

`guest` is the login every deployed broker should refuse, so a test that
passes with it says nothing about the login the App is given, and a fixed
password in a repository is one more secret to scan for, even in a test. A test
of TLS issues the broker's certificate from `humustest.NewCA`, writes it to
a temporary directory, and copies it into the container, so there are no
fixed certificates either.

### Behind a build tag, in a CI job of its own

**Tests that start a container sit behind a build tag,** so `go test ./...`
runs the unit tests alone, with no Docker, and they open real sockets
nowhere else. `queue/rabbitmq`'s own run behind `testcontainers`, starting
with:

```
//go:build testcontainers
```

and run in a CI job of their own, "RabbitMQ (testcontainers)":

```
go -C queue/rabbitmq test -race -tags testcontainers ./...
```

The job isn't a required check, and the coverage gate counts the unit tests
alone: a container test is slower, and depends on Docker and on pulling an
image, so it informs a review rather than blocking one. The build job vets
the tagged tests without starting a container, so they keep compiling. An
end user's own container tests fit the same shape: a tag of their own, a job
of their own, and every test that doesn't need a broker outside the tag.

### The Service contract

`queue/rabbitmq`'s own tests hold `rabbitmq.Service` to the Service contract
with [`humustest.TestService`](/humus/pr-preview/pr-1185/testing/#checking-a-service),
with nothing listening at `Addr`, so it shuts down as the lifecycle expects
while it waits for a broker:

```go
func TestService_passes_the_Service_contract_with_no_broker(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		svc, err := rabbitmq.NewService(s, settings(), queue.ProcessorFunc[rabbitmq.Message](func(context.Context, rabbitmq.Message) error {
			return nil
		}))
		require.NoError(t, err)
		return svc
	})
}
```

A Service of the end user's own that wraps a consumer is checked the same
way.

## Migration from amqp091-go

humus had no RabbitMQ package before the reset. A consumer written on
amqp091-go directly moves over like this:

| On amqp091-go | With `queue/rabbitmq` |
|---|---|
| `amqp.Dial(os.Getenv("AMQP_URL"))` | `Settings.Connection`, sourced in `Init`, dialled through `s.Host`; a URI parsed with `amqp.ParseURI`, as [An AMQP URI](#an-amqp-uri) shows |
| `ch.Qos(n, 0, false)` | `Settings.Prefetch` |
| `ch.Consume(q, "", autoAck, ...)` and a loop over the deliveries | `Settings.Queue`, `Settings.Delivery`, and a `queue.Processor[rabbitmq.Message]` |
| `d.Ack(false)`, `d.Nack(false, true)` | the Service, as `Delivery` says |
| `conn.NotifyClose` and a reconnect loop | the Service, as [Connection loss](#connection-loss) describes |
| `ch.QueueDeclare`, `ch.ExchangeDeclare`, `ch.QueueBind` at startup | the end user's infrastructure, set up before the App starts |
| `ch.PublishWithContext` | `Publisher.Publish`, with confirms, returns, retries and a breaker |
| `amqp.SetLogger`, OpenTelemetry's globals | `s.Telemetry` |

Two behaviours change for such a consumer:

- **Under at-least-once, a processing error now stops the App,** and the
  message is rejected and delivered again, where a hand-written consumer
  often nacked it and carried on, or acked it and lost it.
- **Auto-ack is no longer how at-most-once is done.** `AtMostOnce` acks a
  batch of up to `Prefetch` deliveries, and waits for the broker to have the
  ack, before it processes any of them, so a backlog no longer fills the
  process's memory.
