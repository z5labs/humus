// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package kafka consumes and produces Kafka records with humus's lifecycle,
// implementing the contract of package queue with franz-go.
//
// It is in the nested module github.com/z5labs/humus/queue/kafka, so
// franz-go stays out of the module graph of end users who don't consume
// Kafka. The module is always built against the core module in the same
// commit.
//
// # Running on humus.Main
//
// The end user's Init sources the [Settings], builds the queue.Processor,
// and returns the [Service] that [NewService] builds:
//
//	func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
//		settings, err := readSettings(ctx, s) // kafka.Settings, sourced with config.Struct
//		if err != nil {
//			return nil, err
//		}
//		svc, err := kafka.NewService(s, settings, queue.ProcessorFunc[kafka.Message](h.Handle))
//		if err != nil {
//			return nil, err
//		}
//		return []humus.Service{svc}, nil
//	}
//
// The Service runs on humus.Main's defaults, which are a consumer's: a 20s
// grace period, and no drain, because nothing load-balances a consumer. So
// the package has no App and no Main of its own. A Service has one Delivery
// and one Processor, and a Message carries its topic, so one Processor can
// dispatch on it. Two Delivery values are two Services with two group IDs,
// returned from one Init.
//
// NewService opens no connection. Serve builds the franz-go client, dials
// every broker through the Setup's Host, and joins the group.
//
// # Delivery
//
// Under [queue.AtLeastOnce], each partition processes a batch of records and
// then commits the last record it processed, which commits every record
// before it. A record is committed only once it and every earlier record of
// its partition were processed. A record whose processing failed, or was cut
// short by a crash, a rebalance or the grace period running out, is
// delivered again, so a Processor under AtLeastOnce must be idempotent.
//
// Under [queue.AtMostOnce], each partition commits a batch before it
// processes any of it, and then processes every record of the batch. A
// record is never processed twice, and the records of a batch are lost if
// the process stops before it has processed them. MaxPollRecords bounds a
// batch, and so how many records one crash can lose.
//
// # Order and concurrency
//
// Each assigned partition has one goroutine, which processes its records
// one at a time, in offset order. Partitions are processed concurrently, so
// the concurrency is the number of partitions assigned to the Service.
// Kafka orders a partition and not a topic, so records of different
// partitions are processed in no particular order.
//
// # Shutdown
//
// A Service gets the lifecycle's two signals. When Serve's context is done,
// or Shutdown is called, the Service stops polling, lets each partition's
// record in flight finish, commits, leaves the group and returns. Leaving
// makes the group rebalance at once, so the other members take the
// partitions without waiting for the session timeout. Under AtMostOnce, the
// record in flight is every record of the batch that was committed.
//
// The context Process is given keeps the values of Serve's context and is
// not cancelled when shutdown starts, so a Processor that honours it
// doesn't fail the record in flight on every SIGTERM. It is cancelled when
// Shutdown's context is done, which the lifecycle bounds by the grace
// period: then Shutdown cancels it and the leave, waits for Serve to return,
// and returns an error wrapping its context's error, which the lifecycle
// reports as a *humus.GracePeriodError. The records cut short are not
// committed.
//
// # Revocation and loss
//
// When the group rebalances and a partition moves to another member,
// franz-go revokes it first. The Service stops the partition, waits for its
// record in flight, commits, and only then lets the rebalance go on, so the
// partition's new owner starts after the last record this one processed.
// All of that has to fit in RebalanceTimeout.
//
// When the member is fenced out of the group, another member may own its
// partitions already. franz-go may report them as lost or, when it learns of
// the fence from a refused commit and rejoins the group as a new member, as
// revoked. Either way, the Service stops them after their record in flight
// and commits nothing more for them, and their new owner processes their
// uncommitted records again.
//
// Kafka checks a commit's member ID and generation, but not who owns the
// partition. So the Service sends a partition's commit only under the member
// ID the partition is held under: that of the rebalance that assigned it, or
// of a later one that kept it. A commit franz-go would send under another
// member ID, for a partition the new member wasn't given, is not sent, and
// so can't move its new owner's committed offset.
//
// The Service logs, at INFO, the partitions it is assigned, has revoked and
// loses, with the topic and the partitions as attributes, through the
// LoggerProvider of the Setup's telemetry, under the scope
// github.com/z5labs/humus/queue/kafka.
//
// # Failures
//
// No panic is recovered: a panicking Processor crashes the process. Every
// other failure follows this table.
//
//	|                                  | AtLeastOnce                          | AtMostOnce                           |
//	|----------------------------------|--------------------------------------|--------------------------------------|
//	| Process returns an error         | not committed; the Service stops as  | already committed; logged at ERROR,  |
//	|                                  | on shutdown, and Serve returns the   | with the record's topic, partition   |
//	|                                  | error, naming the record; exit 1     | and offset; processing goes on       |
//	| Process panics                   | the process crashes; nothing after   | the process crashes; the batch was   |
//	|                                  | the last commit is committed         | committed                            |
//	| a commit fails because the       | not an error: franz-go rejoins the   | the same, and the batch whose commit |
//	| member was fenced out of the     | group; a later commit covers the     | failed is not processed: it is       |
//	| group or the group is            | records only while the partition     | logged at WARN with its offsets; if  |
//	| rebalancing                      | stays with the member, and           | the partition stays with the member, |
//	|                                  | otherwise its new owner processes    | the batch is lost                    |
//	|                                  | them again                           |                                      |
//	| any other commit fails           | the Service stops, and Serve returns | the same                             |
//	|                                  | the error; exit 1                    |                                      |
//	| a poll returns an error franz-go | the Service stops, and Serve returns | the same                             |
//	| doesn't retry, such as an        | the error; exit 1                    |                                      |
//	| authorization failure            |                                      |                                      |
//	| a broker refuses the SASL        | the Service stops, and Serve returns | the same                             |
//	| credentials                      | the error; exit 1                    |                                      |
//	| the brokers are unreachable      | franz-go keeps retrying, logs each   | the same                             |
//	|                                  | failed attempt at WARN, and kotel    |                                      |
//	|                                  | counts it; the Service keeps serving |                                      |
//
// A commit is refused for a fenced-out member with UNKNOWN_MEMBER_ID,
// ILLEGAL_GENERATION or FENCED_INSTANCE_ID, and during a rebalance with
// REBALANCE_IN_PROGRESS. A commit the Service doesn't send, because the
// partition isn't held under the member ID franz-go would send it under, is
// handled the same way. A poll's error ends the Service when it is an error
// code from Kafka that franz-go doesn't retry; an error code franz-go
// retries, or a failure to connect, is franz-go's to recover from.
//
// An at-least-once failure stops the whole Service, not just its partition.
// The other partitions finish their record in flight and commit, so only
// the failed record and those after it are processed again, when the App
// restarts. An error that ends the Service is returned rather than logged,
// and the lifecycle logs it. Retries are the Processor's: a Processor that
// would rather skip a record that always fails than stop on it returns nil,
// after storing the record wherever it keeps dead letters.
//
// # Producing
//
// A [Producer] produces records to one Kafka cluster. The end user's Init
// builds it with [NewProducer], from [ProducerSettings], and any Service
// uses it: a Processor that writes to another topic or to a dead-letter
// topic, an HTTP handler that publishes an event, a job. It is not a
// Service. One Producer is safe for concurrent use, and serves every
// Service of the process:
//
//	dlq, err := kafka.NewProducer(s, ps) // ps is a kafka.ProducerSettings, sourced like Settings
//	if err != nil {
//		return nil, err
//	}
//	svc, err := kafka.NewService(s, settings, queue.ProcessorFunc[kafka.Message](func(ctx context.Context, m kafka.Message) error {
//		order, err := decode(m.Value)
//		if err != nil {
//			// A dead letter, stored before the record is committed.
//			return dlq.Produce(ctx, kafka.Record{Topic: "orders.dlq", Key: m.Key, Value: m.Value, Headers: m.Headers})
//		}
//		return store.Save(ctx, order)
//	}))
//
// ProducerSettings repeats Settings' Brokers, TLS and SASL, which mean what
// they mean there, take the same check and dial through the Setup's Host
// the same way: a Producer may produce to a cluster other than the one a
// Service consumes, so each set is sourced on its own. NewProducer builds
// one franz-go client and opens no connection: the first call does.
//
// [Producer.Produce] is synchronous. It returns nil once the cluster has
// acknowledged every record, with acks from all in-sync replicas, or an
// error once one has failed. So a Processor that produces before it returns
// nil has the consumed record committed only after the produced one is
// stored. A caller that wants more throughput puts more records in one
// call, or calls from more goroutines: a call takes the same path through
// franz-go's buffers and batches as any other, and stops the linger on the
// partitions it produced to.
//
// Records outlive a cancelled call. Produce honours its context up to the
// moment it hands its records to franz-go: a call whose context is already
// done sends nothing, and returns an error that wraps the context's error.
// After the hand-off, a context that ends returns the call at once, with
// the same error, and its records stay in the client, until the cluster
// acknowledges them or DeliveryTimeout fails them. So a handler whose
// request is cancelled returns at once, and the grace period reaches a
// Process blocked in Produce. The records don't follow the caller's
// context, because franz-go can't cancel one caller's records on their
// own: it fails a batch by the context of its first record, fails every
// record buffered behind it on that partition, whoever produced them, and
// closes the broker's produce connection when it cancels a request in
// flight. One Producer serves the whole process, so a caller giving up
// would fail another caller's records. And a record already sent is
// stored whether the client still waits for it or not.
//
// DeliveryTimeout, 10s by default, bounds how long the cluster has to
// acknowledge the records of one call, franz-go's retries included. A
// record that isn't acknowledged by then fails, even one already sent, and
// may still have been stored, so a caller that produces it again may store
// it twice, which is at-least-once. There are no retry settings: franz-go's
// idempotent producer retries a batch on the sequence numbers that let the
// broker drop a batch it already stored, and keeps each partition in order.
// A retry around Produce would hand the records over again, under new
// sequence numbers, and store twice a record whose first send landed.
// Acks from all in-sync replicas and idempotence are franz-go's defaults,
// and not settings, and so are its partitioner, which hashes keyed records
// with murmur2 as Kafka's Java producer does, its linger, compression,
// batch size and buffer size.
//
// Records to one partition are stored in the order they are given: within
// one call, in the order of its records, and between calls on different
// goroutines, in the order they reach the client. When a record of a
// partition fails, franz-go fails the records buffered after it on that
// partition too, so a later record is never stored after an earlier one
// failed.
//
// When every record was acknowledged, Produce returns nil. Otherwise it
// returns one error joining each failed record's error, with its topic,
// and its partition when one was chosen, so errors.Is finds
// kgo.ErrRecordTimeout, context.DeadlineExceeded or a kerr code in it. A
// failed call doesn't say which of its records were stored: the records
// that failed may have been stored anyway. Produce rejects a record with no
// Topic before it sends anything, and returns nil when given no records.
//
// The Producer is not a Service, so the lifecycle doesn't stop it: the
// Services that use it stop first. NewProducer registers one function with
// the Setup's OnStop, which runs once every Service has returned. It
// flushes the client with the grace period's context, which waits for the
// records still in it, such as those of a call that returned when its
// context was done, and then closes the client. When the grace period runs
// out first, closing the client fails what is left with
// kgo.ErrClientClosed, those records are lost, and the function returns an
// error that wraps the context's error, which the lifecycle reports as a
// *humus.GracePeriodError. A Produce after that, from a goroutine the end
// user left running, fails at once with an error that wraps
// kgo.ErrClientClosed. If the end user's Init fails after NewProducer, the
// lifecycle runs the function too, so the client is closed and none of
// franz-go's goroutines outlive the App.
//
// The Producer's client logs through the Setup's LoggerProvider, under the
// scope github.com/twmb/franz-go/pkg/kgo, as the Service's does.
//
// # Telemetry
//
// The Service reports through the Setup's telemetry and never through
// OpenTelemetry's globals: its tracer and meter come from the SDK's
// TracerProvider and MeterProvider, under the scope
// github.com/z5labs/humus/queue/kafka, and its propagator is the SDK's
// Propagator.
//
// Each record is processed under a span of its own, named "process
// <topic>", of kind CONSUMER, whose context Process is given. The span's
// parent is the trace context the propagator extracts from the record's
// headers, so a producer's trace continues through the consumer, and the
// producer's baggage, and only the producer's, is on Process's context too.
// A record whose headers carry no trace context starts a new trace. Under
// queue.AtMostOnce, the ERROR record of a failed record carries its span's
// context. The span's attributes are
// messaging.system (kafka), messaging.operation.type and
// messaging.operation.name (both process), messaging.destination.name,
// messaging.destination.partition.id, messaging.consumer.group.name and
// messaging.kafka.offset. The record's key is not recorded. When Process
// returns an error, under either Delivery, the error is recorded on the
// span as an exception event, with error.type and an error status.
//
// The Service records three instruments of OpenTelemetry's messaging
// conventions, as semconv v1.43.0 names them:
//
//   - messaging.client.consumed.messages counts the records handed to the
//     Processor, with messaging.operation.name set to process;
//   - messaging.process.duration is how long each Process took, in seconds,
//     with messaging.operation.name set to process, and error.type when it
//     failed;
//   - messaging.client.operation.duration is how long each commit took, in
//     seconds, with messaging.operation.name set to commit and
//     messaging.operation.type to settle, and error.type when it failed.
//
// Each also has messaging.system, messaging.consumer.group.name,
// messaging.destination.name and messaging.destination.partition.id.
//
// franz-go's broker metrics come from the Meter of franz-go's kotel plugin,
// which the Service builds with the Setup's MeterProvider and nothing else,
// and hands the client as hooks. Its instruments are kotel's, under kotel's
// own scope, github.com/twmb/franz-go/plugin/kotel, with names such as
// messaging.kafka.connects.count, messaging.kafka.connect_errors.count and
// messaging.kafka.fetch_records.count. By broker, with a node_id attribute,
// they count the connections opened, the connection errors and the
// connections closed, the bytes read and written, and the read and write
// errors. By broker and topic, with node_id and topic, they count the
// records and the bytes fetched. kotel's produce counters stay empty,
// because the Service produces nothing. kotel's tracer is not used.
//
// humus renames neither set of instruments. Their names are semconv's and
// kotel's, at the versions this module pins, so a change to the names of
// either, through a semconv or a kotel upgrade, is a breaking change to
// this package.
//
// Against kfake, franz-go's fake cluster, a clean shutdown counts one read
// error for the broker, during the shutdown and never before it. An alert on
// messaging.kafka.read_errors.count should allow for it.
//
// franz-go's own logs go through the same LoggerProvider, under the scope
// github.com/twmb/franz-go/pkg/kgo, and never to standard error or the log
// package's default logger. They include a broker it can't reach, which it
// logs on every attempt while it retries, a rebalance and a retried
// request. Each of franz-go's messages is one record: the message is its
// body, and franz-go's key-value pairs are its attributes, where a string,
// a number or a bool keeps its type, an error is its text, and any other
// value is its fmt text. franz-go's levels are its severities: error is
// ERROR, warn is WARN, info is INFO and debug is DEBUG.
//
// The Service asks the Logger of that scope which severities it is enabled
// for, and franz-go builds no message below the lowest of them. A
// LoggerProvider with no severity filter is enabled for every severity, so
// with one franz-go logs at DEBUG: a record for every request it writes and
// every response it reads. The end user's Builder quiets franz-go as it does
// any scope: with a
// LoggerProvider whose Logger for github.com/twmb/franz-go/pkg/kgo is
// enabled only from, say, WARN, franz-go logs nothing at INFO or DEBUG, and
// with one enabled for nothing, franz-go logs nothing at all.
//
// # Settings
//
// [Settings] holds what configures a consumer: the brokers, the consumer
// group and the topics, how records are delivered, how the brokers are
// dialled and authenticated, and the group's timeouts and fetch bounds.
// Every field is a plain value, [DefaultSettings] gives the package's
// defaults, and each checked field has a check that reports what is wrong
// with a value without naming the field. The package reads no environment
// variable and no file: the end user sources the settings in their Init,
// with whatever variable names they choose, and runs each field's check as
// they do:
//
//	d := kafka.DefaultSettings()
//	settings, err := config.Struct(func(r *config.Resolver) kafka.Settings {
//		k := d
//		// No .Default: Brokers, GroupID and Topics are required, so an unset variable fails as not found.
//		k.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
//		k.GroupID = r.Get(env("KAFKA_GROUP_ID").Validate(kafka.CheckGroupID))
//		k.Topics = r.Get(env("KAFKA_TOPICS").Map(list).Validate(kafka.CheckTopics))
//		k.SessionTimeout = r.Get(env("KAFKA_SESSION_TIMEOUT").Parse(time.ParseDuration).Default(d.SessionTimeout).Validate(kafka.CheckSessionTimeout))
//		return k
//	}).Read(ctx)
//
// Here env and list are the end user's own helpers: env names a variable,
// and list splits a comma-separated value. A variable that isn't set fails
// as not found, and a value that fails its check fails Init, naming the
// variable, so the process exits 78.
//
// Brokers, GroupID and Topics have no default, so the end user always
// sources them, with no .Default in their chains. DefaultSettings leaves
// them nil and "", which their checks reject.
//
// Delivery is the end user's design decision rather than an operator's:
// whether a record may be processed twice or lost is a property of the
// Processor, not of a deployment. So most Inits set it in code, and
// DefaultSettings gives the safe one, [queue.AtLeastOnce]. It is checked
// with [queue.CheckDelivery].
//
// TLS and SASL have no check, and nil turns each off: plaintext, and no
// authentication. TLS covers mTLS: the client certificate goes in the
// config's Certificates. For SASL the end user picks a mechanism from
// franz-go's github.com/twmb/franz-go/pkg/sasl, such as plain, scram, oauth
// or aws, and supplies its credentials from wherever they keep them.
//
// SessionTimeout and FetchMaxBytes keep franz-go's defaults, 45s and 50 MiB.
// RebalanceTimeout is 60s, franz-go's default, because a revoked
// partition's record in flight has to finish within it before the
// partition moves to another member. MaxPollRecords, 500, bounds how many
// records one poll hands out, which bounds how many records an
// at-most-once poll can lose, and how long a revoke waits for the records
// in flight.
package kafka
