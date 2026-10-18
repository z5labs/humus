// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package rabbitmq consumes RabbitMQ queues with humus's lifecycle,
// implementing the contract of package queue with amqp091-go, RabbitMQ's
// own Go client.
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
//	|                                | delivers them again; Serve returns   |                                      |
//	|                                | an error naming the queue; exit 1    |                                      |
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
// each failed attempt to connect, with the error; and at ERROR, each
// processing error under AtMostOnce.
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
// the broker refuses is not retried: Serve returns the error.
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
// # Topology
//
// The Service declares nothing. The queue, its exchange, bindings, type,
// dead-lettering and delivery limit are the end user's infrastructure, set
// up before the Service starts, by RabbitMQ's definitions, an operator, or
// the team that owns the exchange.
package rabbitmq
