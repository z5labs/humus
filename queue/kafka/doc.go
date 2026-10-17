// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package kafka consumes Kafka topics with humus's lifecycle, implementing
// the contract of package queue with franz-go.
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
// When the member is fenced out of the group, its partitions are lost:
// another member may own them already, so the Service stops them after
// their record in flight without committing, and their new owner processes
// the uncommitted records again.
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
//	| member was fenced out of the     | group, and the records are committed | failed is not processed              |
//	| group or the group is            | by a later commit, or processed      | logged at WARN with its offsets; if  |
//	| rebalancing                      | again by the partition's new owner   | the partition stays with the member, |
//	|                                  |                                      | the batch is lost                    |
//	| any other commit fails           | the Service stops, and Serve returns | the same                             |
//	|                                  | the error; exit 1                    |                                      |
//	| a poll returns an error franz-go | the Service stops, and Serve returns | the same                             |
//	| doesn't retry, such as an        | the error; exit 1                    |                                      |
//	| authorization failure            |                                      |                                      |
//	| a broker refuses the SASL        | the Service stops, and Serve returns | the same                             |
//	| credentials                      | the error; exit 1                    |                                      |
//	| the brokers are unreachable      | franz-go keeps retrying, and the     | the same                             |
//	|                                  | Service keeps serving                |                                      |
//
// A commit is refused for a fenced-out member with UNKNOWN_MEMBER_ID,
// ILLEGAL_GENERATION or FENCED_INSTANCE_ID, and during a rebalance with
// REBALANCE_IN_PROGRESS. A poll's error ends the Service when it is an error
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
