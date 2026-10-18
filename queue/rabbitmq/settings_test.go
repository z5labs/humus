// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/rand"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// fieldNames matches words a check's error must not contain, in any case:
// a check reports what is wrong, and whoever calls it names the field or
// the variable.
var fieldNames = regexp.MustCompile(`(?i)\b(?:settings|connection|addr|address|vhost|virtual ?host|tls|sasl|` +
	`authentication|heartbeat|connect ?timeout|queue|delivery|prefetch|concurrency|channels|timeout|` +
	`(?:max ?)?attempts|backoff ?(?:base|max)?|(?:retry ?)?budget|max ?tokens|token ?ratio|` +
	`breaker|window|probes|open ?for|probe ?timeout|policy|trip)\b`)

// requireUnnamed fails the test if err names a field of Settings,
// PublisherSettings or Connection.
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

func TestCheckAddr(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckAddr, []checkCase[string]{
		{name: "a host name", value: "localhost:5672", valid: true},
		{name: "a TLS port", value: "rabbitmq.example.com:5671", valid: true},
		{name: "an IPv4 host", value: "10.0.0.3:5672", valid: true},
		{name: "an IPv6 host", value: "[::1]:5672", valid: true},
		{name: "port 1", value: "localhost:1", valid: true},
		{name: "the highest port", value: "localhost:65535", valid: true},
		{name: "the empty string", value: "", cause: "is not host:port"},
		{name: "a host with no port", value: "localhost", cause: "is not host:port"},
		{name: "no host", value: ":5672", cause: "has no host"},
		{name: "port 0", value: "localhost:0", cause: "has no port from 1 to 65535"},
		{name: "a port above 65535", value: "localhost:65536", cause: "has no port from 1 to 65535"},
		{name: "a negative port", value: "localhost:-1", cause: "has no port from 1 to 65535"},
		{name: "a named port", value: "localhost:amqp", cause: "has no port from 1 to 65535"},
		{name: "an empty port", value: "localhost:", cause: "has no port from 1 to 65535"},
		{name: "a URI", value: "amqp://localhost:5672", cause: "is a URI, not host:port"},
		{name: "a TLS URI", value: "amqps://localhost:5671/", cause: "is a URI, not host:port"},
	})
}

// TestCheckAddr_never_quotes_the_value checks that a login in a wrong
// address doesn't reach the error, and so the logs.
func TestCheckAddr_never_quotes_the_value(t *testing.T) {
	t.Parallel()

	password := rand.Text()
	testCases := []struct {
		name  string
		value string
	}{
		{name: "a URI with a login", value: "amqp://orders:" + password + "@localhost:5672/"},
		{name: "a TLS URI with a login", value: "amqps://orders:" + password + "@localhost:5671/orders"},
		{name: "a login with no scheme", value: "orders:" + password + "@localhost:5672"},
		{name: "a login with no port", value: "orders@" + password},
		{name: "a login with port 0", value: password + ":0"},
		{name: "a login with no host", value: ":" + password},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := rabbitmq.CheckAddr(tc.value)
			require.Error(t, err)
			require.NotContains(t, err.Error(), password)
			requireUnnamed(t, err)
		})
	}
}

func TestCheckVhost(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckVhost, []checkCase[string]{
		{name: "the default", value: "/", valid: true},
		{name: "a name", value: "orders", valid: true},
		{name: "a name with a slash", value: "/orders", valid: true},
		{name: "the empty string", value: "", cause: "must not be empty"},
	})
}

func TestCheckSASL(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckSASL, []checkCase[amqp.Authentication]{
		{name: "PlainAuth", value: &amqp.PlainAuth{Username: "orders", Password: rand.Text()}, valid: true},
		{name: "AMQPlainAuth", value: &amqp.AMQPlainAuth{Username: "orders", Password: rand.Text()}, valid: true},
		{name: "ExternalAuth", value: &amqp.ExternalAuth{}, valid: true},
		{name: "a mechanism of the end user's", value: tokenAuth{}, valid: true},
		{name: "nil", value: nil, cause: "must be set"},
		{name: "a nil PlainAuth", value: (*amqp.PlainAuth)(nil), cause: "must be set"},
		{name: "a nil ExternalAuth", value: (*amqp.ExternalAuth)(nil), cause: "must be set"},
	})
}

// tokenAuth is an Authentication that is not amqp091's, and not a pointer.
type tokenAuth struct{}

func (tokenAuth) Mechanism() string { return "TOKEN" }
func (tokenAuth) Response() string  { return "token" }

func TestCheckHeartbeat(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckHeartbeat, []checkCase[time.Duration]{
		{name: "one second", value: time.Second, valid: true},
		{name: "the default", value: 10 * time.Second, valid: true},
		{name: "a minute", value: time.Minute, valid: true},
		{name: "the longest", value: 65535 * time.Second, valid: true},
		{name: "zero", value: 0, cause: "must be from 1s to 65535s"},
		{name: "negative", value: -time.Second, cause: "must be from 1s to 65535s"},
		{name: "above the longest", value: 65536 * time.Second, cause: "must be from 1s to 65535s"},
		{name: "a fraction of a second", value: 1500 * time.Millisecond, cause: "must be a whole number of seconds"},
		{name: "below a second", value: 500 * time.Millisecond, cause: "must be a whole number of seconds"},
		{name: "one nanosecond", value: time.Nanosecond, cause: "must be a whole number of seconds"},
	})
}

func TestCheckConnectTimeout(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckConnectTimeout, []checkCase[time.Duration]{
		{name: "one nanosecond", value: time.Nanosecond, valid: true},
		{name: "the default", value: 30 * time.Second, valid: true},
		{name: "a minute", value: time.Minute, valid: true},
		{name: "zero", value: 0, cause: "must be positive"},
		{name: "negative", value: -time.Second, cause: "must be positive"},
	})
}

func TestCheckQueue(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckQueue, []checkCase[string]{
		{name: "a name", value: "orders", valid: true},
		{name: "one byte", value: "a", valid: true},
		{name: "255 bytes", value: strings.Repeat("a", 255), valid: true},
		{name: "255 bytes of multi-byte runes", value: strings.Repeat("é", 127) + "a", valid: true},
		{name: "the empty string", value: "", cause: "must not be empty"},
		{name: "256 bytes", value: strings.Repeat("a", 256), cause: "must be at most 255 bytes"},
		{name: "128 runes of 2 bytes", value: strings.Repeat("é", 128), cause: "must be at most 255 bytes"},
	})
}

func TestCheckPrefetch(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckPrefetch, []checkCase[int]{
		{name: "one", value: 1, valid: true},
		{name: "the default", value: 10, valid: true},
		{name: "the highest", value: 65535, valid: true},
		{name: "zero", value: 0, cause: "must be from 1 to 65535"},
		{name: "negative", value: -1, cause: "must be from 1 to 65535"},
		{name: "above the highest", value: 65536, cause: "must be from 1 to 65535"},
	})
}

func TestCheckConcurrency(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckConcurrency, []checkCase[int]{
		{name: "one", value: 1, valid: true},
		{name: "four", value: 4, valid: true},
		{name: "above the highest prefetch", value: 100_000, valid: true},
		{name: "zero", value: 0, cause: "must be at least 1"},
		{name: "negative", value: -1, cause: "must be at least 1"},
	})
}

func TestCheckChannels(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckChannels, []checkCase[int]{
		{name: "one", value: 1, valid: true},
		{name: "the default", value: 8, valid: true},
		{name: "the highest", value: 65535, valid: true},
		{name: "zero", value: 0, cause: "must be from 1 to 65535"},
		{name: "negative", value: -1, cause: "must be from 1 to 65535"},
		{name: "above the highest", value: 65536, cause: "must be from 1 to 65535"},
	})
}

func TestCheckTimeout(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckTimeout, []checkCase[time.Duration]{
		{name: "a nanosecond", value: time.Nanosecond, valid: true},
		{name: "the default", value: 10 * time.Second, valid: true},
		{name: "zero", value: 0, cause: "must be positive"},
		{name: "negative", value: -time.Second, cause: "must be positive"},
	})
}

func TestCheckMaxAttempts(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckMaxAttempts, []checkCase[int]{
		{name: "one, which turns retries off", value: 1, valid: true},
		{name: "the default", value: 3, valid: true},
		{name: "many", value: 1000, valid: true},
		{name: "zero", value: 0, cause: "must be at least 1"},
		{name: "negative", value: -1, cause: "must be at least 1"},
	})
}

func TestCheckBackoffBase(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckBackoffBase, []checkCase[time.Duration]{
		{name: "a nanosecond", value: time.Nanosecond, valid: true},
		{name: "the default", value: 100 * time.Millisecond, valid: true},
		{name: "zero", value: 0, cause: "must be positive"},
		{name: "negative", value: -time.Second, cause: "must be positive"},
	})
}

func TestCheckBackoffMax(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckBackoffMax, []checkCase[time.Duration]{
		{name: "a nanosecond", value: time.Nanosecond, valid: true},
		{name: "the default", value: 5 * time.Second, valid: true},
		{name: "zero", value: 0, cause: "must be positive"},
		{name: "negative", value: -time.Second, cause: "must be positive"},
	})
}

func TestCheckRetryBudgetMaxTokens(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckRetryBudgetMaxTokens, []checkCase[int]{
		{name: "the fewest", value: 3, valid: true},
		{name: "the default", value: 10, valid: true},
		{name: "the most", value: retry.TokenLimit, valid: true},
		{name: "two", value: 2, cause: "must be from 3 to 65536"},
		{name: "zero", value: 0, cause: "must be from 3 to 65536"},
		{name: "above the most", value: retry.TokenLimit + 1, cause: "must be from 3 to 65536"},
	})
}

func TestCheckRetryBudgetTokenRatio(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckRetryBudgetTokenRatio, []checkCase[float64]{
		{name: "the least", value: 0.001, valid: true},
		{name: "the default", value: 0.1, valid: true},
		{name: "the most", value: retry.TokenLimit, valid: true},
		{name: "below the least", value: 0.0009, cause: "must be from 0.001 to 65536"},
		{name: "zero", value: 0, cause: "must be from 0.001 to 65536"},
		{name: "negative", value: -1, cause: "must be from 0.001 to 65536"},
		{name: "above the most", value: retry.TokenLimit + 1, cause: "must be from 0.001 to 65536"},
		{name: "NaN", value: math.NaN(), cause: "must be from 0.001 to 65536"},
		{name: "infinity", value: math.Inf(1), cause: "must be from 0.001 to 65536"},
	})
}

// TestCheckRetryBudget_accepts_exactly_what_retry_NewBudget_accepts runs
// both checks and retry.NewBudget over the edges of each field's range.
func TestCheckRetryBudget_accepts_exactly_what_retry_NewBudget_accepts(t *testing.T) {
	t.Parallel()

	for _, tokens := range []int{math.MinInt, -1, 0, 1, 2, 3, 4, 10, retry.TokenLimit - 1, retry.TokenLimit, retry.TokenLimit + 1, math.MaxInt} {
		for _, ratio := range []float64{math.Inf(-1), -1, 0, 0.0009, 0.001, 0.0011, 0.1, 1, retry.TokenLimit, retry.TokenLimit + 0.5, math.Inf(1), math.NaN()} {
			cfg := retry.BudgetConfig{MaxTokens: tokens, TokenRatio: ratio}
			_, err := retry.NewBudget(cfg)
			checked := rabbitmq.CheckRetryBudgetMaxTokens(tokens) == nil && rabbitmq.CheckRetryBudgetTokenRatio(ratio) == nil
			require.Equal(t, err == nil, checked, "%+v: retry.NewBudget says %v", cfg, err)
		}
	}
}

func TestDefaultPublisherSettings(t *testing.T) {
	t.Parallel()

	d := rabbitmq.DefaultPublisherSettings()
	require.NotNil(t, d.BreakerPolicy.Trip)
	require.NotNil(t, d.BreakerPolicy.Probe)
	// A Policy is two functions, which require.Equal can't compare, so it
	// is checked by what it decides, in
	// TestDefaultPublisherSettings_BreakerPolicy.
	d.BreakerPolicy = circuit.Policy{}
	require.Equal(t, rabbitmq.PublisherSettings{
		Connection:  rabbitmq.DefaultSettings().Connection,
		Channels:    8,
		Timeout:     10 * time.Second,
		MaxAttempts: 3,
		BackoffBase: 100 * time.Millisecond,
		BackoffMax:  5 * time.Second,
		RetryBudget: retry.BudgetConfig{MaxTokens: 10, TokenRatio: 0.1},
		Breaker: circuit.Config{
			Window:       20,
			Probes:       1,
			OpenFor:      10 * time.Second,
			ProbeTimeout: 10 * time.Second,
		},
		ForwardScenario: false,
	}, d)
}

func TestDefaultPublisherSettings_BreakerPolicy(t *testing.T) {
	t.Parallel()

	p := rabbitmq.DefaultPublisherSettings().BreakerPolicy

	t.Run("Trip", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name   string
			counts circuit.Counts
			trips  bool
		}{
			{name: "no results", counts: circuit.Counts{}, trips: false},
			{name: "4 failures in a row", counts: circuit.Counts{Failures: 4, ConsecutiveFailures: 4}, trips: false},
			{name: "5 failures in a row", counts: circuit.Counts{Failures: 5, ConsecutiveFailures: 5}, trips: true},
			{name: "half of 20 failed", counts: circuit.Counts{Successes: 10, Failures: 10, ConsecutiveFailures: 1}, trips: true},
			{name: "9 of 20 failed", counts: circuit.Counts{Successes: 11, Failures: 9, ConsecutiveFailures: 1}, trips: false},
			{name: "half of fewer than 20 failed", counts: circuit.Counts{Successes: 9, Failures: 9, ConsecutiveFailures: 1}, trips: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				require.Equal(t, tc.trips, p.Trip(tc.counts))
			})
		}
	})

	t.Run("Probe", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name   string
			counts circuit.Counts
			state  circuit.State
		}{
			{name: "a failed probe", counts: circuit.Counts{Successes: 2, Failures: 1, ConsecutiveFailures: 1}, state: circuit.Open},
			{name: "2 successful probes", counts: circuit.Counts{Successes: 2, ConsecutiveSuccesses: 2}, state: circuit.HalfOpen},
			{name: "3 successful probes", counts: circuit.Counts{Successes: 3, ConsecutiveSuccesses: 3}, state: circuit.Closed},
			{name: "a probe timed out", counts: circuit.Counts{TimedOut: 1}, state: circuit.HalfOpen},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				require.Equal(t, tc.state, p.Probe(tc.counts))
			})
		}
	})
}

func breakerCountCases() []checkCase[int] {
	outOfRange := "must be from 1 to " + strconv.Itoa(circuit.MaxCount)
	return []checkCase[int]{
		{name: "one", value: 1, valid: true},
		{name: "twenty", value: 20, valid: true},
		{name: "circuit.MaxCount", value: circuit.MaxCount, valid: true},
		{name: "zero", value: 0, cause: outOfRange},
		{name: "negative", value: -1, cause: outOfRange},
		{name: "above circuit.MaxCount", value: circuit.MaxCount + 1, cause: outOfRange},
	}
}

func breakerDurationCases() []checkCase[time.Duration] {
	return []checkCase[time.Duration]{
		{name: "a nanosecond", value: time.Nanosecond, valid: true},
		{name: "the default", value: 10 * time.Second, valid: true},
		{name: "zero", value: 0, cause: "must be positive"},
		{name: "negative", value: -time.Second, cause: "must be positive"},
	}
}

func TestCheckBreakerWindow(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckBreakerWindow, breakerCountCases())
}

func TestCheckBreakerProbes(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckBreakerProbes, breakerCountCases())
}

func TestCheckBreakerOpenFor(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckBreakerOpenFor, breakerDurationCases())
}

func TestCheckBreakerProbeTimeout(t *testing.T) {
	t.Parallel()

	runCheck(t, rabbitmq.CheckBreakerProbeTimeout, breakerDurationCases())
}

// TestCheckBreaker_accepts_exactly_what_circuit_New_accepts runs the four
// checks and circuit.New over the edges of each field's range.
func TestCheckBreaker_accepts_exactly_what_circuit_New_accepts(t *testing.T) {
	t.Parallel()

	policy := rabbitmq.DefaultPublisherSettings().BreakerPolicy
	classify := func(context.Context, error) circuit.Outcome { return circuit.Success }
	observe := func(context.Context, circuit.Transition) {}
	counts := []int{math.MinInt, -1, 0, 1, 20, circuit.MaxCount, circuit.MaxCount + 1}
	durations := []time.Duration{math.MinInt64, -1, 0, 1, time.Second}
	for _, n := range counts {
		for _, d := range durations {
			for _, cfg := range []circuit.Config{
				{Window: n, Probes: 1, OpenFor: time.Second, ProbeTimeout: d},
				{Window: 1, Probes: n, OpenFor: d, ProbeTimeout: time.Second},
			} {
				_, err := circuit.New(cfg, policy, classify, observe)
				checked := rabbitmq.CheckBreakerWindow(cfg.Window) == nil &&
					rabbitmq.CheckBreakerProbes(cfg.Probes) == nil &&
					rabbitmq.CheckBreakerOpenFor(cfg.OpenFor) == nil &&
					rabbitmq.CheckBreakerProbeTimeout(cfg.ProbeTimeout) == nil
				require.Equal(t, err == nil, checked, "%+v: circuit.New says %v", cfg, err)
			}
		}
	}
}

func TestDefaultPublisherSettings_with_the_required_fields_set_passes_every_check(t *testing.T) {
	t.Parallel()

	d := rabbitmq.DefaultPublisherSettings()
	require.Error(t, rabbitmq.CheckAddr(d.Connection.Addr), "Connection.Addr has no default")
	require.Error(t, rabbitmq.CheckSASL(d.Connection.SASL), "Connection.SASL has no default")
	d.Connection.Addr = "localhost:5672"
	d.Connection.SASL = &amqp.PlainAuth{Username: "orders", Password: rand.Text()}

	require.NoError(t, rabbitmq.CheckAddr(d.Connection.Addr))
	require.NoError(t, rabbitmq.CheckVhost(d.Connection.Vhost))
	require.NoError(t, rabbitmq.CheckSASL(d.Connection.SASL))
	require.NoError(t, rabbitmq.CheckHeartbeat(d.Connection.Heartbeat))
	require.NoError(t, rabbitmq.CheckConnectTimeout(d.Connection.ConnectTimeout))
	require.NoError(t, rabbitmq.CheckChannels(d.Channels))
	require.NoError(t, rabbitmq.CheckTimeout(d.Timeout))
	require.NoError(t, rabbitmq.CheckMaxAttempts(d.MaxAttempts))
	require.NoError(t, rabbitmq.CheckBackoffBase(d.BackoffBase))
	require.NoError(t, rabbitmq.CheckBackoffMax(d.BackoffMax))
	require.GreaterOrEqual(t, d.BackoffMax, d.BackoffBase)
	require.NoError(t, rabbitmq.CheckRetryBudgetMaxTokens(d.RetryBudget.MaxTokens))
	require.NoError(t, rabbitmq.CheckRetryBudgetTokenRatio(d.RetryBudget.TokenRatio))
	require.NoError(t, rabbitmq.CheckBreakerWindow(d.Breaker.Window))
	require.NoError(t, rabbitmq.CheckBreakerProbes(d.Breaker.Probes))
	require.NoError(t, rabbitmq.CheckBreakerOpenFor(d.Breaker.OpenFor))
	require.NoError(t, rabbitmq.CheckBreakerProbeTimeout(d.Breaker.ProbeTimeout))
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()

	require.Equal(t, rabbitmq.Settings{
		Connection: rabbitmq.Connection{
			Addr:           "",
			Vhost:          "/",
			TLS:            nil,
			SASL:           nil,
			Heartbeat:      10 * time.Second,
			ConnectTimeout: 30 * time.Second,
		},
		Queue:         "",
		Delivery:      queue.AtLeastOnce,
		Prefetch:      10,
		Concurrency:   1,
		TrustScenario: nil,
	}, rabbitmq.DefaultSettings())
}

func TestDefaultSettings_checks(t *testing.T) {
	t.Parallel()

	d := rabbitmq.DefaultSettings()

	testCases := []struct {
		name  string
		err   error
		valid bool
	}{
		{name: "Connection.Addr", err: rabbitmq.CheckAddr(d.Connection.Addr)},
		{name: "Connection.Vhost", err: rabbitmq.CheckVhost(d.Connection.Vhost), valid: true},
		{name: "Connection.SASL", err: rabbitmq.CheckSASL(d.Connection.SASL)},
		{name: "Connection.Heartbeat", err: rabbitmq.CheckHeartbeat(d.Connection.Heartbeat), valid: true},
		{name: "Connection.ConnectTimeout", err: rabbitmq.CheckConnectTimeout(d.Connection.ConnectTimeout), valid: true},
		{name: "Queue", err: rabbitmq.CheckQueue(d.Queue)},
		{name: "Delivery", err: queue.CheckDelivery(d.Delivery), valid: true},
		{name: "Prefetch", err: rabbitmq.CheckPrefetch(d.Prefetch), valid: true},
		{name: "Concurrency", err: rabbitmq.CheckConcurrency(d.Concurrency), valid: true},
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

func TestDefaultSettings_with_the_required_fields_set_passes_every_check(t *testing.T) {
	t.Parallel()

	d := rabbitmq.DefaultSettings()
	d.Connection.Addr = "localhost:5672"
	d.Connection.SASL = &amqp.PlainAuth{Username: "orders", Password: rand.Text()}
	d.Queue = "orders"

	require.NoError(t, rabbitmq.CheckAddr(d.Connection.Addr))
	require.NoError(t, rabbitmq.CheckVhost(d.Connection.Vhost))
	require.NoError(t, rabbitmq.CheckSASL(d.Connection.SASL))
	require.NoError(t, rabbitmq.CheckHeartbeat(d.Connection.Heartbeat))
	require.NoError(t, rabbitmq.CheckConnectTimeout(d.Connection.ConnectTimeout))
	require.NoError(t, rabbitmq.CheckQueue(d.Queue))
	require.NoError(t, queue.CheckDelivery(d.Delivery))
	require.NoError(t, rabbitmq.CheckPrefetch(d.Prefetch))
	require.NoError(t, rabbitmq.CheckConcurrency(d.Concurrency))
	require.LessOrEqual(t, d.Concurrency, d.Prefetch)
}
