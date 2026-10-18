// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// The tests in this file show the Publisher's circuit breaker, on the
// stand-in of publisher_standin_test.go, or on a humustest Host with
// nothing listening, inside testing/synctest, where OpenFor takes no real
// time.

// The bodies of the Publisher's log records for the breaker's transitions.
const (
	breakerOpen     = "circuit breaker open"
	breakerHalfOpen = "circuit breaker half-open"
	breakerClosed   = "circuit breaker closed"
)

// breakerLogs returns the bodies of the Publisher's breaker records in tel,
// in the order they were emitted.
func breakerLogs(tel *humustest.Telemetry) []string {
	var out []string
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name != serviceScope {
			continue
		}
		switch body := r.Body().AsString(); body {
		case breakerOpen, breakerHalfOpen, breakerClosed:
			out = append(out, body)
		}
	}
	return out
}

// tripOnFirstFailure is a policy that opens on the first failure and closes
// on the first successful probe.
var tripOnFirstFailure = circuit.Policy{
	Trip: func(c circuit.Counts) bool { return c.ConsecutiveFailures >= 1 },
	Probe: func(c circuit.Counts) circuit.State {
		if c.Failures > 0 {
			return circuit.Open
		}
		return circuit.Closed
	},
}

// neverTrips is a policy whose Trip always returns false, which turns the
// breaker off.
var neverTrips = circuit.Policy{
	Trip:  func(circuit.Counts) bool { return false },
	Probe: func(circuit.Counts) circuit.State { return circuit.Closed },
}

func TestPublisher_fails_at_once_once_the_breaker_opens(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		host := h.Host()
		var (
			mu    sync.Mutex
			dials int
		)
		dial := host.Dial
		host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			dials++
			mu.Unlock()
			return dial(ctx, network, address)
		}
		tel := humustest.NewTelemetry()
		sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
		require.NoError(t, err)
		settings := publisherSettings()
		// One attempt per Publish, so each Publish is one attempt through
		// the breaker.
		settings.MaxAttempts = 1
		pub, err := rabbitmq.NewPublisher(&humus.Setup{Host: host, Telemetry: sdk}, settings)
		require.NoError(t, err)

		// Nothing listens at rabbit:5672: each attempt fails to dial.
		for i := range 5 {
			err := pub.Publish(t.Context(), accepted)
			require.ErrorIs(t, err, syscall.ECONNREFUSED, "publish %d", i)
			require.NotErrorIs(t, err, circuit.ErrRejected, "publish %d", i)
		}

		start := time.Now()
		err = pub.Publish(t.Context(), accepted)
		require.Zero(t, time.Since(start), "the rejected Publish returned at once")
		require.ErrorIs(t, err, circuit.ErrRejected)
		rejected, ok := errors.AsType[*circuit.RejectedError](err)
		require.True(t, ok, "the error wraps a *circuit.RejectedError: %v", err)
		require.Equal(t, circuit.Open, rejected.State)
		require.Equal(t, settings.Breaker.OpenFor, rejected.RetryAfter, "the breaker stays open for OpenFor")
		mu.Lock()
		require.Equal(t, 5, dials, "the rejected Publish didn't dial")
		mu.Unlock()

		open := records(tel.Logs(), breakerOpen)
		require.Len(t, open, 1)
		require.Equal(t, log.SeverityWarn, open[0].Severity())
		require.Equal(t, map[string]string{
			"server.address":                "rabbit",
			"server.port":                   "5672",
			"circuit.previous_state":        "closed",
			"circuit.successes":             "0",
			"circuit.failures":              "5",
			"circuit.consecutive_successes": "0",
			"circuit.consecutive_failures":  "5",
			"circuit.timed_out":             "0",
		}, attrs(open[0]))
		require.Equal(t, []string{breakerOpen}, breakerLogs(tel))
	})
}

func TestPublisher_an_attempt_the_breaker_rejected_is_not_retried(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.MaxAttempts = 3
		settings.BreakerPolicy = tripOnFirstFailure
		pub, s := newPublisher(t, settings)
		s.autoNack = true

		// The first attempt is nacked, which opens the breaker, and the
		// retry that follows is rejected, and ends the Publish.
		err := pub.Publish(t.Context(), accepted)
		require.ErrorIs(t, err, circuit.ErrRejected)
		require.Len(t, s.published, 1, "only the first attempt was published")
		<-s.published
		requireNoMorePublishes(t, s)
	})
}

func TestPublisher_an_answer_from_the_broker_does_not_open_the_breaker(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisher(t)
		for i := range 10 {
			done := publishAsync(t.Context(), tp.pub, accepted)
			(<-tp.s.published).unroutable()
			require.ErrorIs(t, <-done, rabbitmq.ErrUnroutable, "publish %d", i)
		}

		done := publishAsync(t.Context(), tp.pub, accepted)
		(<-tp.s.published).ack()
		require.NoError(t, <-done, "the breaker is still closed")
		require.Empty(t, breakerLogs(tp.tel))
	})
}

func TestPublisher_the_breaker_closes_again(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.MaxAttempts = 1
		tp := newTracedPublisherOf(t, settings)

		// The broker nacks five publishes in a row, which opens the breaker.
		tp.s.autoNack = true
		for i := range 5 {
			require.ErrorContains(t, tp.pub.Publish(t.Context(), accepted), "the broker nacked the message", "publish %d", i)
		}
		require.ErrorIs(t, tp.pub.Publish(t.Context(), accepted), circuit.ErrRejected)

		// The broker recovers.
		tp.s.autoNack = false
		tp.s.autoAck = true
		time.Sleep(settings.Breaker.OpenFor)
		for i := range 3 {
			require.NoError(t, tp.pub.Publish(t.Context(), accepted), "probe %d", i)
		}
		require.NoError(t, tp.pub.Publish(t.Context(), accepted), "the breaker is closed")
		require.Equal(t, []string{breakerOpen, breakerHalfOpen, breakerClosed}, breakerLogs(tp.tel))

		halfOpen := records(tp.tel.Logs(), breakerHalfOpen)
		require.Len(t, halfOpen, 1)
		require.Equal(t, log.SeverityInfo, halfOpen[0].Severity())
		require.Equal(t, "open", attrs(halfOpen[0])["circuit.previous_state"])

		closed := records(tp.tel.Logs(), breakerClosed)
		require.Len(t, closed, 1)
		require.Equal(t, log.SeverityInfo, closed[0].Severity())
		require.Equal(t, map[string]string{
			"server.address":                "rabbit",
			"server.port":                   "5672",
			"circuit.previous_state":        "half-open",
			"circuit.successes":             "3",
			"circuit.failures":              "0",
			"circuit.consecutive_successes": "3",
			"circuit.consecutive_failures":  "0",
			"circuit.timed_out":             "0",
		}, attrs(closed[0]))
	})
}

func TestPublisher_a_caller_giving_up_is_not_counted_by_the_breaker(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisher(t)
		for i := range 6 {
			ctx, cancel := context.WithCancel(t.Context())
			done := publishAsync(ctx, tp.pub, accepted)
			<-tp.s.published
			requirePending(t, done)
			cancel()
			require.ErrorIs(t, <-done, context.Canceled, "publish %d", i)
		}

		done := publishAsync(t.Context(), tp.pub, accepted)
		(<-tp.s.published).ack()
		require.NoError(t, <-done, "the breaker is still closed")
		require.Empty(t, breakerLogs(tp.tel))
	})
}

func TestPublisher_a_rejected_Publish_fails_its_span_and_is_not_measured(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.MaxAttempts = 1
		settings.BreakerPolicy = tripOnFirstFailure
		tp := newTracedPublisherOf(t, settings)
		tp.s.autoNack = true

		require.ErrorContains(t, tp.pub.Publish(t.Context(), accepted), "the broker nacked the message")
		rejected := tp.pub.Publish(t.Context(), accepted)
		require.ErrorIs(t, rejected, circuit.ErrRejected)

		spans := spansNamed(tp.tel, "send orders")
		require.Len(t, spans, 2)
		var rejections int
		for _, span := range spans {
			require.Equal(t, codes.Error, span.Status().Code)
			require.Len(t, span.Events(), 1)
			require.Equal(t, "exception", span.Events()[0].Name)
			if spanAttrs(span)["error.type"].AsString() == "*circuit.RejectedError" {
				rejections++
				require.Equal(t, rejected.Error(), span.Status().Description)
				require.Zero(t, span.EndTime().Sub(span.StartTime()), "a rejected Publish's span ends at once")
			}
		}
		require.Equal(t, 1, rejections)

		sent, ok := metricNamed(t, tp.tel, "messaging.client.sent.messages").Data.(metricdata.Sum[int64])
		require.True(t, ok)
		require.Len(t, sent.DataPoints, 1)
		require.Equal(t, int64(1), sent.DataPoints[0].Value, "only the nacked publish was sent")
		require.Equal(t, sentWith("nacked"), setAttrs(sent.DataPoints[0].Attributes))

		durations, ok := metricNamed(t, tp.tel, "messaging.client.operation.duration").Data.(metricdata.Histogram[float64])
		require.True(t, ok)
		require.Len(t, durations.DataPoints, 1)
		require.Equal(t, uint64(1), durations.DataPoints[0].Count)
	})
}

func TestPublisher_a_policy_that_never_trips_turns_the_breaker_off(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.MaxAttempts = 1
		settings.BreakerPolicy = neverTrips
		tp := newTracedPublisherOf(t, settings)
		tp.s.autoNack = true

		for i := range 30 {
			err := tp.pub.Publish(t.Context(), accepted)
			require.ErrorContains(t, err, "the broker nacked the message", "publish %d", i)
			require.NotErrorIs(t, err, circuit.ErrRejected, "publish %d", i)
		}
		require.Len(t, tp.s.published, 30)
		require.Empty(t, breakerLogs(tp.tel))
	})
}
