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
// # Topology
//
// The Service declares nothing. The queue, its exchange, bindings, type,
// dead-lettering and delivery limit are the end user's infrastructure, set
// up before the Service starts, by RabbitMQ's definitions, an operator, or
// the team that owns the exchange.
package rabbitmq
