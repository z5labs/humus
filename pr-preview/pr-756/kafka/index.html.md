# Consuming Kafka

The [`queue/kafka`](https://pkg.go.dev/github.com/z5labs/humus/queue/kafka)
package consumes Kafka topics as a `humus.Service`, with
[franz-go](https://github.com/twmb/franz-go). The Service joins a consumer
group, dials the brokers through the Setup's Host, and hands each record to
the end user's `queue.Processor`, one record at a time per partition, in
offset order. The end user's `Init` sources its `Settings`, builds the Service
with `kafka.NewService`, and returns it to `humus.Main`, whose defaults are a
consumer's.

[`queue`](https://pkg.go.dev/github.com/z5labs/humus/queue), in the core
module, is the contract between the end user's code and any broker: the
`Processor` they write, and when a message is acknowledged. `queue/kafka` is
in the nested module `github.com/z5labs/humus/queue/kafka`, so a service that
doesn't consume Kafka doesn't get franz-go in its module graph. The package
has no App, no `Main`, no defaults of its own and no test package. The design
behind everything on this page is in
[Proposal: queue processing and the Kafka runtime](https://github.com/z5labs/humus/discussions/683),
with its [revision](https://github.com/z5labs/humus/discussions/683#discussioncomment-18988751).

## The contract

The end user writes one thing, whatever the broker: a
[`queue.Processor`](https://pkg.go.dev/github.com/z5labs/humus/queue#Processor),
whose `Process(ctx, msg)` processes one message and returns an error when it
couldn't. `queue.ProcessorFunc` turns a function into one, and for Kafka the
message is a `kafka.Message`: the record's topic, partition, offset, key,
value, headers and timestamp.

A [`queue.Delivery`](https://pkg.go.dev/github.com/z5labs/humus/queue#Delivery)
says when a message is acknowledged, relative to processing it:

- **`queue.AtLeastOnce`** acknowledges a message once `Process` has returned
  `nil` for it. A message is never lost, and may be processed more than once.
- **`queue.AtMostOnce`** acknowledges a message before `Process` is called. A
  message is never processed twice, and is lost if processing fails or the
  process stops first.

`queue.CheckDelivery` rejects any other value, the zero `Delivery` included,
which is not a default. Every broker package's Service keeps the rules of
`queue`'s package doc:

- **A processing error under `AtLeastOnce` ends the Service.** The message
  isn't acknowledged, the Service stops as it would on a signal, and its
  `Serve` returns the error, so the App exits 1. The message is delivered
  again when the App restarts, to it or to another replica.
- **A processing error under `AtMostOnce` is logged and counted,** and
  processing goes on. The message was already acknowledged, so stopping
  wouldn't bring it back.
- **No panic is recovered.** A panicking `Processor` crashes the process.
- **Shutdown drains.** On `Serve`'s context a Service stops taking messages,
  lets the ones in flight finish, acknowledges them, and returns. `Process`'s
  context is not cancelled when shutdown starts, only when the grace period
  runs out.
- **Retries are the `Processor`'s.** A `Processor` that wants to retry a
  transient failure does so itself, before it returns an error. Neither
  `queue` nor a broker package retries for it.

**So a `Processor` under `AtLeastOnce` must be idempotent.** A message whose
processing failed, or was cut short by a crash, a rebalance or the grace
period running out, is delivered again, and processing it twice must leave
the same result as processing it once. This `Processor` stores each order
once, by the record's key, and skips an order it has already stored:

```go
// orders is the end user's Processor: it stores each order once, by the
// record's key, so an order delivered again is skipped.
type orders struct {
	mu     sync.Mutex
	stored map[string]bool
	out    io.Writer
}

// Handle stores the order m carries, unless it is stored already. It is
// idempotent, as a Processor under queue.AtLeastOnce must be.
func (o *orders) Handle(ctx context.Context, m kafka.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	id := string(m.Key)
	if o.stored[id] {
		return nil // delivered again: already stored
	}
	o.stored[id] = true
	_, err := fmt.Fprintf(o.out, "stored order %s: %s\n", id, m.Value)
	return err
}
```

A real `Processor` stores orders in a database, where the same idea is an
upsert, or a unique key that turns a second insert into a no-op. The mutex is
there because the Service processes its partitions concurrently, so `Handle`
may be called from more than one goroutine at once.

## A consumer

The end user's `Init` sources `kafka.Settings` with
[`config.Struct`](https://pkg.go.dev/github.com/z5labs/bedrock/config#Struct),
as [Configuring a humus service](/humus/pr-preview/pr-756/configuration/) describes,
builds the `Processor`, and returns the Service that `kafka.NewService`
builds:

```go
// Init is the end user's Init: it sources the Settings with config.Struct,
// builds the Processor, and returns the Service from NewService.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := kafka.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	list := func(v string) []string { return strings.Split(v, ",") }

	settings, err := config.Struct(func(r *config.Resolver) kafka.Settings {
		k := d
		// No .Default: Brokers, GroupID and Topics are required, so an unset variable fails as not found.
		k.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
		k.GroupID = r.Get(env("KAFKA_GROUP_ID").Validate(kafka.CheckGroupID))
		k.Topics = r.Get(env("KAFKA_TOPICS").Map(list).Validate(kafka.CheckTopics))
		k.SessionTimeout = r.Get(env("KAFKA_SESSION_TIMEOUT").Parse(time.ParseDuration).Default(d.SessionTimeout).Validate(kafka.CheckSessionTimeout))
		return k
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	h := &orders{stored: map[string]bool{}, out: s.Process.Stdout}
	svc, err := kafka.NewService(s, settings, queue.ProcessorFunc[kafka.Message](h.Handle))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}
```

`env` and `list` are the end user's own helpers: `env` names a variable, and
`list` splits a comma-separated value. The variable names are the service's
own: humus defines none of them.

The consumer's `main` is one call to `humus.Main`, here with a telemetry
`Builder` that names the service, as [Telemetry](/humus/pr-preview/pr-756/telemetry/#writing-the-builder)
describes:

```go
humus.Main(Init, humus.WithTelemetry(buildTelemetry))
```

**`NewService` opens no connection.** It runs every check on the `Settings`
again, rejects a nil `Processor`, and builds the Service's instruments. It
builds no client and registers no `OnStop` function, so an `Init` that fails
after it leaks nothing. `Serve` builds the franz-go client, dials every
broker through `s.Host.Dial`, joins the group and starts processing. Dialling
through the Host is what puts the Service on a test's network, with no
test-only seam.

`Init` returns the consumer along with any other Service the process runs,
such as a second consumer or the
[health Service](/humus/pr-preview/pr-756/health/#the-health-service). Any Service
returning ends the App.

## Settings

`kafka.Settings` follows the
[settings convention](/humus/pr-preview/pr-756/configuration/#what-a-package-with-settings-exports):
plain Go values, a check for each field, and `kafka.DefaultSettings()` for
the defaults. Every field's zero value means zero, never "use the default",
so an `Init` starts from `DefaultSettings()` and changes the fields it
sources. The package reads no environment variable and no file, and defines
no variable name.

| Field | Default | Valid | Check |
|---|---|---|---|
| `Brokers` | none: required | at least one; each `host:port`, with a host and a port from 1 to 65535 | `CheckBrokers` |
| `GroupID` | none: required | not empty | `CheckGroupID` |
| `Topics` | none: required | at least one; none empty; no topic twice | `CheckTopics` |
| `Delivery` | `queue.AtLeastOnce` | `queue.AtLeastOnce` or `queue.AtMostOnce` | `queue.CheckDelivery` |
| `TLS` | `nil`, which means plaintext | any | none |
| `SASL` | `nil`, which means no authentication | any | none |
| `SessionTimeout` | 45s | positive | `CheckSessionTimeout` |
| `RebalanceTimeout` | 60s | positive | `CheckRebalanceTimeout` |
| `FetchMaxBytes` | 50 MiB | positive | `CheckFetchMaxBytes` |
| `MaxPollRecords` | 500 | positive | `CheckMaxPollRecords` |

`SessionTimeout` and `FetchMaxBytes` keep franz-go's defaults.
`RebalanceTimeout` is franz-go's 60s too, because a revoked partition's record
in flight has to finish within it before the partition moves to another
member, as [Revocation](#revocation) describes. `MaxPollRecords` bounds how
many records one poll hands out, which bounds how many records an
at-most-once crash can lose, and how long a revoke waits for the records in
flight.

Other franz-go options become fields when end users ask for them, each with a
default and a check. A new field breaks no one, because `DefaultSettings`
supplies it.

### Required fields

**`Brokers`, `GroupID` and `Topics` are required.** None has a default:
humus imposes no cluster, no group and no topic. `DefaultSettings()` leaves
them `nil` and `""`, which their checks reject, and the end user's chains for
them have no `.Default`, as in `Init` above, so a variable that isn't set
fails as not found. The App exits 78, and `config.Struct` names every
variable at fault in one error. With none of the three set:

```
init: config KAFKA_BROKERS: not found
config KAFKA_GROUP_ID: not found
config KAFKA_TOPICS: not found
```

A value that fails its check fails the same way, naming the variable and the
value. With `KAFKA_BROKERS=kafka-0:9092,kafka-1`:

```
init: config KAFKA_BROKERS: "kafka-1" is not host:port
```

`NewService` runs the same checks itself, so a `Settings` built in code with a
bad field fails `Init` too, naming the field, such as
`kafka: NewService: Settings.GroupID must not be empty`. That error is no
`*config.Error`, so the App exits 1: the fault is in the code, not in the
configuration. [How a bad value fails](/humus/pr-preview/pr-756/configuration/#how-a-bad-value-fails)
explains the exit codes.

### The `Delivery` setting

**`Delivery` is set in code.** Whether a record may be processed twice or
lost is a property of the `Processor`, not of a deployment, so it is the end
user's design decision rather than an operator's, and most `Init`s don't
source it. `DefaultSettings()` gives the safe one, `queue.AtLeastOnce`. An
`Init` that wants at-most-once sets it on the `Settings` it builds:

```go
clicks.Delivery = queue.AtMostOnce // a click lost to a crash is not worth a second count
```

A Service has one `Delivery` and one `Processor`. A `Message` carries its
topic, so one `Processor` can dispatch on it. Two topics that need different
`Delivery` values are [two Services](#two-delivery-values-two-services).

### TLS and mTLS

`Settings.TLS` is the `*tls.Config` the Service dials the brokers with, and
`nil` means plaintext. A client certificate in its `Certificates` makes it
mTLS. The Service runs the TLS handshake itself over the connection
`s.Host.Dial` returns, and sets `ServerName` from the broker's host when the
config leaves it empty, without changing the end user's config.

The end user decodes the certificates from files, read through `s.Host.FS`
so that a test reads them from its own directory and production from the
real disk, as [Reading a file](/humus/pr-preview/pr-756/configuration/#reading-a-file)
describes. How they are stored and encoded is the end user's decision. This
chain reads the brokers' CA from the PEM file `KAFKA_TLS_CA_FILE` names, and
the client's certificate chain and private key from the one
`KAFKA_TLS_CERT_FILE` names:

```go
// The CA's file is read first, and the client's certificate only once it has been.
k.TLS = r.Get(env("KAFKA_TLS_CA_FILE").Parse(readFile(s.Host.FS)).FlatMap(func(ca []byte) config.Value[*tls.Config] {
	return env("KAFKA_TLS_CERT_FILE").Parse(readFile(s.Host.FS)).Parse(clientTLS(ca))
}))
```

A missing variable, a missing file and a file that holds no certificate each
fail as a config error with exit 78, named after its variable. `readFile` and
`clientTLS` are the end user's own helpers:

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

// clientTLS returns a parser for config.Value.Parse that builds the TLS
// config of a client that trusts the CA in caPEM, and presents the
// certificate chain and private key in the PEM file it is given.
func clientTLS(caPEM []byte) func(certPEM []byte) (*tls.Config, error) {
	return func(certPEM []byte) (*tls.Config, error) {
		cert, err := tls.X509KeyPair(certPEM, certPEM)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("holds no PEM certificate")
		}
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      roots,
			MinVersion:   tls.VersionTLS12,
		}, nil
	}
}
```

Brokers whose certificates a public CA signed need no `RootCAs`: a config
with only `Certificates` trusts the system's roots. Brokers that need TLS but
no client certificate take a config with only `RootCAs`.

### SASL

`Settings.SASL` is the mechanism the Service authenticates with, and `nil`
means no authentication. It is franz-go's own
[`sasl.Mechanism`](https://pkg.go.dev/github.com/twmb/franz-go/pkg/sasl), so
the end user picks one of franz-go's mechanisms, `plain`, `scram`, `oauth` or
`aws`, and supplies its credentials from wherever they keep them: humus
decides nothing about secrets. They are in franz-go's own module, so they add
no dependency. This chain builds SCRAM-SHA-512 credentials from two
variables, reading the password only once the user name has been read:

```go
k.SASL = r.Get(env("KAFKA_SASL_USER").FlatMap(func(user string) config.Value[sasl.Mechanism] {
	return env("KAFKA_SASL_PASSWORD").Map(func(password string) sasl.Mechanism {
		return scram.Auth{User: user, Pass: password}.AsSha512Mechanism()
	})
}))
```

Both chains are from one `Init`, whose test runs it against a cluster that
requires TLS with a client certificate and SCRAM-SHA-512. SASL sends its
credentials over the connection, so `PLAIN` in particular belongs over TLS.
A broker that refuses the credentials ends the Service with exit 1, rather
than franz-go retrying a password that will never be accepted, as
[Failures](#failures) lists.

## Delivery

What each `Delivery` commits, and when:

- **`AtLeastOnce`:** each partition processes a batch of records, and then
  commits the last record it processed, which commits every record before
  it. A record is committed only once it and every earlier record of its
  partition were processed. A record whose processing failed, or was cut
  short, is delivered again.
- **`AtMostOnce`:** each partition commits a batch before it processes any
  of it, and then processes every record of the batch. A record is never
  processed twice, and the records of a batch are lost if the process stops
  before it has processed them. `MaxPollRecords` bounds a batch, and so how
  many records one crash can lose.

Kafka acknowledges a partition's offset rather than one record, so a commit
covers every earlier record of its partition. Committing once per batch,
rather than once per record, saves a round trip per record, and a revoke and
a shutdown commit too, so only the records in flight at a crash are processed
again.

Which records are processed again, by Delivery:

| | `AtLeastOnce` | `AtMostOnce` |
|---|---|---|
| `Process` returns an error | that record and every record after it, when the App restarts. The records before it are committed. | none: the error is logged and counted, and processing goes on |
| the process crashes, or panics | every record after the partition's last commit | none: the records of the committed batch that weren't processed are lost |
| a rebalance revokes the partition | none: the record in flight finishes and is committed before the partition moves | none: the batch in flight finishes before the partition moves |
| the member is fenced out, and loses the partition | the records processed since the last commit, by the partition's new owner | none |
| SIGTERM | none: the record in flight finishes and is committed | none: the batch in flight finishes |
| the grace period runs out during a record | that record and every record after it | that record and the rest of its batch are lost |

## Order and concurrency

**Each assigned partition has one goroutine,** which processes its records
one at a time, in offset order. Partitions are processed concurrently, so the
concurrency is the number of partitions assigned to the Service. Kafka orders
a partition and not a topic, so records of different partitions are processed
in no particular order. Most consumers depend on the order within a
partition, for example to apply one customer's events in order, and a
producer that keys records by customer puts each customer's records on one
partition.

A replica that wants more concurrency gets more partitions: a topic with more
partitions than replicas spreads them across the group, and a replica
processes each of its own at once. Under `AtMostOnce` as under `AtLeastOnce`,
a partition's records are processed one at a time.

## Failures

No panic is recovered: a panicking `Processor` crashes the process. Every
other failure follows this table:

| | `AtLeastOnce` | `AtMostOnce` |
|---|---|---|
| `Process` returns an error | not committed; the Service stops as on shutdown, and `Serve` returns the error, naming the record; exit 1 | already committed; logged at ERROR, with the record's topic, partition and offset; processing goes on |
| `Process` panics | the process crashes; nothing after the last commit is committed | the process crashes; the batch was committed |
| a commit fails because the member was fenced out of the group, or the group is rebalancing | not an error: franz-go rejoins the group, and the records are committed by a later commit, or processed again by the partition's new owner | the same, and the batch whose commit failed is not processed. It is logged at WARN with its offsets, and if the partition stays with the member, the batch is lost |
| any other commit fails | the Service stops, and `Serve` returns the error; exit 1 | the same |
| a poll returns an error franz-go doesn't retry, such as an authorization failure | the Service stops, and `Serve` returns the error; exit 1 | the same |
| a broker refuses the SASL credentials | the Service stops, and `Serve` returns the error; exit 1 | the same |
| the brokers are unreachable | franz-go keeps retrying and logs each failed attempt at WARN, and kotel counts it; the Service keeps serving | the same |

A commit is refused for a fenced-out member with `UNKNOWN_MEMBER_ID`,
`ILLEGAL_GENERATION` or `FENCED_INSTANCE_ID`, and during a rebalance with
`REBALANCE_IN_PROGRESS`. A poll's error ends the Service when it is an error
code from Kafka that franz-go doesn't retry; an error code franz-go retries,
or a failure to connect, is franz-go's to recover from.

**An at-least-once failure stops the whole Service, not just its
partition.** The other partitions finish their record in flight and commit,
so only the failed record and those after it are processed again, when the
App restarts. The cause names the record, such as
`serve: kafka: processing orders, partition 0, offset 3: cannot process`. An
error that ends the Service is returned rather than logged, and the lifecycle
logs it.

Stopping is loud on purpose. Retrying in place would block the partition with
no limit, skipping would lose the record, and pausing the partition would
hide the failure while the lag grows. A record that always fails shows up as
a crash loop with its error in the logs.

### Dead letters

**Dead letters are the `Processor`'s.** A `Processor` that would rather skip
a record that always fails than stop on it stores the record wherever it
keeps dead letters, such as a table or a topic of its own, and returns `nil`.
The record is then committed like any other, and the partition moves on.
`queue/kafka` has no dead-letter topic, retry topic or skip policy: which
records are worth stopping for, and where the others go, is the end user's
decision.

## Shutdown and rebalances

A Service gets the lifecycle's [two signals](/humus/pr-preview/pr-756/lifecycle/#service),
and the consumer stops on the first:

- **`Serve`'s context is done** as soon as the App starts shutting down. The
  Service stops polling, lets each partition's record in flight finish,
  commits, leaves the group, closes the client and returns `nil`. Under
  `AtMostOnce`, the record in flight is every record of the batch that was
  committed.
- **`Shutdown`** stops `Serve` the same way, and waits for it to return.

**`Process`'s context is not cancelled when shutdown starts.** It keeps the
values of `Serve`'s context and carries the record's span, but not
`Serve`'s cancellation. So a `Processor` that honours its context doesn't
fail the record in flight on every SIGTERM, which under `AtLeastOnce` would
make every rollout end in exit 1.

**It is cancelled when the grace period runs out.** `Shutdown`'s context
carries the grace period as its deadline. When it is done before the records
in flight are, `Shutdown` cancels `Process`'s context and the leave, waits
for `Serve` to return, and returns an error that wraps its context's error,
which the lifecycle reports as a `*humus.GracePeriodError`, with exit 1. The
records cut short are not committed.

**Leaving the group rebalances it at once,** so the other replicas take the
partitions without waiting for `SessionTimeout`. A leave that fails, or that
the grace period cuts short, ends in the same place: the coordinator drops
the member once its session times out.

### Revocation

When the group rebalances and a partition moves to another member, such as
when a replica starts or leaves, franz-go revokes the partition first.
franz-go's default balancer is cooperative-sticky, so a rebalance revokes
only the partitions that move. The Service stops each revoked partition,
waits for its record in flight, commits, and only then lets the rebalance go
on, so the partition's new owner starts after the last record this member
processed. All of that has to fit in `RebalanceTimeout`: a member whose
revoke outlasts it is fenced out of the group.

The Service polls with franz-go's `BlockRebalanceOnPoll`, and allows
rebalancing again right after each poll has handed out its records. So
records are handed out only to partitions the member owns, and a slow record
never holds up a rebalance beyond the revoke, which waits only for the record
in flight.

### Loss

When the member is fenced out of the group, its partitions are lost: another
member may own them already, so a commit would fail. The Service stops them
after their record in flight without committing, franz-go rejoins the group,
and the partitions' new owner processes the uncommitted records again.

## The defaults

The consumer runs on `humus.Main`'s [defaults](/humus/pr-preview/pr-756/lifecycle/#the-defaults),
which are a consumer's, so `queue/kafka` has no `Main` and no defaults of its
own:

- **A grace period of 20s.** A `Processor` that can't finish its record in
  flight within it is cut short, and under `AtLeastOnce` the record is
  processed again.
- **No drain.** Nothing load-balances a consumer. Its traffic stops when it
  leaves the group, which it does as soon as its records in flight are done.
  The drain would only keep a consumer that has already stopped waiting.

### `terminationGracePeriodSeconds`

When Kubernetes stops a pod, it sends the container SIGTERM, and then
SIGKILL once `terminationGracePeriodSeconds` have passed, 30 by default. On
`humus.Main`'s defaults, the worst case from SIGTERM to the exit is the 20s
grace period and the telemetry's shutdown,
`telemetry.Settings.ShutdownTimeout`, 5s: 25s, inside Kubernetes' 30s, with a
margin for the process to exit.

A consumer that raises its grace period raises
`terminationGracePeriodSeconds` with it, to at least the grace period plus
the telemetry's shutdown plus that margin. Otherwise SIGKILL ends the process
first: its container exits 137, nothing in flight is committed, the group
waits `SessionTimeout` before it moves the partitions, and the telemetry not
yet exported is lost.

### Overriding them

An operator changes the grace period with `HUMUS_SHUTDOWN_GRACE_PERIOD`, such
as `HUMUS_SHUTDOWN_GRACE_PERIOD=45s`, and adds a drain with
`HUMUS_SHUTDOWN_DRAIN_DELAY`, without a rebuild, as for any App. A team that
wants another default in code passes `humus.WithShutdownGracePeriod` to
`humus.Main`; the variable still wins over it.
[The lifecycle's settings](/humus/pr-preview/pr-756/lifecycle/#settings) describes
both variables.

### Two `Delivery` values, two Services

A Service has one `Delivery`. Two topics that need different ones are two
Services, each with a group ID of its own, returned together from one
`Init`. A group ID is shared by every replica of one consumer, and Kafka
commits a group's offsets per topic and partition, so the two Services need
two groups:

```go
payments := kafka.DefaultSettings()
payments.Brokers = brokers
payments.GroupID = "payments-consumer"
payments.Topics = []string{"payments"}

clicks := kafka.DefaultSettings()
clicks.Brokers = brokers
clicks.GroupID = "clicks-consumer"
clicks.Topics = []string{"clicks"}
clicks.Delivery = queue.AtMostOnce // a click lost to a crash is not worth a second count

paymentsSvc, err := kafka.NewService(s, payments, p)
if err != nil {
	return nil, err
}
clicksSvc, err := kafka.NewService(s, clicks, p)
if err != nil {
	return nil, err
}
return []humus.Service{paymentsSvc, clicksSvc}, nil
```

Either Service returning ends the App, so an at-least-once failure in
`payments` stops `clicks` too, after its batch in flight.

### Which `Main`

Which `Main` the process uses depends on what a load balancer routes to, as
[the lifecycle's runtime packages](/humus/pr-preview/pr-756/lifecycle/#runtime-packages)
explains for mixed processes:

- **On its own, or next to a probes or metrics server,** a consumer stays on
  `humus.Main`. Nothing routes to that server, and a drain would only delay
  the exit.
- **Next to a server a load balancer routes to,** such as an API, the
  process uses that server's package's `Main`: `httpserver.Main` for
  [an HTTP server](/humus/pr-preview/pr-756/http/#the-defaults), with a 5s drain and a
  10s grace period, or `grpcserver.Main` for
  [a gRPC server](/humus/pr-preview/pr-756/grpc/#the-defaults), with a 5s drain and a
  15s grace period. The consumer still stops taking records as soon as
  shutdown starts, and its records in flight then have the drain and that
  package's grace period to finish, rather than `humus.Main`'s 20s.

## Telemetry

The Service reports through the Setup's telemetry, `s.Telemetry`, and never
through OpenTelemetry's globals: its tracer and meter come from the SDK's
`TracerProvider` and `MeterProvider`, under the scope
`github.com/z5labs/humus/queue/kafka`, and its propagator is the SDK's
`Propagator`.

### The span

Each record is processed under a span of its own, named `process <topic>`,
such as `process orders`, of kind `CONSUMER`. `Process` is given a context
that carries it, so every span the `Processor` starts from that context is
its child, and every record it logs with that context carries its trace ID.

**Its parent is the producer's.** The span's parent is the trace context the
propagator extracts from the record's headers, so a producer's trace
continues through the consumer. The producer's baggage, and only the
producer's, is on `Process`'s context too. A record whose headers carry no
trace context starts a new trace: `Serve`'s span is never a record's parent.

Its attributes follow OpenTelemetry's
[messaging conventions](https://opentelemetry.io/docs/specs/semconv/messaging/):
`messaging.system` (`kafka`), `messaging.operation.type` and
`messaging.operation.name` (both `process`), `messaging.destination.name`,
`messaging.destination.partition.id`, `messaging.consumer.group.name` and
`messaging.kafka.offset`. The record's key is not recorded, because it often
holds a customer's identifier. When `Process` returns an error, under either
`Delivery`, the error is recorded on the span as an exception event, with
`error.type` and an error status.

### The three instruments

The Service records three instruments of the messaging conventions, as
semconv v1.43.0 names them:

| Instrument | Kind | What it records |
|---|---|---|
| `messaging.client.consumed.messages` | counter | the records handed to the `Processor`, with `messaging.operation.name` set to `process` |
| `messaging.process.duration` | histogram, in seconds | how long each `Process` took, with `messaging.operation.name` set to `process`, and `error.type` when it failed |
| `messaging.client.operation.duration` | histogram, in seconds | how long each commit took, with `messaging.operation.name` set to `commit` and `messaging.operation.type` to `settle`, and `error.type` when it failed |

Each also has `messaging.system`, `messaging.consumer.group.name`,
`messaging.destination.name` and `messaging.destination.partition.id`. The
messaging conventions are still in development, so humus pins the semconv
version it uses, and a change to these names is a breaking change to
`queue/kafka`.

### franz-go's broker metrics

franz-go's broker metrics come from the `Meter` of franz-go's
[kotel](https://pkg.go.dev/github.com/twmb/franz-go/plugin/kotel) plugin,
which the Service builds with the Setup's `MeterProvider` and nothing else,
and hands the client as hooks. kotel's tracer is not used: the record's span,
the propagation through headers and the three instruments above are
humus's own.

- **By broker,** with a `node_id` attribute, kotel counts the connections
  opened, the connection errors and the connections closed, the bytes read
  and written, and the read and write errors.
- **By broker and topic,** with `node_id` and `topic`, it counts the records
  and the uncompressed bytes fetched. Its produce counters stay empty,
  because the Service produces nothing.

The instruments are kotel's, under its own scope,
`github.com/twmb/franz-go/plugin/kotel`, with names such as
`messaging.kafka.connects.count`, `messaging.kafka.connect_errors.count`,
`messaging.kafka.fetch_records.count`, `messaging.kafka.read_errors.count`
and `messaging.kafka.write_bytes`. They aren't conventions, because the
messaging conventions define none for a client's connections or bytes. humus
renames none of them, and pins the kotel version it uses, so a kotel release
that renames one is a breaking change to `queue/kafka`.

**A clean shutdown counts one read error.** Against kfake, franz-go's fake
cluster, a clean shutdown counts one read error for the broker, during the
shutdown and never before it. An alert on `messaging.kafka.read_errors.count`
should allow for it.

### The logs

The Service logs through the Setup's `LoggerProvider`, under its scope:

| Record | Severity | When |
|---|---|---|
| `partitions assigned` | INFO | the group assigns the Service partitions |
| `partitions revoked` | INFO | a rebalance revokes them, after their records in flight are committed |
| `partitions lost` | INFO | the member was fenced out of the group |
| `processing a record failed` | ERROR | `Process` returned an error under `AtMostOnce`. It carries the record's span's context, its topic, partition and offset, and the error as `exception.message` and `error.type`. |
| `batch skipped: its commit was refused` | WARN | under `AtMostOnce`, a batch's commit was refused for a member no longer in the group, so the batch wasn't processed |

The partition records carry `messaging.system`,
`messaging.consumer.group.name`, `messaging.destination.name` and
`messaging.destination.partition.ids`. An error that ends the Service is
returned, not logged here: the lifecycle logs it.

**franz-go's own logs** go through the same `LoggerProvider`, under the scope
`github.com/twmb/franz-go/pkg/kgo`, and never to standard error or the `log`
package's default logger. They include a broker it can't reach, which it logs
on every attempt while it retries, a rebalance, and a retried request. Each
of franz-go's messages is one record: the message is its body, and
franz-go's key-value pairs are its attributes, where a string, a number or a
bool keeps its type, an error is its text, and any other value is its `fmt`
text. franz-go's levels are its severities: error is ERROR, warn is WARN,
info is INFO and debug is DEBUG.

The Service asks the `Logger` of that scope which severities it is enabled
for, and franz-go builds no message below the lowest of them. So the end
user's `Builder` quiets franz-go as it does any scope:

- **On `humus.Main`'s default telemetry,** whose `LoggerProvider` exports
  from INFO, franz-go writes its INFO records, such as `joining group` and
  `leaving group`, to standard output with the rest of the App's, and builds
  no DEBUG message.
- **A `LoggerProvider` with no severity filter** is enabled for every
  severity, so with one franz-go logs at DEBUG: a record for every request it
  writes and every response it reads.
- **A `LoggerProvider` whose `Logger` for `github.com/twmb/franz-go/pkg/kgo`
  is enabled only from WARN** has franz-go log nothing at INFO or DEBUG, and
  one enabled for nothing has it log nothing at all.

## Unreachable brokers

**While the brokers are unreachable, the Service keeps serving.** franz-go
retries each connection with backoff, and the Service processes records once
the brokers are back. Failing `Serve` after a bound would turn a broker
outage into a crash loop of every consumer. A consumer that can't connect
shows up in two places:

- **kotel's `messaging.kafka.connect_errors.count`** counts each failed
  attempt, by broker, so a consumer that never connects shows up in its
  metrics.
- **franz-go's logs** record each attempt at WARN, as
  `unable to open connection to broker`, with the broker's address and the
  error.

**Readiness is not decided here.** The usual answer to a consumer that can't
connect is to report it not ready, and a consumer serves no probes today:
the process runs exactly the Services its `Init` returns. Proposal: exposing
health probes (wave 4) decides how a consumer's pod, which has no HTTP server
of its own, serves probes, and whether a consumer whose brokers are
unreachable is ready. If end users find that a consumer should fail instead,
a bound can be added as a setting, which breaks no one.

## Retries and panics

**Retries are the `Processor`'s.** A transient failure, such as a database
that is briefly unavailable, is the `Processor`'s to retry, before it returns
an error, with whatever it likes, such as bedrock's `retry` module, which
retries with a budget and backoff. Neither `queue` nor `queue/kafka` retries,
because a retry policy is the end user's decision, and a retry inside the
Service would block the partition just the same. A `Processor` that retries
honours its context, so a retry loop still stops when the grace period runs
out.

**Panics crash the process.** No panic is recovered, here or anywhere in the
lifecycle. Under `AtLeastOnce`, a crash loses nothing that a recovered panic
would have saved: every record after the last commit is processed again.
Under `AtMostOnce`, a recovered panic would carry on in a process whose state
the panic may have broken. The process exits with Go's exit status 2 and the
stack trace on stderr, and Kubernetes restarts the pod. A `Processor` that
wants a panic handled differently recovers it itself.

## Testing

A consumer is tested whole, with
[`humustest.Start`](https://pkg.go.dev/github.com/z5labs/humus/humustest#Start),
inside `testing/synctest`, as [Testing a humus App](/humus/pr-preview/pr-756/testing/)
describes for any App, against a fake cluster on the test's Host.

**The package ships no test package.** It has no defaults of its own, so
`humustest.Start` already runs a consumer on the ones production uses,
`humus.Main`'s, and putting a fake cluster on its Host takes one option. The
helpers below are a few lines each, which no API would save.

### A fake cluster on the test Host

franz-go's [kfake](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kfake) is
a Kafka cluster in memory, built for `synctest`. `kfake.ListenFn(h.Listen)`
makes it listen on humustest's network rather than the real one, and a test
builds it inside the bubble, with the bubble's `t`, and closes it before the
bubble ends:

```go
// cluster returns a kfake cluster listening on h, with topic seeded with
// the given number of partitions. It is closed when the test ends, inside
// the bubble that built it.
func cluster(t *testing.T, h *humustest.Host, partitions int32, opts ...kfake.Opt) *kfake.Cluster {
	t.Helper()
	all := append([]kfake.Opt{
		kfake.ListenFn(h.Listen),
		kfake.NumBrokers(1),
		kfake.SeedTopics(partitions, topic),
	}, opts...)
	c, err := kfake.NewCluster(all...)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}
```

A test's producer is a franz-go client that dials `h.Dial`, so it reaches the
cluster over the same network:

```go
// produceRecords writes records, each to its own partition, through a
// client that dials h with opts.
func produceRecords(t *testing.T, h *humustest.Host, c *kfake.Cluster, records []*kgo.Record, opts ...kgo.Opt) {
	t.Helper()
	all := append([]kgo.Opt{
		kgo.SeedBrokers(c.ListenAddrs()...),
		kgo.Dialer(h.Dial),
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
	}, opts...)
	cl, err := kgo.NewClient(all...)
	require.NoError(t, err)
	defer cl.Close()
	require.NoError(t, cl.ProduceSync(context.Background(), records...).FirstErr())
}
```

`kfake.TLS(cfg)` serves TLS, so mTLS is tested with certificates from
`humustest.NewCA`, and `kfake.EnableSASL` with `kfake.Superuser` requires
SASL credentials. A test never needs fixed certificates or passwords.

### Running the end user's `Init`

`humustest.Start` runs the end user's own `Init` on `humus.Main`'s defaults.
`humus.WithHost(h.Host())` puts the App on the Host the cluster listens on,
and `humustest.WithEnv` sets the variables `Init` reads. This test produces
one record, lets the consumer process and commit it, and stops the App with
SIGTERM, as Kubernetes would:

```go
func TestInit_processes_and_commits_a_produced_record(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		produce(t, h, c, 1) // key-0, with the value 0

		tel := humustest.NewTelemetry() // keeps the App's logs off p.Stdout()
		p, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"KAFKA_BROKERS":  strings.Join(c.ListenAddrs(), ","),
				"KAFKA_GROUP_ID": "orders-consumer",
				"KAFKA_TOPICS":   "orders",
			}),
		)
		synctest.Wait() // the record is processed and committed
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, "stored order key-0: 0\n", p.Stdout())
		require.Equal(t, int64(1), committed(c)[0], "the record is committed")
		require.Len(t, spansNamed(tel, "process orders"), 1)
	})
}
```

`synctest.Wait` returns once every goroutine in the bubble is durably
blocked: the consumer has processed and committed every record and is
polling for more. Fake time means the consumer's timeouts cost the test
nothing, so a test of a revoke, a slow record or a grace period that runs
out runs in milliseconds.

### The committed offsets

`c.GroupInfo` returns the group's state as the cluster sees it: its members,
and the offsets it has committed. A test asserts on what was committed, which
is what decides what is processed again:

```go
// committed returns the group's committed offset of each partition of
// topic.
func committed(c *kfake.Cluster) map[int32]int64 {
	offsets := make(map[int32]int64)
	info := c.GroupInfo(group)
	if info == nil {
		return offsets
	}
	for p, commit := range info.Commits[topic] {
		offsets[p] = commit.Offset
	}
	return offsets
}
```

A committed offset is the offset of the next record to process, so after
offsets 0 to 2 are processed and committed, it is 3. This test shows an
at-least-once failure and the redelivery after it, by running the App twice
on one cluster:

```go
st, seen := run(true)
require.Equal(t, humus.ExitFailure, st.Code())
require.ErrorIs(t, st.Cause(), bad)
require.ErrorContains(t, st.Cause(), "orders, partition 0, offset 3")
require.Equal(t, []int64{0, 1, 2, 3}, seen)
require.Equal(t, int64(3), committed(c)[0], "0 to 2 are committed, 3 is not")

st, seen = run(false)
require.Zero(t, st.Code(), st.Cause())
require.Equal(t, []int64{3, 4}, seen, "3 is delivered again")
require.Equal(t, int64(5), committed(c)[0])
```

### The Service contract

`queue/kafka`'s own tests hold `kafka.Service` to the Service contract with
[`humustest.TestService`](/humus/pr-preview/pr-756/testing/#checking-a-service), and a
Service of your own that wraps a consumer is checked the same way. The
constructor builds a cluster on `s.Host.Listen` and closes it with
`s.OnStop`:

```go
func TestService_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		c, err := kfake.NewCluster(
			kfake.ListenFn(s.Host.Listen),
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, topic),
		)
		require.NoError(t, err)
		s.OnStop(func(context.Context) error {
			c.Close()
			return nil
		})
		svc, err := kafka.NewService(s, settings(c.ListenAddrs()...), queue.ProcessorFunc[kafka.Message](func(context.Context, kafka.Message) error {
			return nil
		}))
		require.NoError(t, err)
		return svc
	})
}
```

### Against Kafka itself

kfake is not Kafka. `queue/kafka`'s tests against a real broker in a
container, behind the `testcontainers` build tag, cover what a fake can't
show, such as a real broker's cooperative-sticky rebalances and its fencing.
They are the only tests in the package that open a real socket, and they run
in a CI job of their own. A consumer's own tests rarely need them: the
behaviour they pin down is `queue/kafka`'s, not the `Processor`'s.

## Migration from the `queue` and `queue/kafka` packages before the reset

`queue` and `queue/kafka` replace the packages of the same names from before
the reset:

| Before the reset | After |
|---|---|
| `queue.Processor[T]`, `queue.ProcessorFunc[T]` | unchanged |
| `queue.Consumer`, `queue.Acknowledger`, their `Func` adapters, `queue.Runtime`, `queue.RuntimeFunc` | removed |
| `queue.ErrEndOfQueue` | removed; a finite queue is a [job](/humus/pr-preview/pr-756/jobs/) |
| `queue.NewApp(runtime)` | `humus.Main(Init)`, with the end user's `Init` returning the Service from `kafka.NewService` |
| `kafka.NewRuntime(brokers, groupID, opts...)` | `kafka.Settings{Brokers, GroupID, Topics}` and `kafka.NewService(s, settings, p)` |
| `kafka.AtLeastOnce(topic, p)`, `kafka.AtMostOnce(topic, p)` | `Settings.Topics` and `Settings.Delivery`; one `Processor` per Service |
| `kafka.SessionTimeout`, `RebalanceTimeout`, `FetchMaxBytes` | `Settings` fields with checks; `RebalanceTimeout` defaults to 60s |
| `kafka.MaxConcurrentFetches` | removed: its zero meant unlimited, and franz-go's default is unlimited |
| `kafka.WithTLS(cfg)` | `Settings.TLS`, dialled through `Host.Dial` |
| `kafka.Message.Attrs` | removed |
| `kafka.GroupIDAttr`, `TopicAttr`, `PartitionAttr`, `OffsetAttr` | removed; humus has no slog in its API, and the span and metric attributes follow the conventions |
| `messaging.client.messages.processed`, `messaging.client.messages.committed` | `messaging.client.consumed.messages`, `messaging.process.duration`, `messaging.client.operation.duration` |
| `otel.Get*`, `humus.Logger`, kotel, kslog | `s.Telemetry`; kotel's `Meter` alone, given `s.Telemetry.MeterProvider`, for franz-go's broker metrics |
| `HUMUS_KAFKA_*` variables, planned before the reset | none; the end user names and sources every setting |
| unit tests on testcontainers | unit tests on kfake over humustest's Host; containers only behind the build tag |

Two behaviours change for an existing processor:

- **Under at-least-once, a processing error now stops the App,** and the
  record is processed again, where before it was committed and lost.
- **Under at-most-once, a partition's records are processed in order, one
  at a time,** where before a batch was processed concurrently.
