# Consuming and producing Kafka

The [`queue/kafka`](https://pkg.go.dev/github.com/z5labs/humus/queue/kafka)
package consumes Kafka topics as a `humus.Service`, with
[franz-go](https://github.com/twmb/franz-go). The Service joins a consumer
group, dials the brokers through the Setup's Host, and hands each record to
the end user's `queue.Processor`, one record at a time per partition, in
offset order. The end user's `Init` sources its `Settings`, builds the Service
with `kafka.NewService`, and returns it to `humus.Main`, whose defaults are a
consumer's.

The package produces records too. A `kafka.Producer`, which the end user's
`Init` builds with `kafka.NewProducer`, produces to one cluster for any
Service of the process: a `Processor` that produces a dead letter or writes
to another topic, an HTTP handler that publishes an event, a job.
[Producing](#producing) describes it.

[`queue`](https://pkg.go.dev/github.com/z5labs/humus/queue), in the core
module, is the contract between the end user's code and any broker: the
`Processor` they write, and when a message is acknowledged. `queue/kafka` is
in the nested module `github.com/z5labs/humus/queue/kafka`, so a service that
doesn't use Kafka doesn't get franz-go in its module graph. The package has
no App, no `Main`, no defaults of its own and no test package. The design
behind the consumer is in
[Proposal: queue processing and the Kafka runtime](https://github.com/z5labs/humus/discussions/683),
with its [revision](https://github.com/z5labs/humus/discussions/683#discussioncomment-18988751),
and the design behind the producer in
[Proposal: producing to Kafka](https://github.com/z5labs/humus/discussions/764),
with its [revision 1](https://github.com/z5labs/humus/discussions/764#discussioncomment-19008694).

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
// record's key, so an order delivered again is skipped, and produces an
// order it can't read to a dead-letter topic.
type orders struct {
	mu     sync.Mutex
	stored map[string]bool
	out    io.Writer
	dlq    *kafka.Producer
}
```

Its `Handle` method, the `Processor`'s function, hands each order it can
read to `store`, and produces the others to a dead-letter topic with `dlq`,
as [Dead letters](#dead-letters) shows. `store` is the idempotent part:

```go
// store stores the order m carries, unless it is stored already. It is
// idempotent, as a Processor under queue.AtLeastOnce must be.
func (o *orders) store(m kafka.Message) error {
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
as [Configuring a humus service](/humus/pr-preview/pr-1096/configuration/) describes,
builds the `Processor`, and returns the Service that `kafka.NewService`
builds. This one also sources `kafka.ProducerSettings` and builds the
`Producer` its `Processor` produces dead letters with, as
[Building a Producer](#building-a-producer) describes:

```go
// Init is the end user's Init: it sources the Settings and the
// ProducerSettings with config.Struct, builds the Producer and the
// Processor, and returns the Service from NewService.
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

	dp := kafka.DefaultProducerSettings()
	producer, err := config.Struct(func(r *config.Resolver) kafka.ProducerSettings {
		p := dp
		// The dead letters go to the orders' own cluster, so Brokers reads the consumer's variable.
		p.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
		p.DeliveryTimeout = r.Get(env("KAFKA_DELIVERY_TIMEOUT").Parse(time.ParseDuration).Default(dp.DeliveryTimeout).Validate(kafka.CheckDeliveryTimeout))
		p.Breaker.OpenFor = r.Get(env("KAFKA_BREAKER_OPEN_FOR").Parse(time.ParseDuration).Default(dp.Breaker.OpenFor).Validate(kafka.CheckBreakerOpenFor))
		return p
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}
	dlq, err := kafka.NewProducer(s, producer) // opens no connection: the first Produce does
	if err != nil {
		return nil, err
	}

	h := &orders{stored: map[string]bool{}, out: s.Process.Stdout, dlq: dlq}
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
`Builder` that names the service, as [Telemetry](/humus/pr-preview/pr-1096/telemetry/#writing-the-builder)
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
[health Service](/humus/pr-preview/pr-1096/health/#the-health-service). Any Service
returning ends the App.

## Settings

`kafka.Settings` follows the
[settings convention](/humus/pr-preview/pr-1096/configuration/#what-a-package-with-settings-exports):
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
configuration. [How a bad value fails](/humus/pr-preview/pr-1096/configuration/#how-a-bad-value-fails)
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
real disk, as [Reading a file](/humus/pr-preview/pr-1096/configuration/#reading-a-file)
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
| the member is fenced out, and gives up the partition | the records processed since the last commit, by the partition's new owner | none |
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
| a commit fails because the member was fenced out of the group, or the group is rebalancing | not an error: franz-go rejoins the group. A later commit covers the records only while the partition stays with the member, and otherwise its new owner processes them again | the same, and the batch whose commit failed is not processed. It is logged at WARN with its offsets, and if the partition stays with the member, the batch is lost |
| any other commit fails | the Service stops, and `Serve` returns the error; exit 1 | the same |
| a poll returns an error franz-go doesn't retry, such as an authorization failure | the Service stops, and `Serve` returns the error; exit 1 | the same |
| a broker refuses the SASL credentials | the Service stops, and `Serve` returns the error; exit 1 | the same |
| the brokers are unreachable | franz-go keeps retrying and logs each failed attempt at WARN, and kotel counts it; the Service keeps serving | the same |

A commit is refused for a fenced-out member with `UNKNOWN_MEMBER_ID`,
`ILLEGAL_GENERATION` or `FENCED_INSTANCE_ID`, and during a rebalance with
`REBALANCE_IN_PROGRESS`. A commit the Service doesn't send, because the
partition isn't held under the member ID franz-go would send it under, is
handled the same way: see [Loss](#loss). A poll's error ends the Service when it is an error
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

A dead-letter topic is produced to with a [`Producer`](#producing). The
`orders` `Processor`'s `Handle` produces an order that isn't JSON, which
would fail on every delivery, to `orders.dlq`, with the consumed record's
key, value and headers:

```go
// Handle stores the order m carries. An order that isn't JSON would fail
// on every delivery, so rather than stop the Service on it, Handle produces
// it to the dead-letter topic, and returns nil once it is stored there.
func (o *orders) Handle(ctx context.Context, m kafka.Message) error {
	if !json.Valid(m.Value) {
		// A dead letter, stored before the record is committed.
		return o.dlq.Produce(ctx, kafka.Record{Topic: "orders.dlq", Key: m.Key, Value: m.Value, Headers: m.Headers})
	}
	return o.store(m)
}
```

**The dead letter is stored before the record is committed.** `Produce`
returns `nil` only once the cluster has acknowledged every record, and
`Handle` returns what `Produce` returns, so under `AtLeastOnce` the consumed
record is committed only after its dead letter is stored. A dead letter that
fails, because `DeliveryTimeout` passed, the
[circuit breaker](#the-circuit-breaker) is open or a broker refused the
credentials, is `Handle`'s error: the Service stops as on any processing
error, and the record is delivered again when the App restarts, rather than
lost. Under `AtMostOnce` the record was committed before `Handle` ran, so a
dead letter that fails is logged at ERROR, and the record is lost.

**A dead letter may be stored twice.** A record delivered again, after a
crash between the dead letter's acknowledgement and the commit, is produced
again, and so is one whose `Produce` failed after its dead letter was
stored anyway. Whatever reads the dead-letter topic tells duplicates apart
by the key, as any consumer under `AtLeastOnce` must.

Copying `m.Headers` is safe: the `Producer` replaces the consumed record's
trace context with the dead letter's own, as [The send span](#the-send-span)
describes. A `Processor` that wants a dead letter to say where it came from
adds headers of its own, such as the record's topic, partition and offset.

## Shutdown and rebalances

A Service gets the lifecycle's [two signals](/humus/pr-preview/pr-1096/lifecycle/#service),
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

When the member is fenced out of the group, another member may own its
partitions already. franz-go may report them as lost or, when it learns of the
fence from a refused commit and rejoins the group as a new member, as revoked.
Either way, the Service stops them after their record in flight and commits
nothing more for them, and the partitions' new owner processes the uncommitted
records again.

Kafka checks a commit's member ID and generation, but not who owns the
partition, so it would accept a commit from the rejoined member for a
partition another member now owns. So the Service sends a partition's commit
only under the member ID the partition is held under: that of the rebalance
that assigned it, or of a later one that kept it. A member that gets a
partition back after a fence commits it under its new member ID.

## The defaults

The consumer runs on `humus.Main`'s [defaults](/humus/pr-preview/pr-1096/lifecycle/#the-defaults),
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
[The lifecycle's settings](/humus/pr-preview/pr-1096/lifecycle/#settings) describes
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
[the lifecycle's runtime packages](/humus/pr-preview/pr-1096/lifecycle/#runtime-packages)
explains for mixed processes:

- **On its own, or next to a probes or metrics server,** a consumer stays on
  `humus.Main`. Nothing routes to that server, and a drain would only delay
  the exit.
- **Next to a server a load balancer routes to,** such as an API, the
  process uses that server's package's `Main`: `httpserver.Main` for
  [an HTTP server](/humus/pr-preview/pr-1096/http/#the-defaults), with a 5s drain and a
  10s grace period, or `grpcserver.Main` for
  [a gRPC server](/humus/pr-preview/pr-1096/grpc/#the-defaults), with a 5s drain and a
  15s grace period. The consumer still stops taking records as soon as
  shutdown starts, and its records in flight then have the drain and that
  package's grace period to finish, rather than `humus.Main`'s 20s.

## Producing

A [`kafka.Producer`](https://pkg.go.dev/github.com/z5labs/humus/queue/kafka#Producer)
produces records to one Kafka cluster. The end user's `Init` builds it with
`kafka.NewProducer`, from `kafka.ProducerSettings`, and any Service uses it:
a `Processor` that produces a dead letter or writes to another topic, an HTTP
handler that publishes an event, a job. **It is not a Service.** Producing
has no loop of its own to serve: it is called from the Services that do, so
it lives as long as they do and closes after them, as
[Shutdown and the grace period](#shutdown-and-the-grace-period) describes.
One `Producer` is safe for concurrent use and serves every Service of the
process.

### Building a Producer

The end user's `Init` sources `ProducerSettings` with `config.Struct`, as it
does the consumer's `Settings`, and builds the `Producer` before the Services
that use it. In [the consumer's `Init`](#a-consumer), the dead letters go to
the cluster the orders come from, so the chain reads the consumer's
`KAFKA_BROKERS` again:

```go
dp := kafka.DefaultProducerSettings()
producer, err := config.Struct(func(r *config.Resolver) kafka.ProducerSettings {
	p := dp
	// The dead letters go to the orders' own cluster, so Brokers reads the consumer's variable.
	p.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
	p.DeliveryTimeout = r.Get(env("KAFKA_DELIVERY_TIMEOUT").Parse(time.ParseDuration).Default(dp.DeliveryTimeout).Validate(kafka.CheckDeliveryTimeout))
	p.Breaker.OpenFor = r.Get(env("KAFKA_BREAKER_OPEN_FOR").Parse(time.ParseDuration).Default(dp.Breaker.OpenFor).Validate(kafka.CheckBreakerOpenFor))
	return p
}).Read(ctx)
if err != nil {
	return nil, err // exit 78, naming every bad variable
}
dlq, err := kafka.NewProducer(s, producer) // opens no connection: the first Produce does
if err != nil {
	return nil, err
}
```

Each `config.Struct` names every bad variable of its own, and the
consumer's is read first, so with nothing set, `Init` fails naming
`KAFKA_BROKERS`, `KAFKA_GROUP_ID` and `KAFKA_TOPICS` once each, as
[Required fields](#required-fields) shows. A bad value of the producer's,
such as `KAFKA_DELIVERY_TIMEOUT=0s`, fails the same way, with exit 78.

**`Brokers`, `TLS` and `SASL` are the consumer's fields, repeated.** They
mean what they mean in `Settings`, take the same check, and dial through the
Setup's Host the same way, with the same TLS handshake. They are repeated
rather than shared because a `Producer` may produce to a cluster other than
the one a Service consumes, so each set is sourced on its own:

- **The same cluster, from the same variables:** the chain reads the
  consumer's variables again, as above.
- **The same cluster, copied:** an `Init` that has already built the
  consumer's `Settings` copies the three. The
  [secure `Init`](#tls-and-mtls) produces its dead letters over the same TLS,
  with the same SCRAM credentials:

  ```go
  producer := kafka.DefaultProducerSettings()
  // The same cluster, over the same TLS and with the same credentials.
  producer.Brokers, producer.TLS, producer.SASL = settings.Brokers, settings.TLS, settings.SASL
  ```

- **Another cluster:** the chain reads variables of its own, such as
  `KAFKA_DLQ_BROKERS`, with its own TLS and SASL chains.

A process that produces to two clusters builds two `Producer`s.

`ProducerSettings` follows the
[settings convention](/humus/pr-preview/pr-1096/configuration/#what-a-package-with-settings-exports),
like `Settings`: `kafka.DefaultProducerSettings()` gives the defaults, every
field's zero value means zero, and the package reads no variable of its own.

| Field | Default | Valid | Check |
|---|---|---|---|
| `Brokers` | none: required | at least one; each `host:port`, with a host and a port from 1 to 65535 | `CheckBrokers` |
| `TLS` | `nil`, which means plaintext | any | none |
| `SASL` | `nil`, which means no authentication | any | none |
| `DeliveryTimeout` | 10s | positive | `CheckDeliveryTimeout` |
| `Breaker.Window` | 20 | 1 to `circuit.MaxCount` | `CheckBreakerWindow` |
| `Breaker.Probes` | 1 | 1 to `circuit.MaxCount` | `CheckBreakerProbes` |
| `Breaker.OpenFor` | 10s | positive | `CheckBreakerOpenFor` |
| `Breaker.ProbeTimeout` | 10s | positive | `CheckBreakerProbeTimeout` |
| `BreakerPolicy` | see [The circuit breaker](#the-circuit-breaker) | `Trip` and `Probe` set | none: `NewProducer` checks it |

**`NewProducer` opens no connection.** It runs every check again, and fails
naming each field at fault, such as
`kafka: NewProducer: ProducerSettings.DeliveryTimeout must be positive`,
which exits 1, as a bad `Settings` given to `NewService` does. It builds the
breaker, the instruments and one franz-go client, and registers the
`Producer`'s flush and close with `s.OnStop`. The first `Produce` dials the
brokers.

**Not settings:** acks from all in-sync replicas and idempotent writes, which
are franz-go's and Kafka's defaults, and which turning off would trade
durability for latency in a way that stays invisible until a broker fails;
and franz-go's partitioner, which hashes keyed records with murmur2 as
Kafka's Java producer does, its linger, compression, batch size and buffer
size. Like the consumer's, a franz-go option becomes a field, with a default
and a check, when end users ask for it.

### A call

`Producer.Produce(ctx, records...)` is synchronous. **It returns `nil` once
the cluster has acknowledged every record,** with acks from all in-sync
replicas, or an error once one has failed. So a record is either stored or
failed when `Produce` returns, which is what `AtLeastOnce` needs: a
`Processor` that produces before it returns `nil` has the consumed record
committed only after the produced one is stored, as
[Dead letters](#dead-letters) shows.

**More throughput is more records per call, or more goroutines.** There is no
asynchronous `Produce`. At franz-go's level a synchronous call costs no
throughput: it takes the same path through franz-go's buffers, batches and
requests in flight as an asynchronous one, and it stops franz-go's linger on
the partitions it produced to, so they are sent at once. Under load, records
pile up while requests are in flight, and the next request takes them all,
so the records of many callers share a batch. A caller that wants not to
wait at all calls `Produce` in a goroutine of its own. An asynchronous call
was rejected: its callback would run on franz-go's one goroutine for
results, where blocking stalls every producer of the process; a `Processor`
that used it could commit a record whose output is later lost; and it would
tie the API to franz-go's.

**Order.** Records to one partition are stored in the order they are given:
within one call, in the order of its records, and between calls on different
goroutines, in the order they reach the client. When a record of a partition
fails, franz-go fails the records buffered after it on that partition too, so
a later record is never stored after an earlier one failed.

**Errors.** When a record failed, `Produce` returns one error joining each
failed record's error, with its topic, and its partition when one was chosen,
so `errors.Is` finds `kgo.ErrRecordTimeout`, `context.DeadlineExceeded` or a
`kerr` code in it. A failed call doesn't say which of its records were
stored: the records that failed may have been stored anyway. `Produce`
rejects a record with no `Topic` before it sends anything, and returns `nil`
when given no records.

### Records outlive a cancelled call

**`Produce` honours its context up to the moment it hands its records to
franz-go.** A call whose `ctx` is already done sends nothing, starts no span,
doesn't reach the breaker, and returns an error that wraps `ctx`'s error.

**After the hand-off, a `ctx` that ends returns the call at once,** with the
same error, and its records stay in the client, with a context of the call's
own, until the cluster acknowledges them or `DeliveryTimeout` fails them.
Their spans and measurements are recorded then. So a handler whose request
is cancelled returns at once, and the grace period reaches a `Process`
blocked in `Produce`, as the lifecycle requires.

The records don't follow `ctx` after the hand-off, because franz-go can't
cancel one caller's records on their own:

- **A sent record can't be taken back.** The broker stores a request it has
  read, whether the client still waits for it or not. Cancelling the request
  would change what the caller is told, not what is stored.
- **Only a batch's first record counts.** franz-go decides whether to fail a
  batch from its first record's context alone, so a cancelled record in the
  middle of a batch is sent anyway.
- **One cancelled record fails its partition.** franz-go fails every record
  buffered behind it on that partition, whoever produced them.
- **A cancelled request cuts the connection.** It closes the broker's
  produce connection and fails every request in flight on it, for every
  partition that broker leads. They are sent again after a new dial, TLS
  handshake and SASL authentication.
- **It bumps the producer epoch.** The next produce, on any broker, waits
  for the cluster to hand out a new producer ID.

One `Producer` serves the whole process. If a caller's `ctx` reached
franz-go, any caller could fail any other caller's records: an HTTP client
that disconnects could fail a dead letter that a `Processor` buffered behind
that handler's event on the same partition. The `Processor` would get a
`context.Canceled` that isn't its own, and under `AtLeastOnce` that error
ends the consumer, and the App exits 1.

### `DeliveryTimeout`, and no retry settings

**`DeliveryTimeout`, 10s by default, bounds how long the cluster has to
acknowledge the records of one call,** franz-go's retries included. A record
that isn't acknowledged by then fails, even one already sent, and may still
have been stored, so a caller that produces it again may store it twice,
which is at-least-once and what a `Processor` under `AtLeastOnce` already
handles. Without the bound, a hung broker would hold a call, and the App's
shutdown, until it answered.

**`DeliveryTimeout` is the one retry setting:** how long a call lets
franz-go keep trying. There is no retry count, no backoff and no retry
budget, and no bedrock `retry.Retrier` around `Produce`, because franz-go
retries, and a retry around it would do harm:

- **Duplicates.** franz-go's idempotent producer retries a batch on the
  sequence numbers that let the broker drop a batch it has already stored. A
  retry around `Produce` would hand the records over again under new
  sequence numbers, and store twice a record whose first send landed.
- **Order.** franz-go retries a partition's batches in order. A retry around
  `Produce` would send its records after whatever other calls produced in the
  meantime.
- **Retries don't multiply with callers.** franz-go retries a partition's
  batch, which holds every caller's records for that partition, once per
  backoff. A thousand callers waiting on a dead partition cost one retry per
  backoff, not a thousand, so there is no retry storm for a budget to stop.

The default fits the grace periods of the runtime packages: it is half of
`humus.Main`'s 20s, which a consumer runs on, so a `Process` blocked in
`Produce` at SIGTERM finishes inside the grace period, and it equals
`httpserver.Main`'s 10s, where a handler's `Produce` returns sooner anyway
when the grace period cancels its request.

### The circuit breaker

**A circuit breaker guards the cluster,** so that while the cluster keeps
failing, `Produce` fails at once rather than hold every caller for
`DeliveryTimeout`. Without it, an HTTP handler that publishes an event would
hold its request for 10s during an outage before it answered 503. With it,
the first few calls wait, and the rest fail at once until a probe gets
through. It is bedrock's
[`circuit.Breaker`](https://pkg.go.dev/github.com/z5labs/bedrock/circuit),
one per `Producer`: one `Producer` is one client to one cluster.

While the breaker is open, `Produce` returns at once, with nothing sent, an
error wrapping a `*circuit.RejectedError`, which matches
`circuit.ErrRejected` and says how much longer the breaker stays open in its
`RetryAfter`. `Produce` doesn't retry a rejected call: whether to wait and
try again is the caller's, as `queue`'s contract leaves retries to the
`Processor`.

**Each call to `Produce` is one call through the breaker.** It is admitted
before any of its records reach franz-go, and counted once every record has
its result, which may be after `Produce` returns. The breaker counts the
cluster, not the records or the callers:

- **A success** is a call whose every record was acknowledged.
- **An ignored call** counts as nothing: its records failed for their own
  reasons or their topics', not the cluster's, with a Kafka error code
  franz-go doesn't retry, such as `MESSAGE_TOO_LARGE`, `INVALID_RECORD` or
  `TOPIC_AUTHORIZATION_FAILED`, or `UNKNOWN_TOPIC_OR_PARTITION` once franz-go
  has given up on the topic. One topic's mistake doesn't open the breaker for
  the others.
- **A failure** is anything else, the cluster not storing records in time:
  `DeliveryTimeout` passing, `kgo.ErrRecordTimeout`, a retriable error code
  franz-go stopped on, or a connection that failed.

A caller giving up never counts: the breaker sees each call's result with
the call's own context, which a caller's `ctx` doesn't cancel. A call whose
`ctx` was already done, and a call after a broker
[refused the credentials](#refused-credentials), return before the breaker,
so it counts neither.

**The defaults:** the breaker opens after 5 failures in a row, or once half
of the last 20 calls, `Breaker.Window`, failed. It stays open for
`Breaker.OpenFor`, 10s, and then turns half-open: it lets `Breaker.Probes`
calls through at once, 1, as probes, each holding its slot for at most
`Breaker.ProbeTimeout`, 10s, which matches `DeliveryTimeout`, so a probe
holds its slot no longer than its call does. It opens again on a failed
probe, and closes after 3 successful ones. `Breaker` is bedrock's own
`circuit.Config`, with a check for each field, so an operator's bad
`KAFKA_BREAKER_OPEN_FOR` fails startup naming the variable, with exit 78.

**`BreakerPolicy` is set in code.** It is a `circuit.Policy`, two functions,
`Trip` and `Probe`, so it can't come from a variable. bedrock ships no
policy, so `DefaultProducerSettings` holds humus's, and the end user
replaces it in their `Init`. A policy whose `Trip` always returns `false`
turns the breaker off:

```go
ps.BreakerPolicy.Trip = func(circuit.Counts) bool { return false }
```

**One breaker covers every partition.** When one broker of three is down,
the calls to the partitions it leads fail and the rest succeed, which the
default policy may or may not trip on. A breaker per broker or per partition
would follow Kafka's failures more closely, but franz-go picks a record's
partition inside the client, after the breaker has admitted the call.
franz-go moves unkeyed records away from backed-up partitions, and a keyed
record has only one partition.

Each change of the breaker's state is a log record, as
[The logs](#the-logs) lists. The `Producer` measures the breaker too, with
three instruments of humus's own: its state, its changes of state, and the
calls it counted or rejected, as
[humus's own instruments](#humuss-own-instruments) lists. A rejected call is
also an error on its records' spans.

### Refused credentials

**When a broker refuses the `Producer`'s SASL credentials,** with
`SASL_AUTHENTICATION_FAILED`, `UNSUPPORTED_SASL_MECHANISM` or
`ILLEGAL_SASL_STATE`, the `Producer` fails every call. franz-go would
otherwise retry the connection, and each call would wait out its
`DeliveryTimeout` with no sign of why. A Service stops the App on a refusal,
as [Failures](#failures) lists, and a `Producer` can't, so it fails loudly
instead:

- on the first refusal, it emits one ERROR record, `a broker refused the
  credentials`, naming the broker, as [The logs](#the-logs) lists, and the
  calls waiting on their records return the refusal;
- from then on, every `Produce` returns the refusal at once, with nothing
  sent, and before the breaker.

The error names the broker as `host:port`, as the Service's does, and
`errors.Is` finds its `kerr` code in it, such as
`kerr.SaslAuthenticationFailed`. Credentials don't fix themselves while the
process runs, so the `Producer` never tries them again: a `Processor` that
produces fails its record, under `AtLeastOnce` the Service stops, and the App
is restarted with whatever the secret holds then.

### Shutdown and the grace period

**The `Producer` is not a Service, so the lifecycle doesn't stop it.** The
Services that use it stop first: the App calls their `Shutdown`, waits for
every `Serve` to return, and only then runs the `OnStop` functions, so no
Service is producing when the `Producer` closes. On SIGTERM, a `Process`
blocked in `Produce` keeps its context, as [Shutdown and rebalances](#shutdown-and-rebalances)
describes, so its dead letter is stored, and its record committed, as the
Service drains.

**`NewProducer` registers one function with `s.OnStop`.** It flushes the
client with the grace period's context, which waits for the records still in
it, such as those of a call that returned when its `ctx` was done, at most
`DeliveryTimeout` old. Then it closes the client.

**When the grace period runs out first,** closing the client fails what is
left with `kgo.ErrClientClosed`, those records are lost, and the function
returns an error that wraps the context's error, which the lifecycle reports
as a `*humus.GracePeriodError`, with exit 1. A `Produce` after that, from a
goroutine the end user left running, fails at once with an error that wraps
`kgo.ErrClientClosed`.

If the end user's `Init` fails after `NewProducer`, the lifecycle runs the
`OnStop` functions registered so far, so the client is closed and none of
franz-go's goroutines outlive the App.

## Telemetry

The Service and the `Producer` report through the Setup's telemetry,
`s.Telemetry`, and never through OpenTelemetry's globals: their tracers and
meters come from the SDK's `TracerProvider` and `MeterProvider`, under the
scope `github.com/z5labs/humus/queue/kafka`, and their propagator is the
SDK's `Propagator`.

### The process span

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

### The send span

**The `Producer` sends each record under a span of its own,** named
`send <topic>`, such as `send orders.dlq`, of kind `PRODUCER`, a child of the
span in `Produce`'s context. Its attributes are `messaging.system` (`kafka`),
`messaging.operation.type` and `messaging.operation.name` (both `send`) and
`messaging.destination.name`, and, once the record is acknowledged,
`messaging.destination.partition.id` and `messaging.kafka.offset`. The
record's key is not recorded, as the consumer doesn't record it. The span
ends when the record is acknowledged or fails, as its result arrives, which
may be after `Produce` returns. A failure, a rejection by the breaker
included, is recorded on it as an exception event, with `error.type` and an
error status.

**The span's context goes into the record's headers.** The Setup's
propagator injects it, with the baggage of `Produce`'s context: with the
default propagator, those are `traceparent`, `tracestate` and `baggage`.
Before injecting, the `Producer` removes every header of the record whose key
is one of the propagator's fields. A dead letter that copies the consumed
record's headers would otherwise carry two `traceparent` headers, and the
consumer would read the first, the old one.

So the `process` span of a Service that consumes the record, on any topic, is
a child of its `send` span, and inside a `Process`, the `send` span is a child
of the `process` span: one trace runs from the first producer through every
topic a record passes through, its dead letter included.

**When a span starts.** `Produce` first checks the call: a record with no
topic, a `ctx` already done, a closed client or a broker's earlier refusal
of the credentials returns it at once, and starts no span. Only then does
each record's span start, and its headers get the trace context, before the
call goes through the breaker. So a call the breaker rejects has a span for
each record, which records the rejection, and a call that never got past
the checks has none.

### The instruments

The Service records three instruments of the messaging conventions, as
semconv v1.43.0 names them:

| Instrument | Kind | What it records |
|---|---|---|
| `messaging.client.consumed.messages` | counter | the records handed to the `Processor`, with `messaging.operation.name` set to `process` |
| `messaging.process.duration` | histogram, in seconds | how long each `Process` took, with `messaging.operation.name` set to `process`, and `error.type` when it failed |
| `messaging.client.operation.duration` | histogram, in seconds | how long each commit took, with `messaging.operation.name` set to `commit` and `messaging.operation.type` to `settle`, and `error.type` when it failed |

Each also has `messaging.system`, `messaging.consumer.group.name`,
`messaging.destination.name` and `messaging.destination.partition.id`.

The `Producer` records two more:

| Instrument | Kind | What it records |
|---|---|---|
| `messaging.client.sent.messages` | counter | each record once its result is known, with `messaging.operation.name` set to `send`, and `error.type` when it failed |
| `messaging.client.operation.duration` | histogram, in seconds | how long each record took, from `Produce` taking it to its result, with `messaging.operation.name` and `messaging.operation.type` set to `send`, and `error.type` when it failed |

Each also has `messaging.system`, `messaging.destination.name` and, once
franz-go has chosen it, `messaging.destination.partition.id`. The
`Producer`'s `messaging.client.operation.duration` is the same instrument as
the Service's commits, told apart by `messaging.operation.name`. A record the
breaker rejected, a record with no topic, and the records of a call whose
context was already done were never sent, so neither instrument counts them:
the conventions say a sent-messages count must not count messages that were
created but haven't been sent. A rejected record's span records the
rejection, and `humus.circuit.calls` counts its call.

The messaging conventions are still in development, so humus pins the
semconv version it uses, and a change to these names is a breaking change to
`queue/kafka`.

### humus's own instruments

The `Producer` records three instruments of humus's own for its
[breaker](#the-circuit-breaker), with the Setup's `MeterProvider`, in the
scope `github.com/z5labs/humus/queue/kafka`, beside its messaging
instruments:

| Instrument | Kind | Unit | Attributes | Recorded |
|---|---|---|---|---|
| `humus.circuit.state` | asynchronous UpDownCounter | `{breaker}` | `humus.circuit.state`: `closed`, `open` or `half-open` | at each collection: 1 for the breaker's state, 0 for the other two |
| `humus.circuit.transitions` | Counter | `{transition}` | `humus.circuit.state`, the state it went to, and `humus.circuit.previous_state`, the one it left | at each change of the breaker's state, beside its log record |
| `humus.circuit.calls` | Counter | `{call}` | `humus.circuit.outcome`: `success`, `failure`, `ignored` or `rejected` | once a call's every record has its result, or as the breaker rejects it |

Every data point also carries the cluster's attributes: `messaging.system`,
set to `kafka`, and `server.address` and `server.port` of the first of
`ProducerSettings.Brokers`, the seed the end user named first rather than
the broker that answered. It carries nothing per call, such as
`messaging.destination.name`: the messaging instruments break records down
already. Each other attribute's value is the `String()` of bedrock's own
type, `circuit.State` or `circuit.Outcome`.

- **`humus.circuit.state` is read, not tracked.** At each collection it reads
  the breaker's state once, so it shows `half-open` as soon as `OpenFor` has
  passed, with no call made. Two `Producer`s for two clusters are two
  series, told apart by their first brokers. The App's last collection,
  after the `OnStop` functions, still reads it, so the final export has the
  state the App stopped in.
- **`humus.circuit.transitions` catches the short opens.** A breaker can
  open, go half-open and close between two collections, a minute apart by
  default, and `humus.circuit.state` never shows it open.
- **`humus.circuit.calls` counts a call when the breaker does.** One call is
  one `Produce`, whatever its number of records, counted once every record
  has its result, which may be after `Produce` returns. A call whose records
  failed for their own reasons or their topics' is `ignored`. A call the
  breaker rejects is `rejected`, once: it sent nothing, so it is in neither
  messaging instrument. A call whose `ctx` was already done, or one after a
  broker refused the credentials, never reached the breaker and counts
  nothing.

There is no `humus.retry.decisions`, as humus's HTTP and gRPC clients
record: franz-go retries inside a produce request, and exposes no decision
to count.

In Prometheus, they are `humus_circuit_state`,
`humus_circuit_transitions_total` and `humus_circuit_calls_total`, and each
attribute is a label such as `humus_circuit_state`. An alert on an open
breaker reads the state, and one on breakers that open and close between
scrapes reads the transitions:

```promql
# A breaker is open now.
humus_circuit_state{humus_circuit_state="open"} > 0

# A breaker opened in the last 5 minutes, however briefly.
increase(humus_circuit_transitions_total{humus_circuit_state="open"}[5m]) > 0
```

A View in the end user's telemetry `Builder` drops or renames any of them.
humus has no setting to turn them off. A test reads them as it reads any
metric, with `humustest.Telemetry`'s `Metrics`, filtered to the scope.
[humus's own instruments](/humus/pr-preview/pr-1096/telemetry/#humuss-own-instruments)
on the Telemetry page lists what each of humus's clients records, with the
View and the test written out.

### franz-go's broker metrics

franz-go's broker metrics come from the `Meter` of franz-go's
[kotel](https://pkg.go.dev/github.com/twmb/franz-go/plugin/kotel) plugin,
which the Service and the `Producer` each build with the Setup's
`MeterProvider` and nothing else, and hand their client as hooks. kotel's
tracer is not used: the spans, the propagation through headers and the
instruments above are humus's own.

- **By broker,** with a `node_id` attribute, kotel counts the connections
  opened, the connection errors and the connections closed, the bytes read
  and written, and the read and write errors.
- **By broker and topic,** with `node_id` and `topic`, it counts the records
  and the uncompressed bytes the Service fetched, and the records and the
  bytes the `Producer` produced, in `messaging.kafka.produce_records.count`
  and `messaging.kafka.produce_bytes.count`.

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
| `partitions revoked` | INFO | a rebalance revokes them, after their records in flight are committed; after a fence, nothing more is committed for them |
| `partitions lost` | INFO | the member was fenced out of the group |
| `processing a record failed` | ERROR | `Process` returned an error under `AtMostOnce`. It carries the record's span's context, its topic, partition and offset, and the error as `exception.message` and `error.type`. |
| `batch skipped: its commit was refused` | WARN | under `AtMostOnce`, a batch's commit was refused for a member no longer in the group, or not sent for a partition the member no longer holds, so the batch wasn't processed |

The partition records carry `messaging.system`,
`messaging.consumer.group.name`, `messaging.destination.name` and
`humus.messaging.destination.partition.ids`, the partitions they name. The
skipped batch's record carries the same three, its partition as
`messaging.destination.partition.id`, its first and last offsets as
`humus.messaging.kafka.offset.first` and `humus.messaging.kafka.offset.last`,
and the refusal as `error.type` and `exception.message`. An error that ends
the Service is returned, not logged here: the lifecycle logs it.

The `Producer` logs under the same scope:

| Record | Severity | When |
|---|---|---|
| `circuit breaker open` | WARN | the [breaker](#the-circuit-breaker) opened, from closed or from half-open |
| `circuit breaker half-open` | INFO | the breaker's `OpenFor` passed, and it lets probes through |
| `circuit breaker closed` | INFO | enough probes succeeded |
| `a broker refused the credentials` | ERROR | the first time a broker [refused the credentials](#refused-credentials). It carries `messaging.system`, `server.address` and `server.port` naming the broker, and the refusal as `exception.message` and `error.type`. |

A breaker record is emitted in the context of the call that changed the
state, so it carries that call's trace ID. Its attributes are the cluster's,
`messaging.system`, set to `kafka`, and `server.address` and `server.port`
of the first of `ProducerSettings.Brokers`, then the state the breaker left
as `humus.circuit.previous_state`, and the counts of that state:
`humus.circuit.successes`, `humus.circuit.failures`,
`humus.circuit.consecutive_successes`, `humus.circuit.consecutive_failures`
and `humus.circuit.timed_out`. The bodies and the attributes are those of
humus's other clients' breaker records, so one query finds every breaker.
So an alert on `circuit breaker open` says the cluster kept failing, and the
counts say how; an alert on a metric, `humus.circuit.state`, is in
[humus's own instruments](#humuss-own-instruments).

**franz-go's own logs** go through the same `LoggerProvider`, under the scope
`github.com/twmb/franz-go/pkg/kgo`, and never to standard error or the `log`
package's default logger. The `Producer`'s client logs there too, as
the Service's does. They include a broker it can't reach, which it logs on
every attempt while it retries, a rebalance, and a retried request. Each
of franz-go's messages is one record: the message is its body, and
franz-go's key-value pairs are its attributes, where a string, a number or a
bool keeps its type, an error is its text, and any other value is its `fmt`
text. franz-go's levels are its severities: error is ERROR, warn is WARN,
info is INFO and debug is DEBUG.

The Service and the `Producer` ask the `Logger` of that scope which
severities it is enabled for, and franz-go builds no message below the lowest of them. So the end
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
shows up in three places:

- **kotel's `messaging.kafka.connect_errors.count`** counts each failed
  attempt, by broker, so a consumer that never connects shows up in its
  metrics.
- **franz-go's logs** record each attempt at WARN, as
  `unable to open connection to broker`, with the broker's address and the
  error.
- **Its readiness**, through [`Joined`](#readiness): a consumer that has
  never joined its group is not ready, so a rollout whose new pods can't
  reach the brokers stalls.

## Readiness

**A consumer is ready once it has joined its group.** `Joined` returns a
`health.Monitor` that is unhealthy until franz-go first hands the member an
assignment, empty or not, and healthy from then until `Serve` returns. It
goes in the readiness of whatever exposes the probes: a probes server, such
as `httphealth.NewService` in a pod with no server of its own, or a
[health Service](/humus/pr-preview/pr-1096/health/#the-health-service):

```go
probes := health.NewService(health.Readiness(consumer.Joined()))
```

`httphealth.NewService` takes the same `health.Readiness(consumer.Joined())`
option, and is how a consumer's pod, which has no server of its own, serves
its probes: Health shows
[the `Init`](/humus/pr-preview/pr-1096/health/#a-consumers-readiness) and
[the manifest](/humus/pr-preview/pr-1096/health/#manifests). A rollout whose new pods
can't reach the brokers then stalls, and is
reported failed at its progress deadline, rather than replacing every old pod
with pods that never consume. A member assigned no partition, because
the group has more members than the topics have partitions, has joined, and
is ready. `Joined` may be called in `Init`, before `Serve`, and its monitor
never returns an error.

**A broker outage after the first join doesn't make a consumer unready.** The
outage hits every pod at once, so taking them all out of service would gain
nothing, and franz-go rejoins the group on its own once the brokers are
back. Neither does a rebalance, nor a fenced-out member rejoining.

**`Joined` is for readiness only.** A liveness monitor would restart every
pod in a broker outage, and the kubelet's restart backoff, up to five
minutes by default, would then hold them down after the brokers return,
while franz-go rejoins on its own. The errors a restart would fix, an error
code franz-go doesn't retry and credentials a broker refuses, already end
`Serve`, so the pod exits and is restarted without a probe.

**A `Producer` whose brokers are unreachable fails its calls.** Each call
waits out its `DeliveryTimeout` while franz-go retries the connection, and
fails with an error matching `context.DeadlineExceeded`. Once enough have
failed, the [circuit breaker](#the-circuit-breaker) opens, and calls fail at
once until a probe gets through. A `Processor` that produces fails its
record, so under `AtLeastOnce` the Service stops, and the record is
processed again once the App restarts.

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

## Failure injection

A Producer is an injection point per topic, `kafka:<topic>`, such as
`kafka:orders`: a request or job whose baggage carries a failure scenario
can make its produce calls to one topic fail or slow down. An abort fails
every record of the call unsent, and the breaker counts it a failure, as for
a cluster that is down. The Producer strips the scenario from the records'
headers, unless `ProducerSettings.ForwardScenario` is set. A Service strips
the scenario a record carries, before `Process` sees it, unless
`Settings.TrustScenario` trusts the record, such as by its topic, which the
cluster's ACLs decide who can write to. When it is nil, the default, no
record is trusted. Without `humus.WithFaults`, nothing is evaluated or
stripped. [Failure injection](/humus/pr-preview/pr-1096/failure-injection/) explains
the model, whom to trust and how, and how to test a scenario through a
record's `baggage` header.

## Testing

A consumer is tested whole, with
[`humustest.Start`](https://pkg.go.dev/github.com/z5labs/humus/humustest#Start),
inside `testing/synctest`, as [Testing a humus App](/humus/pr-preview/pr-1096/testing/)
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

### What a Producer stored

A `Producer` is tested the same way, through the end user's `Init`, against
the cluster on the test Host. The cluster seeds the topic the `Producer`
produces to, `kfake.SeedTopics(1, dlqTopic)` here for `orders.dlq`, and the
test reads it back with a client of its own, after the App has exited:

```go
// readAll returns every record of topic, in each partition's order,
// through a client that dials h with opts. It polls until a poll finds
// nothing new for a second, which inside a bubble takes no real time, and
// fails the test on any error but the poll's own deadline, so a client that
// can't read the topic isn't mistaken for an empty one.
func readAll(t *testing.T, h *humustest.Host, c *kfake.Cluster, topic string, opts ...kgo.Opt) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(append([]kgo.Opt{
		kgo.SeedBrokers(c.ListenAddrs()...),
		kgo.Dialer(h.Dial),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	}, opts...)...)
	require.NoError(t, err)
	defer cl.Close()

	var out []*kgo.Record
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		fetches := cl.PollFetches(ctx)
		cancel()
		fetches.EachError(func(_ string, _ int32, err error) {
			if !errors.Is(err, context.DeadlineExceeded) {
				require.NoError(t, err, "reading %s", topic)
			}
		})
		n := 0
		fetches.EachRecord(func(r *kgo.Record) {
			out = append(out, r)
			n++
		})
		if n == 0 {
			return out
		}
	}
}
```

This test produces an order that isn't JSON and one that is, and checks that
the first became a dead letter, and that both were committed:

```go
st := <-status
require.Zero(t, st.Code(), "cause: %v", st.Cause())
require.Equal(t, "stored order key-1: 1\n", p.Stdout(), "the dead letter didn't stop the partition")
require.Equal(t, int64(2), committed(c)[0], "the dead letter's record is committed too")
dead := readAll(t, h, c, dlqTopic)
require.Len(t, dead, 1)
require.Equal(t, "key-0", string(dead[0].Key))
require.Equal(t, "{", string(dead[0].Value))
require.Len(t, spansNamed(tel, "send "+dlqTopic), 1)
```

**Wait for the commit, not only for `synctest.Wait`.** `Wait` returns as
soon as every other goroutine in the bubble is durably blocked, and a
`Produce` waiting on one of franz-go's timers is, while fake time passes only
once the test's goroutine blocks too. So `Wait` can return while the dead
letter is still on its way, and a SIGTERM then finds the next record
unprocessed. The test waits for the condition it needs instead, the offset
it expects committed, letting fake time pass meanwhile, which costs it
nothing:

```go
// waitCommitted lets fake time pass until the group has committed offset on
// partition 0 of topic, failing the test if a minute of it passes first.
// synctest.Wait isn't enough once a Processor produces: Wait returns as
// soon as every other goroutine is durably blocked, and a Produce waiting
// on one of franz-go's timers is, while fake time passes only once the
// test's goroutine blocks too.
func waitCommitted(t *testing.T, c *kfake.Cluster, offset int64) {
	t.Helper()
	for start := time.Now(); committed(c)[0] < offset; time.Sleep(10 * time.Millisecond) {
		require.Less(t, time.Since(start), time.Minute, "offset %d is never committed", offset)
	}
}
```

A `Producer` that is the whole of an App, such as one called from a job, is
tested the same way: `queue/kafka`'s own tests run each `Produce` inside a
[job](/humus/pr-preview/pr-1096/jobs/) whose `Init` builds the `Producer`.

### The Service contract

`queue/kafka`'s own tests hold `kafka.Service` to the Service contract with
[`humustest.TestService`](/humus/pr-preview/pr-1096/testing/#checking-a-service), and a
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
| `queue.ErrEndOfQueue` | removed; a finite queue is a [job](/humus/pr-preview/pr-1096/jobs/) |
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
