//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

// The tests in this file run whole Apps on humus.OSHost(), in real time,
// against the RabbitMQ in a container that connect_testcontainers_test.go
// starts. They show against RabbitMQ itself what the unit tests show on the
// stand-in, and what only a broker can: what it requeues, and what it
// counts toward a quorum queue's delivery limit.

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"go.uber.org/goleak"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// waitLimit bounds every wait of a test for the broker or the App.
const waitLimit = 30 * time.Second

// quorumQueue declares a quorum queue of the test's own, and returns its
// name. RabbitMQ counts its failed deliveries in x-delivery-count.
func quorumQueue(t *testing.T) string {
	t.Helper()
	return declare(t, amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeQuorum})
}

// publish publishes a persistent message with each ID to queue, through the
// default exchange, and waits for the broker to confirm each.
func publish(t *testing.T, queue string, ids ...string) {
	t.Helper()
	ch := adminChannel(t)
	for _, id := range ids {
		dc, err := ch.PublishWithDeferredConfirmWithContext(t.Context(), "", queue, true, false, amqp.Publishing{
			MessageId:    id,
			DeliveryMode: amqp.Persistent,
			Body:         []byte(id),
		})
		require.NoError(t, err)
		ok, err := dc.WaitContext(t.Context())
		require.NoError(t, err)
		require.True(t, ok, "the broker nacked %s", id)
	}
}

// ids returns the numbers from 0 to n-1, as strings.
func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strconv.Itoa(i)
	}
	return out
}

// queueState returns how many messages queue has ready, and how many
// consumers.
func queueState(t *testing.T, queue string) (ready, consumers int) {
	t.Helper()
	q, err := adminChannel(t).QueueDeclarePassive(queue, true, false, false, false, nil)
	require.NoError(t, err)
	return q.Messages, q.Consumers
}

// takeReady takes every message ready on queue, in order, acking each.
func takeReady(t *testing.T, queue string) []amqp.Delivery {
	t.Helper()
	ch := adminChannel(t)
	var out []amqp.Delivery
	for {
		d, ok, err := ch.Get(queue, true)
		require.NoError(t, err)
		if !ok {
			return out
		}
		out = append(out, d)
	}
}

// deliveryCount returns the x-delivery-count header of headers: how many
// failed deliveries a quorum queue has counted for the message, 0 when it
// has counted none.
func deliveryCount(t *testing.T, headers map[string]any) int64 {
	t.Helper()
	switch v := headers["x-delivery-count"].(type) {
	case nil:
		return 0
	case int64:
		return v
	case int32:
		return int64(v)
	case int16:
		return int64(v)
	case int8:
		return int64(v)
	case uint8:
		return int64(v)
	default:
		t.Fatalf("x-delivery-count is a %T: %v", v, v)
		return 0
	}
}

// consumerSettings is DefaultSettings consuming queue over c.
func consumerSettings(c rabbitmq.Connection, queue string) rabbitmq.Settings {
	s := rabbitmq.DefaultSettings()
	s.Connection = c
	s.Queue = queue
	return s
}

// consumes returns an Init whose only Service consumes settings with p.
func consumes(settings rabbitmq.Settings, p queue.ProcessorFunc[rabbitmq.Message]) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := rabbitmq.NewService(s, settings, p)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
}

// startApp starts an App on humus.OSHost() whose only Service consumes
// settings with p.
func startApp(t *testing.T, settings rabbitmq.Settings, p queue.ProcessorFunc[rabbitmq.Message], opts ...humustest.Option) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	all := append([]humustest.Option{humustest.WithApp(humus.WithHost(humus.OSHost()))}, opts...)
	return humustest.Start(t, consumes(settings, p), all...)
}

// exit waits for the App's exit status.
func exit(t *testing.T, status <-chan bedrock.ExitStatus) bedrock.ExitStatus {
	t.Helper()
	select {
	case st := <-status:
		return st
	case <-time.After(waitLimit):
		t.Fatal("the App did not exit")
		return bedrock.ExitStatus{}
	}
}

// recorder records the messages a Processor was given, in order.
type recorder struct {
	mu   sync.Mutex
	msgs []rabbitmq.Message
}

func (r *recorder) add(m rabbitmq.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
}

func (r *recorder) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.msgs))
	for i, m := range r.msgs {
		out[i] = m.MessageID
	}
	return out
}

func (r *recorder) messages() []rabbitmq.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.msgs)
}

// eventually fails the test unless cond holds within waitLimit.
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	require.Eventually(t, cond, waitLimit, 20*time.Millisecond, msg)
}

// inFlight blocks the Processor on the message with ID id, which it reports
// on started, until release is closed.
type inFlight struct {
	id      string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newInFlight(t *testing.T, id string) *inFlight {
	f := &inFlight{id: id, started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(f.open)
	return f
}

// hold blocks on m if it is the message held.
func (f *inFlight) hold(m rabbitmq.Message) {
	if m.MessageID != f.id {
		return
	}
	close(f.started)
	<-f.release
}

func (f *inFlight) open() { f.once.Do(func() { close(f.release) }) }

// await waits until the message held is being processed.
func (f *inFlight) await(t *testing.T) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(waitLimit):
		t.Fatalf("message %s was never processed", f.id)
	}
}

func TestService_processes_in_order_and_drains_on_SIGTERM(t *testing.T) {
	t.Parallel()

	q := quorumQueue(t)
	publish(t, q, ids(8)...)

	var got recorder
	held := newInFlight(t, "5")
	p, status := startApp(t, consumerSettings(plainText(t), q), func(_ context.Context, m rabbitmq.Message) error {
		got.add(m)
		held.hold(m)
		return nil
	})
	held.await(t)
	require.Equal(t, ids(6), got.ids(), "five are processed in order, and the sixth is in flight")

	p.Signal(syscall.SIGTERM)
	// The sixth finishes once the consumer is cancelled, which the broker
	// can't show: a quorum queue keeps counting a cancelled consumer until
	// the messages it holds are settled.
	time.Sleep(2 * time.Second)
	held.open()

	st := exit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.Equal(t, ids(6), got.ids(), "nothing more was processed")

	eventually(t, func() bool {
		ready, consumers := queueState(t, q)
		return ready == 2 && consumers == 0
	}, "the queue never had the two prefetched behind the sixth ready again, and no consumer")
	rest := takeReady(t, q)
	require.Len(t, rest, 2)
	for i, d := range rest {
		require.Equal(t, strconv.Itoa(6+i), d.MessageId, "they are ready in their order")
		require.Zero(t, deliveryCount(t, d.Headers), "a nack counts nothing toward the delivery limit")
	}
}

func TestInit_at_least_once_stops_on_a_failure_and_redelivers(t *testing.T) {
	t.Parallel()

	q := quorumQueue(t)
	publish(t, q, ids(5)...)

	bad := errors.New("cannot process")
	run := func(fail bool) (bedrock.ExitStatus, []rabbitmq.Message) {
		var got recorder
		p, status := startApp(t, consumerSettings(plainText(t), q), func(_ context.Context, m rabbitmq.Message) error {
			got.add(m)
			if fail && m.MessageID == "3" {
				return bad
			}
			return nil
		})
		if !fail {
			eventually(t, func() bool { return len(got.ids()) == 2 }, "the redelivered messages were never processed")
			p.Signal(syscall.SIGTERM)
		}
		return exit(t, status), got.messages()
	}

	st, seen := run(true)
	require.Equal(t, humus.ExitFailure, st.Code())
	require.ErrorIs(t, st.Cause(), bad)
	require.ErrorContains(t, st.Cause(), q)
	require.ErrorContains(t, st.Cause(), "message 3")
	require.Equal(t, []string{"0", "1", "2", "3"}, idsOf(seen))

	st, seen = run(false)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.ElementsMatch(t, []string{"3", "4"}, idsOf(seen))
	for _, m := range seen {
		switch m.MessageID {
		case "3":
			require.Equal(t, int64(1), deliveryCount(t, m.Headers), "3 was rejected, which counts once")
			require.True(t, m.Redelivered)
		case "4":
			require.Zero(t, deliveryCount(t, m.Headers), "4 was nacked, which counts nothing")
		}
	}
}

// idsOf returns the IDs of msgs.
func idsOf(msgs []rabbitmq.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.MessageID
	}
	return out
}

func TestService_a_grace_period_that_runs_out_counts_each_unacked_delivery_once(t *testing.T) {
	t.Parallel()

	q := quorumQueue(t)
	publish(t, q, ids(3)...)

	started := make(chan struct{}, 3)
	p, status := startApp(t, consumerSettings(plainText(t), q), func(ctx context.Context, _ rabbitmq.Message) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}, humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "1s"}))
	select {
	case <-started:
	case <-time.After(waitLimit):
		t.Fatal("nothing was processed")
	}
	p.Signal(syscall.SIGTERM)

	st := exit(t, status)
	require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
	_, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
	require.True(t, ok, "the cause is a *humus.GracePeriodError: %v", st.Cause())

	eventually(t, func() bool { ready, _ := queueState(t, q); return ready == 3 }, "the unacked deliveries were never requeued")
	rest := takeReady(t, q)
	require.ElementsMatch(t, ids(3), []string{rest[0].MessageId, rest[1].MessageId, rest[2].MessageId})
	for _, d := range rest {
		require.Equal(t, int64(1), deliveryCount(t, d.Headers), "%s counted once toward the delivery limit", d.MessageId)
	}
}

// TestService_at_most_once_never_redelivers_a_batch_it_acked closes every
// connection to the broker, so it doesn't run in parallel with the other
// tests.
func TestService_at_most_once_never_redelivers_a_batch_it_acked(t *testing.T) {
	q := declareQueue(t)
	publish(t, q, ids(3)...)

	var got recorder
	held := newInFlight(t, "0")
	s := consumerSettings(plainText(t), q)
	s.Delivery = queue.AtMostOnce
	s.Prefetch = 3
	_, status := startApp(t, s, func(_ context.Context, m rabbitmq.Message) error {
		got.add(m)
		held.hold(m)
		return nil
	})
	held.await(t)

	code, out, err := rabbit(t).container.Exec(t.Context(), []string{"rabbitmqctl", "close_all_connections", "humus test"})
	require.NoError(t, err)
	if code != 0 {
		b, _ := io.ReadAll(out)
		t.Fatalf("rabbitmqctl close_all_connections exited %d: %s", code, b)
	}

	// The batch was acked, and the ack confirmed, before 0 was processed,
	// so the rest of it is processed too once 0 finishes. In this story a
	// lost connection then ends Serve; the reconnect comes later.
	held.open()
	st := exit(t, status)
	require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
	require.ErrorContains(t, st.Cause(), "lost the connection consuming "+q)

	// None of the batch, the first messages, is ready again: only what was
	// prefetched behind it and never acked is.
	ready := []string{}
	for _, d := range takeReady(t, q) {
		ready = append(ready, d.MessageId)
	}
	batch := 3 - len(ready)
	t.Logf("the batch was %d messages", batch)
	require.GreaterOrEqual(t, batch, 1)
	require.Equal(t, ids(3)[:batch], got.ids(), "the batch was processed, once")
	require.Equal(t, ids(3)[batch:], ready, "no message of the batch is delivered again")
}

func TestService_stops_when_the_broker_refuses_the_queue_or_the_login(t *testing.T) {
	t.Parallel()

	t.Run("a missing queue", func(t *testing.T) {
		t.Parallel()

		_, status := startApp(t, consumerSettings(plainText(t), unique("missing")), func(context.Context, rabbitmq.Message) error {
			return nil
		})
		st := exit(t, status)
		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		require.ErrorContains(t, st.Cause(), "NOT_FOUND")
	})

	t.Run("a wrong password", func(t *testing.T) {
		t.Parallel()

		c := plainText(t)
		c.SASL = &amqp.PlainAuth{Username: rabbit(t).username, Password: rand.Text()}
		_, status := startApp(t, consumerSettings(c, declareQueue(t)), func(context.Context, rabbitmq.Message) error {
			return nil
		})
		st := exit(t, status)
		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		require.ErrorIs(t, st.Cause(), amqp.ErrCredentials)
	})
}

func TestService_consumes_over_TLS(t *testing.T) {
	t.Parallel()

	q := declareQueue(t)
	publish(t, q, ids(3)...)

	var got recorder
	p, status := startApp(t, consumerSettings(overTLS(t), q), func(_ context.Context, m rabbitmq.Message) error {
		got.add(m)
		return nil
	})
	eventually(t, func() bool { return len(got.ids()) == 3 }, "the messages were never processed")
	p.Signal(syscall.SIGTERM)

	st := exit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.Equal(t, ids(3), got.ids())
	ready, consumers := queueState(t, q)
	require.Zero(t, ready, "every message was acked")
	require.Zero(t, consumers)
}

// TestService_leaves_no_goroutine_of_amqp091_behind counts the process's
// goroutines, so it doesn't run in parallel with the other tests.
func TestService_leaves_no_goroutine_of_amqp091_behind(t *testing.T) {
	q := declareQueue(t)
	publish(t, q, "0")
	// Everything running now, the test's own amqp091 connections included,
	// is not the Service's.
	before := goleak.IgnoreCurrent()

	var got recorder
	p, status := startApp(t, consumerSettings(plainText(t), q), func(_ context.Context, m rabbitmq.Message) error {
		got.add(m)
		return nil
	})
	eventually(t, func() bool { return len(got.ids()) == 1 }, "the message was never processed")
	p.Signal(syscall.SIGTERM)

	st := exit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.NoError(t, goleak.Find(before))
}
