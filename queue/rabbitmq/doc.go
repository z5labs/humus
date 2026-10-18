// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package rabbitmq consumes RabbitMQ queues with humus's lifecycle,
// implementing the contract of package queue with amqp091-go, RabbitMQ's
// own Go client, and publishes to RabbitMQ with publisher confirms.
//
// It is in the nested module github.com/z5labs/humus/queue/rabbitmq, so
// amqp091-go stays out of the module graph of end users who don't use
// RabbitMQ. The module is always built against the core module in the same
// commit.
//
// # Running on humus.Main
//
// The end user's Init sources the [Settings], builds the queue.Processor,
// and returns the [Service] that [NewService] builds:
//
//	func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
//		settings, err := readSettings(ctx, s) // rabbitmq.Settings, sourced with config.Struct
//		if err != nil {
//			return nil, err
//		}
//		svc, err := rabbitmq.NewService(s, settings, queue.ProcessorFunc[rabbitmq.Message](h.Handle))
//		if err != nil {
//			return nil, err
//		}
//		return []humus.Service{svc}, nil
//	}
//
// The Service runs on humus.Main's defaults, which are a consumer's: a 20s
// grace period, and no drain, because nothing load-balances a consumer. So
// the package has no App and no Main of its own. A Service consumes one
// queue, with one Delivery and one Processor, and its acks are on the one
// channel it consumes on, so a failure stops only its own queue. Two queues,
// or two Delivery values, are two Services returned from one Init.
//
// NewService opens no connection and registers no OnStop function. Serve
// connects, as Connecting describes, and consumes.
//
// The Service serves no health probes of its own. A consumer's pod serves
// them on a probes server, as Readiness shows.
//
// # Delivery
//
// The Service never consumes with auto-ack, even under
// [queue.AtMostOnce]. RabbitMQ ignores the prefetch count of an auto-ack
// consumer and sends as fast as TCP allows, and amqp091 buffers every
// delivery without a bound: a backlog would fill the process's memory, and
// a crash or a rollout would lose everything buffered, already acked. So
// every delivery is acked by the Service, and Prefetch always bounds what it
// holds.
//
// Under [queue.AtLeastOnce], a delivery is acked once Process returns nil
// for it. A delivery whose processing failed, or was cut short by a crash or
// by the grace period running out, is delivered again, so a Processor under
// AtLeastOnce must be idempotent. When Process returns an error:
//
//   - the delivery is rejected with requeue, with basic.reject;
//   - the Service stops as on shutdown: the other workers finish their
//     deliveries and ack them, and every delivery received but not
//     processed is nacked with requeue, in one basic.nack;
//   - Serve returns an error that wraps the Processor's and names the queue,
//     and the message's ID when it has one, so the App exits 1, and the
//     broker delivers the message again when it restarts.
//
// A failure rejects the one delivery and nacks the rest because RabbitMQ
// counts a basic.reject, and a channel or connection that closes with a
// delivery unacked, toward a quorum queue's delivery limit, and doesn't count
// a basic.nack. So exactly the message that failed is counted, and a message
// that was merely prefetched when another failed never is. A message that
// always fails crash-loops the pod until the queue's delivery limit
// dead-letters or drops it: 20 deliveries on a quorum queue, by default
// since RabbitMQ 4.0. A classic queue has no delivery limit, and the crash
// loop is what shows the failure.
//
// Under [queue.AtMostOnce], the Service takes a delivery and every delivery
// already buffered behind it, up to Prefetch, acks them with one basic.ack
// with multiple set, and waits for the reply to a basic.qos on the same
// channel. RabbitMQ handles a channel's methods in order, so the reply means
// it has the ack, which has no reply of its own. Only then does the Service
// hand the batch to its workers, and it processes all of it. A message is
// never processed twice: if the connection drops before the reply, the batch
// isn't processed, and the broker has either dropped it or requeues it for
// another consumer. A crash loses at most one batch, and a clean shutdown
// loses nothing. A processing error is logged at ERROR, and processing goes
// on.
//
// # Order and concurrency
//
// A queue delivers its messages to one consumer in order. The Service hands
// them, in that order, to Concurrency workers, each of which processes one
// at a time, on the one channel it consumes on, so the Service has one
// prefetch window and one stream of acks. With Concurrency 1, the default,
// messages are processed in the order the queue delivered them. Three things
// break that order, as for any RabbitMQ consumer: a redelivery, more than
// one replica, and Concurrency above 1. A Processor that needs the messages
// of a key processed in order uses a single-active-consumer queue, which is
// topology the end user sets up, and Concurrency 1.
//
// # Shutdown
//
// A Service gets the lifecycle's two signals. When Serve's context is done,
// or Shutdown is called, the Service:
//
//  1. cancels its consumer, with basic.cancel, so the broker sends nothing
//     more;
//  2. hands out no more deliveries, except the rest of an AtMostOnce batch
//     it already acked;
//  3. waits for the deliveries in flight to finish, and acks them;
//  4. nacks with requeue, in one basic.nack with multiple set, every
//     delivery it received but didn't process, which counts nothing against
//     them;
//  5. closes the channel and the connection, and Serve returns nil.
//
// The context Process is given keeps the values of Serve's context, and is
// not cancelled when shutdown starts, so a Processor that honours it doesn't
// fail the delivery in flight on every SIGTERM. It is cancelled when
// Shutdown's context is done, which the lifecycle bounds by the grace
// period: then Shutdown cancels it, closes the connection at once, waits for
// Serve to return, and returns an error wrapping its context's error, which
// the lifecycle reports as a *humus.GracePeriodError. The broker then
// requeues every delivery that wasn't acked, and each counts toward a
// quorum queue's delivery limit.
//
// # Failures
//
// No panic is recovered: a panicking Processor crashes the process. Every
// other failure follows this table.
//
//	|                                | AtLeastOnce                          | AtMostOnce                           |
//	|--------------------------------|--------------------------------------|--------------------------------------|
//	| Process returns an error       | rejected, which counts toward the    | already acked; logged at ERROR, with |
//	|                                | delivery limit; the Service stops as | the queue and the message's ID;      |
//	|                                | on shutdown, and Serve returns the   | processing goes on                   |
//	|                                | error, naming the queue and the      |                                      |
//	|                                | message's ID; exit 1                 |                                      |
//	| Process panics                 | the process crashes; the broker      | the process crashes; the batch acked |
//	|                                | requeues every unacked delivery, and | is lost                              |
//	|                                | each counts                          |                                      |
//	| the broker is unreachable      | the Service keeps trying to connect, | the same                             |
//	|                                | logging each attempt at WARN         |                                      |
//	| the broker refuses the login,  | the Service stops, and Serve returns | the same                             |
//	| the vhost or the queue         | the error; exit 1                    |                                      |
//	| the connection is lost after   | the deliveries in flight finish,     | the same; a batch whose ack was      |
//	| consuming started              | their acks fail, and the broker      | confirmed is processed               |
//	|                                | delivers them again; the Service     |                                      |
//	|                                | logs it at WARN and reconnects       |                                      |
//	| the broker closes the channel  | the Service stops, and Serve returns | the same                             |
//	| with 406 PRECONDITION_FAILED   | the error; exit 1                    |                                      |
//
// The login or the vhost is refused with ACCESS_REFUSED, or amqp091's
// ErrCredentials, ErrSASL or ErrVhost. The queue is refused with 404
// NOT_FOUND, 403 ACCESS_REFUSED, or 405 RESOURCE_LOCKED for a queue another
// connection consumes exclusively. An error that ends the Service is
// returned rather than logged, and the lifecycle logs it.
//
// The Service logs through the LoggerProvider of the Setup's telemetry,
// under the scope github.com/z5labs/humus/queue/rabbitmq: at INFO,
// consuming, on each connection, with the queue and server.address; at WARN,
// each failed attempt to connect, with the error, and each lost connection,
// with the AMQP reply code as error.type and its reason as
// exception.message when the broker gave one; and at ERROR, each processing
// error under AtMostOnce, with its delivery's span context.
//
// # Telemetry
//
// The Service reports through the Setup's telemetry and never through
// OpenTelemetry's globals: its tracer and meter come from the SDK's
// TracerProvider and MeterProvider, under the scope
// github.com/z5labs/humus/queue/rabbitmq, and its propagator is the SDK's
// Propagator. Their names and attributes are OpenTelemetry's semantic
// conventions for RabbitMQ, as semconv v1.43.0 has them. The Publisher
// reports the same way, as Publishing describes.
//
// Each delivery is processed under a span of its own, named "process
// <queue>", of kind CONSUMER, whose context Process is given. The span's
// parent is the trace context the propagator extracts from the message's
// headers, so a publisher's trace continues through the consumer, and the
// publisher's baggage, and only the publisher's, is on Process's context
// too. Only a header whose value is a string is read: a traceparent header
// of any other type, such as bytes, is ignored. A message whose headers
// carry no trace context starts a new trace. The span's attributes are:
//
//   - messaging.system (rabbitmq), and messaging.operation.type and
//     messaging.operation.name, both process;
//   - messaging.destination.name, as semconv defines it for RabbitMQ:
//     {exchange}:{routing key}:{queue}, leaving out what is empty, and the
//     routing key when it equals the queue's name, so a message published
//     to the queue orders on the default exchange is orders, and one
//     published to the exchange shop with the routing key order.created is
//     shop:order.created:orders;
//   - messaging.rabbitmq.destination.routing_key, when the routing key isn't
//     empty, and messaging.rabbitmq.message.delivery_tag;
//   - messaging.message.id, and messaging.message.conversation_id, from the
//     correlation ID, when they are set;
//   - server.address and server.port, from Connection.Addr.
//
// The message's body is never recorded. When Process returns an error,
// under either Delivery, the error is recorded on the span as an exception
// event, with error.type and an error status.
//
// The Service records two instruments of OpenTelemetry's messaging
// conventions:
//
//   - messaging.client.consumed.messages counts the deliveries handed to the
//     Processor;
//   - messaging.process.duration is how long each Process took, in seconds,
//     with error.type when it failed.
//
// Each has messaging.system, messaging.operation.name (process),
// messaging.destination.name, server.address and server.port. The broker
// sends no reply to an ack, so settling a delivery has no duration, and
// the Service records no messaging.client.operation.duration.
//
// Because semconv's destination name includes the routing key, each routing
// key a delivery arrives with is a series of its own in both instruments. A
// publisher that puts identifiers, such as an order's ID, in its routing
// keys gives the consumer a series per key, which a metrics backend may
// struggle with; such a publisher's routing keys belong in headers, or in
// the body, instead.
//
// # Settings
//
// [Settings] holds what configures a consumer: how the broker is reached,
// in its [Connection], the queue it consumes, how deliveries are acked, and
// how many are held and processed at once. Every field is a plain value,
// [DefaultSettings] gives the package's defaults, and each checked field
// has a check that reports what is wrong with a value without naming the
// field. The package reads no environment variable and no file: the end
// user sources the settings in their Init, with whatever variable names
// they choose, and runs each field's check as they do:
//
//	d := rabbitmq.DefaultSettings()
//	settings, err := config.Struct(func(r *config.Resolver) rabbitmq.Settings {
//		k := d
//		// No .Default: the address, the login and the queue are required.
//		k.Connection.Addr = r.Get(env("RABBITMQ_ADDR").Validate(rabbitmq.CheckAddr))
//		k.Connection.SASL = &amqp.PlainAuth{
//			Username: r.Get(env("RABBITMQ_USERNAME")),
//			Password: r.Get(env("RABBITMQ_PASSWORD")),
//		}
//		k.Queue = r.Get(env("RABBITMQ_QUEUE").Validate(rabbitmq.CheckQueue))
//		k.Prefetch = r.Get(env("RABBITMQ_PREFETCH").Parse(strconv.Atoi).Default(d.Prefetch).Validate(rabbitmq.CheckPrefetch))
//		return k
//	}).Read(ctx)
//
// Here env is the end user's own helper, which names a variable, and amqp
// is github.com/rabbitmq/amqp091-go. A variable that isn't set fails as not
// found, and a value that fails its check fails Init, naming the variable,
// so the process exits 78.
//
// Connection.Addr, Connection.SASL and Queue have no default, so the end
// user always sources them, with no .Default in their chains.
// DefaultSettings leaves them "", nil and "", which their checks reject.
// humus imposes no port, and AMQP has no anonymous login, so there is no
// login to default to.
//
// humus takes no URL, and [CheckAddr] rejects one without quoting it, so a
// login in it never reaches the logs. An end user who keeps an AMQP URI
// parses it with amqp.ParseURI in their Init and fills the Connection from
// it:
//
//   - Addr is net.JoinHostPort(u.Host, strconv.Itoa(u.Port)), which
//     brackets an IPv6 host;
//   - Vhost is u.Vhost;
//   - SASL is u.PlainAuth(). ParseURI fills in guest and guest when the URI
//     carries no login, so an Init that requires one checks u.Username
//     itself;
//   - an amqps URI, whose port defaults to 5671, means TLS must be set: a
//     nil TLS would dial a TLS port in plain text.
//
// A nil TLS is plain text, because humus can't control what a service
// connects to. Set, the broker is dialled with it, and an empty ServerName
// defaults to Addr's host. A client certificate goes in the config's
// Certificates, with SASL set to amqp091's ExternalAuth. TLS has no check.
//
// Heartbeat and ConnectTimeout keep amqp091's defaults, 10s and 30s.
// Heartbeats can't be turned off: a dead connection would then go unnoticed
// until TCP gave up.
//
// Prefetch is 10 and Concurrency is 1, so by default a queue's deliveries
// are processed in the order the broker sent them, the round trip of each
// ack is hidden behind nine more deliveries, and at most ten messages are
// held back from the queue's other consumers. Concurrency above Prefetch
// would leave workers that never get a delivery.
//
// Delivery is the end user's design decision rather than an operator's:
// whether a message may be processed twice or lost is a property of the
// Processor, not of a deployment. So most Inits set it in code, and
// DefaultSettings gives the safe one, [queue.AtLeastOnce]. It is checked
// with [queue.CheckDelivery].
//
// # Connecting
//
// The Service and the Publisher connect to the broker the same way, in
// three steps, and ConnectTimeout bounds all three together, as does the
// context they connect under:
//
//  1. they dial Connection.Addr with the Setup's Host, so the broker is
//     reached only through the Host, and a test can run them on a Host of
//     its own;
//  2. with TLS set, they run the TLS handshake over that connection, with a
//     clone of the config whose ServerName is Addr's host when the config
//     leaves it empty, so the end user's config is never changed;
//  3. they run the AMQP handshake with amqp.Open, logging in with SASL,
//     opening Vhost and proposing Heartbeat.
//
// A step that fails closes the network connection. One that fails because
// ConnectTimeout ran out reports context.DeadlineExceeded, and one cut short
// by its context reports the context's error, never amqp091's
// ErrCredentials or ErrVhost, which amqp091 returns for any failure at those
// points of the handshake. Otherwise a wrong login fails with ErrCredentials,
// and a vhost the login can't open with ErrVhost.
//
// Serve makes its attempts to connect, and to consume on the connection,
// within a retry.Retrier from github.com/z5labs/bedrock/retry: exponential
// backoff from 100ms up to 30s, with no Budget and no limit on attempts, so
// only Serve's context ends them. Each failed attempt is logged at WARN, so
// a broker that is down at startup is waited for. A login, vhost or queue
// the broker refuses is not retried: Serve returns the error. The same
// Retrier connects again after a lost connection, as Connection loss
// describes.
//
// The Service consumes with manual acks and a consumer tag of its own, so
// amqp091 never builds one from os.Args[0]. Consumer tags are scoped to a
// channel, and the Service consumes once on each channel it opens, so its
// fixed tag never collides.
//
// humus builds no URL, and never calls amqp091's Dial, DialConfig or
// DialTLS, which take one, so the password in SASL can't reach a log in
// one. Nor does it set amqp091's logger, which is a package-level variable.
//
// # Connection loss
//
// A connection is lost when the network drops it, when it is silent for 1.5
// heartbeat intervals, or when the broker closes it. A node that shuts down
// closes every connection with 320 CONNECTION_FORCED, so a rolling upgrade
// of RabbitMQ loses every consumer's connection once per node. The broker
// cancelling the consumer, because its queue was deleted, is handled as a
// lost connection too. When the connection or the channel closes, the
// Service:
//
//  1. lets the deliveries in flight finish. Their acks fail in the client,
//     and the Service drops those errors: the broker already requeued the
//     deliveries, and delivers them again;
//  2. hands out nothing more: what the consumer had buffered was requeued
//     too, except the rest of an AtMostOnce batch whose ack was confirmed,
//     which is processed;
//  3. logs the lost connection at WARN, with the AMQP reply code and reason
//     when the broker gave one;
//  4. connects again, as Connecting describes, with the same Retrier and
//     its backoff, logging each failed attempt at WARN, and consumes again.
//
// So the Service rides out a rolling upgrade: it connects to a node that is
// up, or waits for one, and its pod neither restarts nor lands in
// Kubernetes' crash-loop backoff. What a lost connection costs is
// redeliveries. Every delivery that was unacked is delivered again, with
// Redelivered set, and RabbitMQ counts each one toward a quorum queue's
// delivery limit, as it does for any connection that closes with deliveries
// unacked. A delivery is never processed twice by one connection: the
// Service connects again only once the old one's deliveries are done.
//
// Some errors aren't fixed by connecting again, and stop the Service
// instead, with Serve returning the error, so the App exits 1:
//
//   - the refusals of Connecting: a login, a vhost, or a queue the broker
//     refuses, such as the 404 NOT_FOUND the reconnect after a deleted queue
//     meets;
//   - a channel the broker closes with 406 PRECONDITION_FAILED, as it does
//     for a delivery held unacked past its consumer_timeout, 30 minutes by
//     default. Connecting again would only hand the same Processor the same
//     delivery.
//
// Shutdown while the Service connects again stops it at once, waiting for
// no backoff, and Serve returns nil.
//
// amqp091's experimental recovery, Config.Recovery since v1.15.0, isn't
// used. It is experimental, it redials through DialConfig's URL, which humus
// never builds, rather than through the Host, and it declares the topology
// again, which is the end user's. The Service's reconnect is a short loop of
// the Retrier it connects with at startup, which humus tests.
//
// # Readiness
//
// Nothing routes traffic to a consumer, so its readiness only gates
// rollouts. A login, vhost or queue the broker refuses already ends the
// Service, and the pod's crash loop stops the rollout. A wrong address
// doesn't: the Service keeps retrying, as it does through a broker's
// upgrade, and without a monitor its pod would turn ready and replace a pod
// that consumed.
//
// [Service.Consuming] closes that gap. It returns a health.Monitor that is
// unhealthy until the broker first answers the Service's basic.consume, and
// healthy from then until Serve returns, through any number of lost
// connections and reconnects. It goes in the readiness of whatever serves
// the pod's probes. A consumer's pod has no server of its own, so that is a
// probes server made with httphealth.NewService:
//
//	consumer, err := rabbitmq.NewService(s, settings, queue.ProcessorFunc[rabbitmq.Message](h.Handle))
//	if err != nil {
//		return nil, err
//	}
//	probes, err := httphealth.NewService(s, probeSettings, health.Readiness(consumer.Consuming()))
//	if err != nil {
//		return nil, err
//	}
//	return []humus.Service{consumer, probes}, nil
//
// A new pod is then held not ready until it has consumed from its queue
// once, so a rollout whose new pods have a wrong broker address stalls, and
// is reported failed at its progress deadline, rather than replacing every
// pod that consumes with pods that don't. A lost connection after that
// doesn't make a consumer unready: a broker outage hits every pod at once,
// and taking them all out of service would change nothing but block the
// rollout that might fix it.
//
// Consuming is for readiness only. A liveness monitor would restart every
// pod in a broker outage, and the kubelet's restart backoff, up to five
// minutes by default, would then hold them down after the broker comes
// back. The Service reconnects on its own, and the errors a restart would
// fix already end Serve, as Connection loss lists, so the pod exits and is
// restarted without a probe.
//
// # Publishing
//
// A [Publisher] publishes to the broker, and returns once the broker has
// confirmed the message. The end user's Init builds it from
// [PublisherSettings] with [NewPublisher], and hands it to whatever
// publishes, such as a Processor that publishes an event, or an HTTP
// handler:
//
//	pub, err := rabbitmq.NewPublisher(s, publisherSettings) // rabbitmq.PublisherSettings, sourced with config.Struct
//	if err != nil {
//		return nil, err
//	}
//	err = pub.Publish(ctx, rabbitmq.Publishing{Exchange: "orders", RoutingKey: "accepted", Body: body})
//	if errors.Is(err, rabbitmq.ErrUnroutable) {
//		// No queue was bound to receive it.
//	}
//
// NewPublisher opens no connection. The first Publish connects, as
// Connecting describes, on a connection of the Publisher's own, separate
// from every Service's. RabbitMQ applies TCP back-pressure to a connection
// that publishes faster than the broker can take, and a consumer's acks on
// that connection would wait behind it, so RabbitMQ advises publishing and
// consuming on separate connections. Concurrent first calls connect once,
// and after the connection is lost, the next Publish connects again. The
// Publisher outlives every Service that uses it: its connection is closed by
// the function NewPublisher registers with Setup.OnStop, which runs once
// every Service has stopped, so a Processor finishing its delivery during
// the drain can still publish. After it has run, Publish fails at once.
//
// Each channel the Publisher opens is in confirm mode and carries one
// publish at a time. Each attempt of a Publish takes a channel, connecting
// first if needed, publishes the message with mandatory set, so the broker
// returns a message no queue is bound to receive rather than dropping it,
// and waits for the broker's confirm. Each attempt's result decides, with
// bedrock's retry, whether another attempt follows:
//
//   - confirmed: Publish returns nil;
//   - returned: Publish returns an error that matches [ErrUnroutable], with
//     no retry, since another attempt would be routed the same way.
//     amqp091 hands a basic.return to the channel's listener before the
//     confirm that follows it, so a return that is waiting once the confirm
//     arrives belongs to that publish;
//   - a channel error, such as 404 NOT_FOUND for an exchange that doesn't
//     exist, or 403 ACCESS_REFUSED: Publish returns the broker's
//     *amqp.Error, with no retry, since the broker refused the message, and
//     the channel is replaced;
//   - nacked by the broker, a dropped connection, a failed dial or a failed
//     handshake: the broker didn't take the message, or its answer was
//     lost, and another attempt, on a new channel or connection when the
//     old one was lost, may well be confirmed. So another attempt follows,
//     after a wait drawn from retry.Exponential(BackoffBase, BackoffMax),
//     up to [PublisherSettings].MaxAttempts in all, and Publish returns the
//     last attempt's error. 1 MaxAttempts turns retries off;
//   - the context's deadline, or the caller cancelling it: no further
//     attempt, as retry decides it, and Publish returns an error wrapping
//     the context's error.
//
// That table is fixed. An HTTP client's Classify exists because each API
// decides what its answers mean, but RabbitMQ's protocol fixes what a
// confirm, a nack, a return and a channel error mean, so the Publisher has
// no Classify setting, as Kafka's producer has none.
//
// Retries add load to a broker when it can least take it, so the retries of
// every Publish of a Publisher share one budget, [PublisherSettings].
// RetryBudget, as retry.Budget describes: each failure that would be
// retried takes a token, each confirm adds back TokenRatio of one, and no
// attempt follows a failure while half the tokens or fewer are left. While
// the broker keeps failing, each Publish makes one attempt rather than
// MaxAttempts. A first attempt is never refused.
//
// A circuit breaker, bedrock's circuit.Breaker, guards the broker, so that
// while the broker keeps failing Publish fails at once rather than hold
// every caller for Timeout: an HTTP handler that publishes an event answers
// 503 quickly during an outage, rather than after 10s. The first few
// attempts wait, and the rest fail at once until a probe gets through. One
// Publisher is one connection to one broker, with one breaker, which
// NewPublisher builds from [PublisherSettings]' Breaker and BreakerPolicy.
// By default it opens after 5 failed attempts in a row, or once half of
// the last 20 or more failed, stays open for 10s, and then lets one probe
// through at a time, closing after 3 are confirmed and reopening on one
// that fails. A BreakerPolicy whose Trip always returns false turns it off.
//
// The breaker sits inside the retries: each attempt of a Publish is one
// call through it. While it is open, an attempt sends nothing and fails at
// once with an error wrapping a *circuit.RejectedError, which matches
// circuit.ErrRejected and says how much longer the breaker stays open, and
// no attempt follows it, since the breaker would reject that one too:
//
//	err := pub.Publish(ctx, rabbitmq.Publishing{Exchange: "orders", RoutingKey: "accepted", Body: body})
//	if errors.Is(err, circuit.ErrRejected) {
//		// Nothing was sent; the *circuit.RejectedError says how long the breaker stays open.
//	}
//
// Whether to wait and try again is the caller's. The breaker counts the
// broker, not the messages or the callers, so what an attempt's result
// counts as is fixed too:
//
//   - confirmed: a success;
//   - returned, [ErrUnroutable], or a channel error, such as 404 NOT_FOUND
//     or 403 ACCESS_REFUSED: a success, since the broker answered. A
//     mistake in one exchange's bindings doesn't open the breaker for the
//     others;
//   - nacked by the broker, a dropped connection, a failed dial or a failed
//     handshake: a failure;
//   - the context's deadline, Timeout's or the caller's: a failure, since a
//     broker that has stopped answering ends every attempt this way;
//   - the caller cancelling the context: not counted, since it says
//     nothing about the broker.
//
// Inside the breaker, and before anything of the attempt is written to the
// broker, is the seam where Proposal: failure injection (wave 5) injects
// its failures, so an injected failure counts as a real one would.
//
// Each change of the breaker's state is one log record of the Publisher's,
// under the scope github.com/z5labs/humus/queue/rabbitmq, in the context of
// the Publish that made it: circuit breaker open at WARN, and circuit
// breaker half-open and circuit breaker closed at INFO. Its attributes are
// server.address and server.port, from Connection.Addr,
// circuit.previous_state, and the counts of the state the breaker left:
// circuit.successes, circuit.failures, circuit.consecutive_successes,
// circuit.consecutive_failures and circuit.timed_out. There are no
// instruments of humus's own for the breaker: its transitions are in the
// logs, and a rejected Publish is an error on its span. Whether humus
// records instruments of its own is Proposal: humus's own metrics (wave 5).
//
// A retry can publish a message twice: when a connection is lost after the
// broker took the message but before its confirm arrived, the attempt that
// follows publishes it again. So publishing is at least once, and a
// consumer of the message is idempotent anyway, under [queue]'s contract.
//
// [PublisherSettings].Channels bounds the publishes in flight at once: an
// attempt waits for a channel when every one is busy, and gives its
// channel back before any wait for the next. [PublisherSettings].Timeout
// bounds the whole Publish, every attempt, the waits between them and each
// confirm included, and a deadline the caller's context already has wins
// when it is earlier. No attempt starts after it, and a wait that would
// end after it isn't begun. When the context is done during an attempt,
// the channel isn't reused, since a confirm may still arrive on it.
//
// A [Publishing]'s zero value is persistent, with delivery mode 2, so a
// message isn't lost on a broker restart because a field was left out.
// [Publishing].Transient makes it delivery mode 1. Every other field is
// passed to the broker as it is, and the caller's Headers map is never
// changed.
//
// The Publisher reports through the Setup's telemetry, as the Service does,
// under the same scope. Each Publish has a span of its own, a child of the
// span on the caller's context, named "send <exchange>", or "send
// amq.default" for the default exchange, of kind PRODUCER. The span is
// started before the message's headers are written, and the Setup's
// propagator injects its trace context, and the caller's baggage, into a
// copy of the headers, so the consumer's "process <queue>" span continues
// the publisher's trace without the end user's code touching the headers.
// A nil Headers map gets a new one. The span's attributes are:
//
//   - messaging.system (rabbitmq), and messaging.operation.type and
//     messaging.operation.name, both send;
//   - messaging.destination.name, as semconv defines it for a RabbitMQ
//     publish: {exchange}:{routing key}, leaving out what is empty, so a
//     message published to the default exchange with the routing key orders
//     is orders, and one published to the exchange shop with the routing key
//     order.created is shop:order.created;
//   - messaging.rabbitmq.destination.routing_key, when the routing key isn't
//     empty;
//   - messaging.message.id, and messaging.message.conversation_id, from the
//     correlation ID, when they are set;
//   - server.address and server.port, from Connection.Addr.
//
// The message's body is never recorded. A failed Publish is recorded on
// its span as an exception event, with error.type and an error status, a
// Publish the breaker rejected included.
//
// Each attempt to publish records two instruments of OpenTelemetry's
// messaging conventions:
//
//   - messaging.client.sent.messages counts the messages published;
//   - messaging.client.operation.duration is how long the broker took to
//     confirm each, from the publish to its confirm, in seconds.
//
// Each has messaging.system, messaging.operation.name (send),
// messaging.destination.name, server.address and server.port, and
// error.type when the attempt failed. A return and a channel error have the
// broker's AMQP reply code as error.type, such as 312 for a return and 404
// for an exchange that doesn't exist, and a nack has nacked. A Publish has
// one span, whatever its attempts, and a nack followed by a confirm counts
// two messages sent, one with error.type nacked. An attempt that fails
// before it publishes, waiting for a channel or connecting, publishes
// nothing, so neither instrument records it, and neither does an attempt
// the breaker rejected. As with the
// Service, a routing key that carries identifiers makes a series per key in
// both instruments.
//
// # Topology
//
// The Service and the Publisher declare nothing. The queue, its exchange,
// bindings, type, dead-lettering and delivery limit are the end user's
// infrastructure, set up before the Service starts, by RabbitMQ's
// definitions, an operator, or the team that owns the exchange. The
// Publisher publishes only to exchanges that exist: a missing exchange
// fails a Publish with 404 NOT_FOUND.
package rabbitmq
