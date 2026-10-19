//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

// The tests in this file run Apps, with failure injection on, against the
// RabbitMQ in a container that connect_testcontainers_test.go starts, and a
// Service whose TrustScenario trusts the broker's login by its UserID, as
// the package doc's Failure injection recommends. Only RabbitMQ shows a
// user-id validated against a login and delivered to the consumer: the
// unit tests' stand-in implements no part of AMQP.

import (
	"bufio"
	"context"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// publishWithBaggage publishes a persistent message with id to queue,
// through the default exchange, with a baggage header of bag and the
// user-id userID, and waits for the broker to confirm it. The admin
// channel's connection logs in as the broker's user, so RabbitMQ accepts
// only that user, or none, as userID.
func publishWithBaggage(t *testing.T, queue, id, userID, bag string) {
	t.Helper()
	dc, err := adminChannel(t).PublishWithDeferredConfirmWithContext(t.Context(), "", queue, true, false, amqp.Publishing{
		MessageId:    id,
		UserId:       userID,
		DeliveryMode: amqp.Persistent,
		Headers:      amqp.Table{"baggage": bag},
		Body:         []byte(id),
	})
	require.NoError(t, err)
	ok, err := dc.WaitContext(t.Context())
	require.NoError(t, err)
	require.True(t, ok, "the broker nacked %s", id)
}

func TestService_strips_the_scenario_of_an_untrusted_delivery_and_keeps_one_trusted_by_its_UserID(t *testing.T) {
	t.Parallel()

	q := declareQueue(t)
	user := rabbit(t).username
	bag := scenarioBaggage(t, abortAccepted)
	publishWithBaggage(t, q, "untrusted", "", bag)
	publishWithBaggage(t, q, "trusted", user, bag)

	codec := faultsCodec(t)
	var (
		mu   sync.Mutex
		seen []processed
		done = make(chan struct{})
	)
	settings := consumerSettings(plainText(t), q)
	settings.TrustScenario = func(m rabbitmq.Message) bool {
		return m.UserID == user
	}
	tel := humustest.NewTelemetry()
	p, status := startApp(t, settings, queue.ProcessorFunc[rabbitmq.Message](func(ctx context.Context, m rabbitmq.Message) error {
		_, carried, _ := fit.ScenarioFromContext(ctx, codec)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, processed{
			id:       m.MessageID,
			scenario: carried,
			tenant:   baggage.FromContext(ctx).Member("tenant").Value(),
		})
		if len(seen) == 2 {
			close(done)
		}
		return nil
	}), humustest.WithApp(humus.WithTelemetry(tel.Builder()), withFaults(t)))

	select {
	case <-done:
	case st := <-status:
		t.Fatalf("the App exited before processing both deliveries: %v", st.Cause())
	case <-time.After(waitLimit):
		t.Fatal("the deliveries were never both processed")
	}
	p.Signal(syscall.SIGTERM)
	st := exit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())

	mu.Lock()
	require.Equal(t, []processed{
		{id: "untrusted", scenario: false, tenant: "acme"},
		{id: "trusted", scenario: true, tenant: "acme"},
	}, seen, "every other baggage member is kept")
	mu.Unlock()

	strips := map[string]int{}
	for _, span := range spansNamed(tel, "process "+q) {
		evs := spanEvents(span, stripEvent)
		for _, e := range evs {
			require.Empty(t, e.Attributes)
		}
		strips[spanAttrs(span)["messaging.message.id"].AsString()] = len(evs)
	}
	require.Equal(t, map[string]int{"untrusted": 1, "trusted": 0}, strips)
}

// trustedUser is a TrustScenario that trusts a delivery whose UserID is
// user.
func trustedUser(user string) func(rabbitmq.Message) bool {
	return func(m rabbitmq.Message) bool { return m.UserID == user }
}

// delivered is what Process saw of one delivery.
type delivered struct {
	queue    string
	id       string
	userID   string
	scenario bool
}

// deliveries records what Process saw of each delivery, in order, and logs
// it, with the time and the baggage header as it arrived.
type deliveries struct {
	t     *testing.T
	codec fit.Decoder
	mu    sync.Mutex
	seen  []delivered
}

func (d *deliveries) add(ctx context.Context, m rabbitmq.Message) {
	_, carried, _ := fit.ScenarioFromContext(ctx, d.codec)
	d.t.Logf("%s Process: queue %s, message %s, user-id %q, baggage header %q, scenario in its context: %t",
		time.Now().Format(time.RFC3339Nano), m.Queue, m.MessageID, m.UserID, m.Headers["baggage"], carried)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, delivered{queue: m.Queue, id: m.MessageID, userID: m.UserID, scenario: carried})
}

func (d *deliveries) all() []delivered {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]delivered(nil), d.seen...)
}

// forwarding returns DefaultPublisherSettings over c, forwarding the
// scenario.
func forwarding(c rabbitmq.Connection) rabbitmq.PublisherSettings {
	ps := rabbitmq.DefaultPublisherSettings()
	ps.Connection = c
	ps.ForwardScenario = true
	return ps
}

// logSpans logs each span of tel named one of names, with its start time,
// its message ID and its events.
func logSpans(t *testing.T, tel *humustest.Telemetry, names ...string) {
	t.Helper()
	for _, name := range names {
		for _, span := range spansNamed(tel, name) {
			var events []string
			for _, e := range span.Events() {
				events = append(events, e.Name)
			}
			t.Logf("%s span %q, message %s, events %v", span.StartTime().Format(time.RFC3339Nano),
				span.Name(), spanAttrs(span)["messaging.message.id"].AsString(), events)
		}
	}
}

// brokerLines returns the lines of the broker's log that contain s.
func brokerLines(t *testing.T, s string) []string {
	t.Helper()
	rc, err := rabbit(t).container.Logs(context.Background())
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	var out []string
	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		if strings.Contains(sc.Text(), s) {
			out = append(out, sc.Text())
		}
	}
	require.NoError(t, sc.Err())
	return out
}

// TestRabbitMQ_validates_a_user_id_and_leaves_a_message_published_without_one_with_none
// is RabbitMQ's half: it validates a user-id the publisher sets, refusing
// one that isn't the connection's user, and gives a message published
// without one none.
func TestRabbitMQ_validates_a_user_id_and_leaves_a_message_published_without_one_with_none(t *testing.T) {
	t.Parallel()

	q := declareQueue(t)
	user := rabbit(t).username
	publishWithBaggage(t, q, "none", "", tenantOnly(t))
	publishWithBaggage(t, q, "own", user, tenantOnly(t))

	got := takeReady(t, q)
	require.Len(t, got, 2)
	for _, d := range got {
		t.Logf("message %s arrived with user-id %q", d.MessageId, d.UserId)
	}
	require.Empty(t, got[0].UserId, "RabbitMQ sets no user-id of its own")
	require.Equal(t, user, got[1].UserId)

	other := "not-" + user
	ch := adminChannel(t)
	closes := ch.NotifyClose(make(chan *amqp.Error, 1))
	dc, err := ch.PublishWithDeferredConfirmWithContext(t.Context(), "", q, true, false, amqp.Publishing{
		MessageId: "other",
		UserId:    other,
		Body:      []byte("other"),
	})
	require.NoError(t, err)
	select {
	case e := <-closes:
		t.Logf("the channel closed: %v", e)
		require.NotNil(t, e)
		require.Equal(t, amqp.PreconditionFailed, e.Code)
	case <-time.After(waitLimit):
		t.Fatal("the broker never closed the channel")
	}
	ack, err := dc.WaitContext(t.Context())
	require.NoError(t, err)
	require.False(t, ack, "the refused message was never confirmed")

	for _, line := range brokerLines(t, other) {
		t.Logf("broker: %s", line)
	}
	require.Empty(t, takeReady(t, q), "the refused message never reached the queue")
}

// TestPublisher_a_Service_that_trusts_the_Publishers_user_keeps_the_scenario_it_forwards
// is the report's promise, from a job: a job's BAGGAGE carries a scenario,
// which its Publisher forwards, with the user-id the Publisher logs in as,
// and the Service, which trusts that user, keeps it.
func TestPublisher_a_Service_that_trusts_the_Publishers_user_keeps_the_scenario_it_forwards(t *testing.T) {
	t.Parallel()

	q := declareQueue(t)
	user := rabbit(t).username
	got := &deliveries{t: t, codec: faultsCodec(t)}
	var (
		mu        sync.Mutex
		pubErr    error
		processed = make(chan struct{})
		once      sync.Once
	)
	tel := humustest.NewTelemetry()
	_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		pub, err := rabbitmq.NewPublisher(s, forwarding(plainText(t)))
		if err != nil {
			return nil, err
		}
		settings := consumerSettings(plainText(t), q)
		settings.TrustScenario = trustedUser(user)
		svc, err := rabbitmq.NewService(s, settings, queue.ProcessorFunc[rabbitmq.Message](func(ctx context.Context, m rabbitmq.Message) error {
			got.add(ctx, m)
			once.Do(func() { close(processed) })
			return nil
		}))
		if err != nil {
			return nil, err
		}
		// The job publishes under its BAGGAGE, and ends the App once the
		// Service has processed what it published.
		return []humus.Service{svc, job.NewService(s, func(ctx context.Context) error {
			_, carried, _ := fit.ScenarioFromContext(ctx, faultsCodec(t))
			t.Logf("%s the job publishes, scenario in its context: %t", time.Now().Format(time.RFC3339Nano), carried)
			err := pub.Publish(ctx, rabbitmq.Publishing{RoutingKey: q, MessageID: "forwarded", UserID: user, Body: []byte("forwarded")})
			mu.Lock()
			pubErr = err
			mu.Unlock()
			if err != nil {
				return nil
			}
			select {
			case <-processed:
			case <-ctx.Done():
			}
			return nil
		})}, nil
	},
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(tel.Builder()), withFaults(t)),
		humustest.WithEnv(map[string]string{"BAGGAGE": scenarioBaggage(t, sparesAccepted)}),
	)
	st := exit(t, status)
	logSpans(t, tel, "send amq.default", "process "+q)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	mu.Lock()
	require.NoError(t, pubErr)
	mu.Unlock()

	seen := got.all()
	require.Len(t, seen, 1)
	require.Equal(t, user, seen[0].userID, "the message names the user the Publisher logged in as, which RabbitMQ validated")
	require.True(t, seen[0].scenario, "the Service trusted the scenario the Publisher forwarded")
	require.Empty(t, spanEvents(onlySpanNamed(t, tel, "process "+q), stripEvent))
}

// TestService_a_scenario_a_Processor_forwards_is_trusted_by_the_next_Service
// is the report's case: a delivery published with amqp091, with the
// trusted user-id, reaches Process with its scenario, and Process publishes
// it on with a humus Publisher that forwards the scenario, with the user-id
// the Publisher logs in as, to a second Service that trusts the same user.
func TestService_a_scenario_a_Processor_forwards_is_trusted_by_the_next_Service(t *testing.T) {
	t.Parallel()

	in, out := declareQueue(t), declareQueue(t)
	user := rabbit(t).username
	publishWithBaggage(t, in, "order-1", user, scenarioBaggage(t, sparesAccepted))

	got := &deliveries{t: t, codec: faultsCodec(t)}
	done := make(chan struct{})
	var once sync.Once
	tel := humustest.NewTelemetry()
	proc, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		pub, err := rabbitmq.NewPublisher(s, forwarding(plainText(t)))
		if err != nil {
			return nil, err
		}
		first := consumerSettings(plainText(t), in)
		first.TrustScenario = trustedUser(user)
		forwarder, err := rabbitmq.NewService(s, first, queue.ProcessorFunc[rabbitmq.Message](func(ctx context.Context, m rabbitmq.Message) error {
			got.add(ctx, m)
			return pub.Publish(ctx, rabbitmq.Publishing{RoutingKey: out, MessageID: m.MessageID, UserID: user, Body: m.Body})
		}))
		if err != nil {
			return nil, err
		}
		next := consumerSettings(plainText(t), out)
		next.TrustScenario = trustedUser(user)
		receiver, err := rabbitmq.NewService(s, next, queue.ProcessorFunc[rabbitmq.Message](func(ctx context.Context, m rabbitmq.Message) error {
			got.add(ctx, m)
			once.Do(func() { close(done) })
			return nil
		}))
		if err != nil {
			return nil, err
		}
		return []humus.Service{forwarder, receiver}, nil
	}, humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(tel.Builder()), withFaults(t)))

	select {
	case <-done:
	case st := <-status:
		t.Fatalf("the App exited before the second Service processed the message: %v", st.Cause())
	case <-time.After(waitLimit):
		t.Fatal("the second Service never processed the message")
	}
	proc.Signal(syscall.SIGTERM)
	st := exit(t, status)
	logSpans(t, tel, "process "+in, "send amq.default", "process "+out)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())

	require.Equal(t, []delivered{
		{queue: in, id: "order-1", userID: user, scenario: true},
		{queue: out, id: "order-1", userID: user, scenario: true},
	}, got.all(), "both Services trust the user, and keep the scenario")
}
