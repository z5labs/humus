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
// # Readiness
//
// A consumer is ready once it has joined its consumer group. [Service.Joined]
// returns a health.Monitor that is unhealthy until franz-go first hands the
// member an assignment, empty or not, and healthy from then until Serve
// returns. It goes in the readiness of whatever exposes the probes: a probes
// server, such as httphealth.NewService in a pod with no server of its own,
// or a health Service.
//
//	probes, err := httphealth.NewService(s, probeSettings, health.Readiness(consumer.Joined()))
//
// A rollout whose new pods can't reach the brokers then stops at its
// progress deadline, with the old pods still consuming, rather than
// replacing them with pods that never consume. A member assigned no
// partition, because the group has more members than the topics have
// partitions, has joined, and is ready.
//
// A broker outage after the first join doesn't make a consumer unready. The
// outage hits every pod at once, so taking them all out of service would
// gain nothing, and franz-go rejoins the group on its own once the brokers
// are back. Neither does a rebalance, nor a fenced-out member rejoining.
//
// Joined is for readiness only. A liveness monitor would restart every pod
// in a broker outage, and the kubelet's restart backoff, up to five minutes
// by default, would then hold them down after the brokers return, while
// franz-go rejoins on its own. The errors a restart would fix, an error code
// franz-go doesn't retry and credentials a broker refuses, already end
// Serve, so the pod exits and is restarted without a probe.
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
// it twice, which is at-least-once. DeliveryTimeout is the one retry
// setting: how long a call lets franz-go keep trying. There is no retry
// count, no backoff and no retry budget, and no bedrock retry.Retrier
// around Produce, because franz-go retries, and a retry around it would do
// harm. franz-go's idempotent producer retries a batch on the sequence
// numbers that let the broker drop a batch it already stored, so a retry
// around Produce, handing the records over again under new sequence
// numbers, would store twice a record whose first send landed. franz-go
// retries a partition's batches in order, and a retry around Produce would
// send its records after whatever other calls produced in the meantime.
// And franz-go's retries don't multiply with callers: it retries a
// partition's batch, which holds every caller's records for that
// partition, once per backoff, so a thousand callers waiting on a dead
// partition cost one retry per backoff, not a thousand.
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
// A circuit breaker, bedrock's circuit.Breaker, guards the cluster, so
// that while the cluster keeps failing Produce fails at once rather than
// hold every caller for DeliveryTimeout: an HTTP handler that publishes an
// event answers 503 quickly during an outage, rather than after 10s. The
// first few calls wait, and the rest fail at once until a probe gets
// through. The dependency the breaker guards is the cluster: one Producer
// is one client to one cluster, with one breaker, which NewProducer builds
// from ProducerSettings' Breaker and BreakerPolicy. A process that produces
// to two clusters builds two Producers.
//
// Each call to Produce is one call through the breaker. It is admitted
// once each record's span has started and its headers carry the trace
// context, and before any record reaches franz-go, and it is counted once
// every record has its result, which may be after Produce returns. While
// the breaker is open, Produce returns an error wrapping a
// *circuit.RejectedError, which matches circuit.ErrRejected and says how
// much longer the breaker stays open, at once and with nothing sent. Each
// record's span records the rejection as a failure, and neither instrument
// counts the record, which was never sent. Produce doesn't retry a
// rejected call: whether to wait and try again is the caller's, as queue's
// contract leaves retries to the Processor. A call whose context is already
// done, and a call after the cluster refused the credentials, return before
// the breaker, so it counts neither.
//
// The breaker counts the cluster, not the records or the callers. It sees
// each call's result with the call's own context, which a caller giving up
// doesn't cancel, so a caller giving up never counts:
//
//   - a success is a call whose every record was acknowledged;
//   - a call is ignored, and counts as nothing, when its records failed for
//     their own reasons or their topics', not the cluster's: a Kafka error
//     code franz-go doesn't retry, such as MESSAGE_TOO_LARGE, INVALID_RECORD
//     or TOPIC_AUTHORIZATION_FAILED, or UNKNOWN_TOPIC_OR_PARTITION once
//     franz-go has given up on the topic. One topic's mistake doesn't open
//     the breaker for the others;
//   - a failure is anything else, the cluster not storing records in time:
//     DeliveryTimeout passing, kgo.ErrRecordTimeout, a retriable error code
//     franz-go stopped on, or a connection that failed.
//
// One breaker covers every partition. When one broker of three is down,
// the calls to the partitions it leads fail and the rest succeed, which the
// default policy may or may not trip on. A breaker per broker or per
// partition would follow Kafka's failures more closely, but franz-go picks
// a record's partition inside the client, after the breaker has admitted
// the call. franz-go moves unkeyed records away from backed-up partitions,
// and a keyed record has only one partition.
//
// When a broker refuses the Producer's SASL credentials, with
// SASL_AUTHENTICATION_FAILED, UNSUPPORTED_SASL_MECHANISM or
// ILLEGAL_SASL_STATE, the Producer fails every call, since it can't stop
// the App as the Service does. franz-go would otherwise retry the
// connection, and each call would wait out its DeliveryTimeout with no sign
// of why. On the first refusal the Producer emits one ERROR record, under
// the scope github.com/z5labs/humus/queue/kafka, with messaging.system,
// server.address and server.port naming the broker, and the calls waiting
// on their records return the refusal. From then on every Produce returns
// it at once, with nothing sent. The error names the broker as host:port,
// as the Service's does, and errors.Is finds its kerr code, such as
// kerr.SaslAuthenticationFailed. Credentials don't fix themselves while the
// process runs, so the Producer never tries them again: a Processor that
// produces fails its record, and under queue.AtLeastOnce the Service stops
// and the App is restarted with whatever the secret holds then.
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
// Each change of the breaker's state is one log record of the Producer's,
// under the scope github.com/z5labs/humus/queue/kafka, in the context of
// the call that made it: circuit breaker open at WARN, and circuit breaker
// half-open and circuit breaker closed at INFO. Its attributes are
// messaging.system, set to kafka, circuit.previous_state, and the counts
// of the state the breaker left: circuit.successes, circuit.failures,
// circuit.consecutive_successes, circuit.consecutive_failures and
// circuit.timed_out. There are no instruments of humus's own for the
// breaker: its transitions are in the logs, and a rejected call is an error
// on its records' spans.
//
// # Telemetry
//
// The Service and the Producer report through the Setup's telemetry and
// never through OpenTelemetry's globals: their tracers and meters come from
// the SDK's TracerProvider and MeterProvider, under the scope
// github.com/z5labs/humus/queue/kafka, and their propagator is the SDK's
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
// The Producer sends each record it hands to franz-go under a span of its
// own, named "send <topic>", of kind PRODUCER, a child of the span in
// Produce's context. The span's attributes are messaging.system (kafka),
// messaging.operation.type and messaging.operation.name (both send) and
// messaging.destination.name, and, once the record is acknowledged,
// messaging.destination.partition.id and messaging.kafka.offset. The
// record's key is not recorded. The span ends when the record is
// acknowledged or fails, as its result arrives, which may be after Produce
// returns. A failure is recorded on it as an exception event, with
// error.type and an error status.
//
// The span's context goes into the record's headers. Before injecting, the
// Producer removes every header of the record whose key is one of the
// propagator's fields, such as a dead letter's copy of the consumed
// record's traceparent. The propagator then injects the span's context and
// the baggage of Produce's context; with the default propagator, those are
// traceparent, tracestate and baggage. So the process span of a Service
// that consumes the record, on any topic, is a child of its send span, and
// inside a Process, the send span is a child of the process span: one trace
// runs from the first producer through every topic a record passes
// through.
//
// The Producer records two more instruments of the messaging conventions:
//
//   - messaging.client.sent.messages counts each record once its result is
//     known, with messaging.operation.name set to send, and error.type when
//     it failed;
//   - messaging.client.operation.duration is how long each record took,
//     from Produce taking it to its result, in seconds, with
//     messaging.operation.name and messaging.operation.type set to send,
//     and error.type when it failed.
//
// Each also has messaging.system, messaging.destination.name and, once
// franz-go has chosen it, messaging.destination.partition.id. A record
// Produce rejected for having no topic, and the records of a call whose
// context was already done, were never sent: they start no span, and
// neither instrument counts them.
//
// franz-go's broker metrics come from the Meter of franz-go's kotel plugin,
// which the Service and the Producer each build with the Setup's
// MeterProvider and nothing else, and hand their client as hooks. Its
// instruments are kotel's, under kotel's own scope,
// github.com/twmb/franz-go/plugin/kotel, with names such as
// messaging.kafka.connects.count, messaging.kafka.connect_errors.count and
// messaging.kafka.fetch_records.count. By broker, with a node_id attribute,
// they count the connections opened, the connection errors and the
// connections closed, the bytes read and written, and the read and write
// errors. By broker and topic, with node_id and topic, they count the
// records and the bytes fetched, and, for the Producer, the records and
// the bytes produced, in messaging.kafka.produce_records.count and
// messaging.kafka.produce_bytes.count. kotel's tracer is not used.
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
