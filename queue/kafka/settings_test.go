// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// fieldNames matches words a check's error must not contain, in any case:
// a check reports what is wrong, and whoever calls it names the field or
// the variable.
var fieldNames = regexp.MustCompile(`(?i)\b(?:settings|brokers?|group ?id|topics?|delivery|tls|sasl|` +
	`session ?timeout|rebalance ?timeout|fetch ?max ?bytes|max ?poll ?records)\b`)

// requireUnnamed fails the test if err names a field of Settings.
func requireUnnamed(t *testing.T, err error) {
	t.Helper()
	require.NotRegexp(t, fieldNames, err.Error(), "the error names a field")
}

// checkCase is one value given to a check: it is valid, or its error
// contains cause.
type checkCase[T any] struct {
	name  string
	value T
	valid bool
	cause string
}

func runCheck[T any](t *testing.T, check func(T) error, testCases []checkCase[T]) {
	t.Helper()
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := check(tc.value)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.cause)
			requireUnnamed(t, err)
		})
	}
}

func TestCheckBrokers(t *testing.T) {
	t.Parallel()

	runCheck(t, kafka.CheckBrokers, []checkCase[[]string]{
		{name: "one broker", value: []string{"localhost:9092"}, valid: true},
		{name: "several brokers", value: []string{"kafka-0:9092", "kafka-1:9092", "10.0.0.3:9093"}, valid: true},
		{name: "an IPv6 host", value: []string{"[::1]:9092"}, valid: true},
		{name: "port 1", value: []string{"localhost:1"}, valid: true},
		{name: "the highest port", value: []string{"localhost:65535"}, valid: true},
		{name: "nil", value: nil, cause: "must list at least one"},
		{name: "an empty list", value: []string{}, cause: "must list at least one"},
		{name: "the empty string", value: []string{""}, cause: `"" is not host:port`},
		{name: "a host with no port", value: []string{"localhost"}, cause: `"localhost" is not host:port`},
		{name: "no host", value: []string{":9092"}, cause: `":9092" has no host`},
		{name: "port 0", value: []string{"localhost:0"}, cause: `"localhost:0" has no port from 1 to 65535`},
		{name: "a port above 65535", value: []string{"localhost:65536"}, cause: "has no port from 1 to 65535"},
		{name: "a negative port", value: []string{"localhost:-1"}, cause: "has no port from 1 to 65535"},
		{name: "a named port", value: []string{"localhost:kafka"}, cause: "has no port from 1 to 65535"},
		{name: "an empty port", value: []string{"localhost:"}, cause: "has no port from 1 to 65535"},
		{name: "a URL", value: []string{"kafka://localhost:9092"}, cause: "is not host:port"},
		{name: "a bad broker among good ones", value: []string{"kafka-0:9092", "kafka-1"}, cause: `"kafka-1" is not host:port`},
	})
}

func TestCheckBrokers_names_every_bad_broker(t *testing.T) {
	t.Parallel()

	err := kafka.CheckBrokers([]string{"kafka-0", "kafka-1:9092", "kafka-2:0"})
	require.ErrorContains(t, err, `"kafka-0" is not host:port`)
	require.ErrorContains(t, err, `"kafka-2:0" has no port from 1 to 65535`)
	require.NotContains(t, err.Error(), "kafka-1")
	requireUnnamed(t, err)
}

func TestCheckGroupID(t *testing.T) {
	t.Parallel()

	runCheck(t, kafka.CheckGroupID, []checkCase[string]{
		{name: "a name", value: "orders", valid: true},
		{name: "one character", value: "a", valid: true},
		{name: "the empty string", value: "", cause: "must not be empty"},
	})
}

func TestCheckTopics(t *testing.T) {
	t.Parallel()

	runCheck(t, kafka.CheckTopics, []checkCase[[]string]{
		{name: "one", value: []string{"orders"}, valid: true},
		{name: "several", value: []string{"orders", "payments", "orders.dlq"}, valid: true},
		{name: "nil", value: nil, cause: "must list at least one"},
		{name: "an empty list", value: []string{}, cause: "must list at least one"},
		{name: "an empty name", value: []string{"orders", ""}, cause: "has an empty name at index 1"},
		{name: "a name twice", value: []string{"orders", "payments", "orders"}, cause: `names "orders" more than once`},
	})
}

func TestCheckTopics_reports_every_problem_once(t *testing.T) {
	t.Parallel()

	err := kafka.CheckTopics([]string{"", "orders", "orders", "orders", "payments", "payments"})
	require.EqualError(t, err, "has an empty name at index 0\n"+
		`names "orders" more than once`+"\n"+
		`names "payments" more than once`)
	requireUnnamed(t, err)
}

func TestCheckSessionTimeout(t *testing.T) {
	t.Parallel()

	runCheck(t, kafka.CheckSessionTimeout, durationCases())
}

func TestCheckRebalanceTimeout(t *testing.T) {
	t.Parallel()

	runCheck(t, kafka.CheckRebalanceTimeout, durationCases())
}

func durationCases() []checkCase[time.Duration] {
	return []checkCase[time.Duration]{
		{name: "one nanosecond", value: time.Nanosecond, valid: true},
		{name: "a minute", value: time.Minute, valid: true},
		{name: "zero", value: 0, cause: "must be positive"},
		{name: "negative", value: -time.Second, cause: "must be positive"},
	}
}

func TestCheckFetchMaxBytes(t *testing.T) {
	t.Parallel()

	runCheck(t, kafka.CheckFetchMaxBytes, []checkCase[int32]{
		{name: "one byte", value: 1, valid: true},
		{name: "1 GiB", value: 1 << 30, valid: true},
		{name: "zero", value: 0, cause: "must be positive"},
		{name: "negative", value: -1, cause: "must be positive"},
	})
}

func TestCheckMaxPollRecords(t *testing.T) {
	t.Parallel()

	runCheck(t, kafka.CheckMaxPollRecords, []checkCase[int]{
		{name: "one record", value: 1, valid: true},
		{name: "ten thousand", value: 10_000, valid: true},
		{name: "zero", value: 0, cause: "must be positive"},
		{name: "negative", value: -1, cause: "must be positive"},
	})
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	require.Equal(t, kafka.Settings{
		Brokers:          nil,
		GroupID:          "",
		Topics:           nil,
		Delivery:         queue.AtLeastOnce,
		TLS:              nil,
		SASL:             nil,
		SessionTimeout:   45 * time.Second,
		RebalanceTimeout: 60 * time.Second,
		FetchMaxBytes:    50 << 20,
		MaxPollRecords:   500,
	}, kafka.DefaultSettings())
}

func TestDefaultSettings_checks(t *testing.T) {
	t.Parallel()

	d := kafka.DefaultSettings()

	testCases := []struct {
		name  string
		err   error
		valid bool
	}{
		{name: "Brokers", err: kafka.CheckBrokers(d.Brokers)},
		{name: "GroupID", err: kafka.CheckGroupID(d.GroupID)},
		{name: "Topics", err: kafka.CheckTopics(d.Topics)},
		{name: "Delivery", err: queue.CheckDelivery(d.Delivery), valid: true},
		{name: "SessionTimeout", err: kafka.CheckSessionTimeout(d.SessionTimeout), valid: true},
		{name: "RebalanceTimeout", err: kafka.CheckRebalanceTimeout(d.RebalanceTimeout), valid: true},
		{name: "FetchMaxBytes", err: kafka.CheckFetchMaxBytes(d.FetchMaxBytes), valid: true},
		{name: "MaxPollRecords", err: kafka.CheckMaxPollRecords(d.MaxPollRecords), valid: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.valid {
				require.NoError(t, tc.err)
				return
			}
			require.Error(t, tc.err, "a required field has no default, so its check fails")
			requireUnnamed(t, tc.err)
		})
	}
}

func TestCheckDeliveryTimeout(t *testing.T) {
	t.Parallel()

	runCheck(t, kafka.CheckDeliveryTimeout, []checkCase[time.Duration]{
		{name: "one nanosecond", value: time.Nanosecond, valid: true},
		{name: "the default", value: 10 * time.Second, valid: true},
		{name: "a minute", value: time.Minute, valid: true},
		{name: "zero", value: 0, cause: "must be positive"},
		{name: "negative", value: -time.Second, cause: "must be positive"},
	})
}

func TestDefaultProducerSettings(t *testing.T) {
	t.Parallel()

	require.Equal(t, kafka.ProducerSettings{
		Brokers:         nil,
		TLS:             nil,
		SASL:            nil,
		DeliveryTimeout: 10 * time.Second,
	}, kafka.DefaultProducerSettings())
}

func TestDefaultProducerSettings_checks(t *testing.T) {
	t.Parallel()

	d := kafka.DefaultProducerSettings()

	t.Run("Brokers", func(t *testing.T) {
		t.Parallel()

		err := kafka.CheckBrokers(d.Brokers)
		require.Error(t, err, "Brokers has no default, so its check fails")
		requireUnnamed(t, err)
	})

	t.Run("DeliveryTimeout", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, kafka.CheckDeliveryTimeout(d.DeliveryTimeout))
	})

	t.Run("with Brokers set, every check passes", func(t *testing.T) {
		t.Parallel()

		ps := kafka.DefaultProducerSettings()
		ps.Brokers = []string{"127.0.0.1:9092"}
		require.NoError(t, kafka.CheckBrokers(ps.Brokers))
		require.NoError(t, kafka.CheckDeliveryTimeout(ps.DeliveryTimeout))
	})
}
