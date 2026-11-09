// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/kafka"
)

// The bodies of the Producer's log records for the breaker's transitions.
const (
	breakerOpen     = "circuit breaker open"
	breakerHalfOpen = "circuit breaker half-open"
	breakerClosed   = "circuit breaker closed"
)

// answerProduce makes the cluster answer every Produce with code on every
// partition while failing reports true, and handle it as it would have
// otherwise.
func answerProduce(c *kfake.Cluster, code int16, failing func() bool) {
	c.ControlKey(kmsg.Produce.Int16(), func(req kmsg.Request) (kmsg.Response, error, bool) {
		c.KeepControl()
		if !failing() {
			return nil, nil, false
		}
		r := req.(*kmsg.ProduceRequest)
		resp := r.ResponseKind().(*kmsg.ProduceResponse)
		for _, rt := range r.Topics {
			st := kmsg.NewProduceResponseTopic()
			st.Topic = rt.Topic
			st.TopicID = rt.TopicID
			for _, rp := range rt.Partitions {
				sp := kmsg.NewProduceResponseTopicPartition()
				sp.Partition = rp.Partition
				sp.ErrorCode = code
				st.Partitions = append(st.Partitions, sp)
			}
			resp.Topics = append(resp.Topics, st)
		}
		return resp, nil, true
	})
}

// always is a failing that always reports true.
func always() bool { return true }

// breakerLogs returns the bodies of the Producer's breaker records in tel,
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

func TestProduce_fails_at_once_once_the_breaker_opens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		answerProduce(c, kerr.NotEnoughReplicas.Code, always) // every Produce, a retriable error

		tel := humustest.NewTelemetry()
		var errs []error
		var took []time.Duration
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			for range 6 {
				start := time.Now()
				errs = append(errs, p.Produce(ctx, value(0)))
				took = append(took, time.Since(start))
			}
			return nil
		})

		for i := range 5 {
			require.ErrorIs(t, errs[i], context.DeadlineExceeded, "call %d", i)
			// DeliveryTimeout, in fake time. A call may return a few
			// milliseconds after its deadline: then the Producer has
			// franz-go refresh its metadata at once, and franz-go fails
			// the record at the try that follows, whether the deadline
			// fell on a try or while franz-go waited to try again.
			require.InDelta(t, 10*time.Second, took[i], float64(100*time.Millisecond), "call %d took %v", i, took[i])
		}
		require.ErrorIs(t, errs[5], circuit.ErrRejected)
		rejected, ok := errors.AsType[*circuit.RejectedError](errs[5])
		require.True(t, ok, "the error wraps a *circuit.RejectedError: %v", errs[5])
		require.Equal(t, circuit.Open, rejected.State)
		require.Equal(t, 10*time.Second, rejected.RetryAfter, "the breaker stays open for OpenFor")
		require.Zero(t, took[5], "the rejected call returned at once")

		open := records(tel.Logs(), breakerOpen)
		require.Len(t, open, 1)
		require.Equal(t, log.SeverityWarn, open[0].Severity())
		host, port, err := net.SplitHostPort(c.ListenAddrs()[0])
		require.NoError(t, err)
		require.Equal(t, map[string]string{
			"messaging.system":                    "kafka",
			"server.address":                      host,
			"server.port":                         port,
			"humus.circuit.previous_state":        "closed",
			"humus.circuit.successes":             "0",
			"humus.circuit.failures":              "5",
			"humus.circuit.consecutive_successes": "0",
			"humus.circuit.consecutive_failures":  "5",
			"humus.circuit.timed_out":             "0",
		}, attrs(open[0]))
		require.Equal(t, []string{breakerOpen}, breakerLogs(tel))
	})
}

func TestProduce_a_records_own_failure_does_not_open_the_breaker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		tooLarge := kafka.Record{Topic: topic, Value: make([]byte, 1_000_013)} // larger than ProducerBatchMaxBytes
		var errs []error
		var last error
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			for range 10 {
				errs = append(errs, p.Produce(ctx, tooLarge))
			}
			last = p.Produce(ctx, value(0))
			return nil
		})

		require.Len(t, errs, 10)
		for i, err := range errs {
			require.ErrorIs(t, err, kerr.MessageTooLarge, "call %d", i)
		}
		require.NoError(t, last, "the breaker is still closed")
		require.Empty(t, breakerLogs(tel))
		require.Len(t, readAll(t, h, c, topic), 1)
	})
}

// heldRequest says when the cluster in closesAgain answers the first Produce
// request of the last of the calls that fail.
type heldRequest int

const (
	// notHeld has the cluster answer it when it comes, like every other.
	notHeld heldRequest = iota
	// answeredBeforeRecovery has the cluster hold it past the call's
	// DeliveryTimeout, then answer it while it still fails.
	answeredBeforeRecovery
	// answeredAfterRecovery has the cluster hold it past the call's
	// DeliveryTimeout, then handle it once it has recovered, so its record
	// is stored although the call failed.
	answeredAfterRecovery
)

// holdOnce returns a failing for answerProduce that reports failing, and
// that, the first time it is asked while hold is set, waits for released
// first. The cluster handles nothing else while it waits. It closes handled
// once it has read failing for the held request.
func holdOnce(failing, hold *atomic.Bool, released <-chan struct{}, handled chan<- struct{}) func() bool {
	return func() bool {
		if !hold.CompareAndSwap(true, false) {
			return failing.Load()
		}
		<-released
		defer close(handled)
		return failing.Load()
	}
}

// closesAgain has five calls fail while the cluster answers every Produce
// with a retriable error, so that the breaker opens, and has the cluster
// answer the first Produce request of the last of them as held says. It then
// checks that the next call is rejected, and that once the cluster recovers
// and OpenFor has passed, three probes and the call after them succeed, the
// breaker closes again, and the topic holds the records of those four calls,
// in order. It returns the values the topic holds.
//
// A call that fails at its DeliveryTimeout may still have its record stored,
// as doc.go says: with franz-go retrying every 5s, the last try of a call
// goes out as its 10s deadline passes, and the cluster can handle that
// request after it has recovered. So the topic may hold the record of a
// failed call before the four.
func closesAgain(t *testing.T, held heldRequest) []string {
	t.Helper()
	h := humustest.NewHost(t)
	c := cluster(t, h, 1)
	var failing, hold atomic.Bool
	failing.Store(true)
	released := make(chan struct{})
	handled := make(chan struct{})
	answerProduce(c, kerr.NotEnoughReplicas.Code, holdOnce(&failing, &hold, released, handled))

	var failed []error
	var rejected error
	var probes []error
	var after error
	tel := humustest.NewTelemetry()
	ps := producerSettings(c.ListenAddrs()...)
	producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
		for i := range 5 {
			hold.Store(held != notHeld && i == 4)
			failed = append(failed, p.Produce(ctx, value(100+i)))
		}
		rejected = p.Produce(ctx, value(105))
		if held == answeredBeforeRecovery {
			close(released)
			<-handled
		}
		failing.Store(false) // the cluster recovers
		if held == answeredAfterRecovery {
			close(released)
			<-handled
		}
		time.Sleep(ps.Breaker.OpenFor)
		for i := range 3 {
			probes = append(probes, p.Produce(ctx, value(i)))
		}
		after = p.Produce(ctx, value(3))
		return nil
	})

	require.Len(t, failed, 5)
	for i, err := range failed {
		require.ErrorIs(t, err, context.DeadlineExceeded, "call %d", i)
	}
	require.ErrorIs(t, rejected, circuit.ErrRejected)
	require.Len(t, probes, 3)
	for i, err := range probes {
		require.NoError(t, err, "probe %d", i)
	}
	require.NoError(t, after)
	require.Equal(t, []string{breakerOpen, breakerHalfOpen, breakerClosed}, breakerLogs(tel))

	halfOpen := records(tel.Logs(), breakerHalfOpen)
	require.Len(t, halfOpen, 1)
	require.Equal(t, log.SeverityInfo, halfOpen[0].Severity())
	require.Equal(t, "open", attrs(halfOpen[0])["humus.circuit.previous_state"])

	closed := records(tel.Logs(), breakerClosed)
	require.Len(t, closed, 1)
	require.Equal(t, log.SeverityInfo, closed[0].Severity())
	a := attrs(closed[0])
	require.Equal(t, "half-open", a["humus.circuit.previous_state"])
	require.Equal(t, "3", a["humus.circuit.successes"])
	require.Equal(t, "3", a["humus.circuit.consecutive_successes"])
	require.Equal(t, "kafka", a["messaging.system"])

	var stored []string
	for _, r := range readAll(t, h, c, topic) {
		stored = append(stored, string(r.Value))
	}
	succeeded := []string{"0", "1", "2", "3"}
	require.GreaterOrEqual(t, len(stored), len(succeeded), "stored: %v", stored)
	before := stored[:len(stored)-len(succeeded)]
	require.Equal(t, succeeded, stored[len(before):], "the calls that succeeded, in order, last: %v", stored)
	seen := map[string]bool{}
	for _, v := range before {
		require.Contains(t, []string{"100", "101", "102", "103", "104"}, v, "only a failed call's record comes before them: %v", stored)
		require.False(t, seen[v], "a failed call's record is stored once: %v", stored)
		seen[v] = true
	}
	return stored
}

func TestProduce_the_breaker_closes_again(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closesAgain(t, notHeld)
	})
}

func TestProduce_the_breaker_closes_again_when_no_failed_calls_record_is_stored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stored := closesAgain(t, answeredBeforeRecovery)
		require.Equal(t, []string{"0", "1", "2", "3"}, stored)
	})
}

func TestProduce_the_breaker_closes_again_when_a_failed_calls_request_is_stored_after_recovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stored := closesAgain(t, answeredAfterRecovery)
		require.Equal(t, []string{"104", "0", "1", "2", "3"}, stored,
			"the last failed call's record, stored once the cluster recovered, then the 4 calls that succeeded")
	})
}

func TestProduce_a_done_ctx_is_not_counted_by_the_breaker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)

		var errs []error
		var last error
		tel := humustest.NewTelemetry()
		producesWith(t, h, tel, producerSettings(c.ListenAddrs()...), func(ctx context.Context, p *kafka.Producer) error {
			done, cancel := context.WithCancel(ctx)
			cancel()
			for range 6 {
				errs = append(errs, p.Produce(done, value(0)))
			}
			last = p.Produce(ctx, value(1))
			return nil
		})

		require.Len(t, errs, 6)
		for i, err := range errs {
			require.ErrorIs(t, err, context.Canceled, "call %d", i)
		}
		require.NoError(t, last, "the breaker is still closed")
		require.Empty(t, breakerLogs(tel))
	})
}

func TestProduce_a_call_after_a_refusal_does_not_reach_the_breaker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		user, password := rand.Text(), rand.Text()
		h := humustest.NewHost(t)
		c := cluster(t, h, 1, kfake.EnableSASL(), kfake.Superuser("PLAIN", user, password))
		refuseWrongPasswords(c, password)

		var first error
		var later []error
		tel := humustest.NewTelemetry()
		ps := producerSettings(c.ListenAddrs()...)
		ps.SASL = plain.Auth{User: user, Pass: rand.Text()}.AsMechanism()
		ps.BreakerPolicy = tripOnFirstFailure
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			first = p.Produce(ctx, value(0))
			// The first call's records fail once DeliveryTimeout passes,
			// which opens the breaker.
			time.Sleep(ps.DeliveryTimeout)
			for range 3 {
				later = append(later, p.Produce(ctx, value(1)))
			}
			return nil
		})

		require.ErrorIs(t, first, kerr.SaslAuthenticationFailed)
		require.Equal(t, []string{breakerOpen}, breakerLogs(tel), "the first call's records counted")
		require.Len(t, later, 3)
		for i, err := range later {
			require.ErrorIs(t, err, kerr.SaslAuthenticationFailed, "call %d", i)
			require.NotErrorIs(t, err, circuit.ErrRejected, "the refusal is returned before the breaker; call %d", i)
		}
	})
}

func TestProduce_a_rejected_call_fails_its_spans_and_is_not_measured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		answerProduce(c, kerr.NotEnoughReplicas.Code, always)

		var failed, rejected error
		tel := humustest.NewTelemetry()
		ps := producerSettings(c.ListenAddrs()...)
		ps.BreakerPolicy = tripOnFirstFailure
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			failed = p.Produce(ctx, value(0))
			rejected = p.Produce(ctx, value(1), value(2))
			return nil
		})
		require.ErrorIs(t, failed, context.DeadlineExceeded)
		require.ErrorIs(t, rejected, circuit.ErrRejected)

		spans := spansNamed(tel, sendSpan)
		require.Len(t, spans, 3, "the failed record's span, and one for each rejected record")
		var rejections int
		for _, span := range spans {
			require.Equal(t, codes.Error, span.Status().Code)
			events := span.Events()
			require.Len(t, events, 1)
			require.Equal(t, "exception", events[0].Name)
			if spanAttrs(span)["error.type"].AsString() == "*circuit.RejectedError" {
				rejections++
				require.Contains(t, span.Status().Description, "circuit: call rejected")
				require.Zero(t, span.EndTime().Sub(span.StartTime()), "a rejected record's span ends at once")
			}
		}
		require.Equal(t, 2, rejections)

		require.Equal(t, int64(1), sum(t, tel, serviceScope, "messaging.client.sent.messages"),
			"only the failed record was sent")
		durations := histogramPoints(t, tel, "messaging.client.operation.duration")
		require.Len(t, durations, 1)
		require.Equal(t, uint64(1), durations[0].Count)
	})
}

func TestProduce_a_policy_that_never_trips_turns_the_breaker_off(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		c := cluster(t, h, 1)
		answerProduce(c, kerr.NotEnoughReplicas.Code, always)

		var errs []error
		tel := humustest.NewTelemetry()
		ps := producerSettings(c.ListenAddrs()...)
		ps.DeliveryTimeout = time.Second
		ps.BreakerPolicy.Trip = func(circuit.Counts) bool { return false }
		producesWith(t, h, tel, ps, func(ctx context.Context, p *kafka.Producer) error {
			for range 30 {
				errs = append(errs, p.Produce(ctx, value(0)))
			}
			return nil
		})

		require.Len(t, errs, 30)
		for i, err := range errs {
			require.ErrorIs(t, err, context.DeadlineExceeded, "call %d", i)
			require.NotErrorIs(t, err, circuit.ErrRejected, "call %d", i)
		}
		require.Empty(t, breakerLogs(tel))
	})
}

func TestClassify(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	record := func(err error) error { return fmt.Errorf("kafka: producing to orders, partition 0: %w", err) }
	testCases := []struct {
		name string
		err  error
		want circuit.Outcome
	}{
		{name: "every record acknowledged", err: nil, want: circuit.Success},
		{name: "MESSAGE_TOO_LARGE", err: record(fmt.Errorf("%w (uncompressed_bytes=2000000)", kerr.MessageTooLarge)), want: circuit.Ignored},
		{name: "INVALID_RECORD", err: record(kerr.InvalidRecord), want: circuit.Ignored},
		{name: "TOPIC_AUTHORIZATION_FAILED", err: record(kerr.TopicAuthorizationFailed), want: circuit.Ignored},
		{name: "UNKNOWN_TOPIC_OR_PARTITION, once franz-go gave up", err: record(kerr.UnknownTopicOrPartition), want: circuit.Ignored},
		{name: "every failed record its own", err: errors.Join(record(kerr.MessageTooLarge), record(kerr.InvalidRecord)), want: circuit.Ignored},
		{name: "DeliveryTimeout passing", err: record(context.DeadlineExceeded), want: circuit.Failure},
		{name: "kgo.ErrRecordTimeout", err: record(kgo.ErrRecordTimeout), want: circuit.Failure},
		{name: "a retriable code franz-go stopped on", err: record(kerr.NotEnoughReplicas), want: circuit.Failure},
		{name: "a failed connection", err: record(&net.OpError{Op: "dial", Err: errors.New("connection refused")}), want: circuit.Failure},
		{name: "one record its own, another the cluster's", err: errors.Join(record(kerr.MessageTooLarge), record(context.DeadlineExceeded)), want: circuit.Failure},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, kafka.Classify(ctx, tc.err))
		})
	}
}
