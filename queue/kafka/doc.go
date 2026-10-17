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
