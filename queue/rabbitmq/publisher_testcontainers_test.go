//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

// The tests in this file run Apps whose job publishes with a Publisher, on
// humus.OSHost(), in real time, against the RabbitMQ in a container that
// connect_testcontainers_test.go starts. They show against RabbitMQ itself
// what the unit tests show on the stand-in: that a confirm, a return and a
// channel error are what the broker sends, and that the trace context the
// Publisher injects reaches the Service that consumes the message.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

// publishErrs runs an App on humus.OSHost() whose Init builds a Publisher
// over c, and whose job publishes each of ms with it, in order. It returns
// each Publish's error, once the App has exited 0.
func publishErrs(t *testing.T, c rabbitmq.Connection, ms ...rabbitmq.Publishing) []error {
	t.Helper()
	settings := rabbitmq.DefaultPublisherSettings()
	settings.Connection = c
	errs := make([]error, len(ms))
	_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		pub, err := rabbitmq.NewPublisher(s, settings)
		if err != nil {
			return nil, err
		}
		return []humus.Service{job.NewService(s, func(ctx context.Context) error {
			for i, m := range ms {
				errs[i] = pub.Publish(ctx, m)
			}
			return nil
		})}, nil
	}, humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder)))
	st := exit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	return errs
}

func TestPublisher_a_confirmed_publish_reaches_its_queue(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]func(*testing.T) rabbitmq.Connection{
		"in plain text": plainText,
		"over TLS":      overTLS,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			q := declareQueue(t)
			at := time.Now().Truncate(time.Second).UTC()
			m := rabbitmq.Publishing{
				RoutingKey:      q,
				Headers:         map[string]any{"tenant": "acme", "attempt": int32(1)},
				ContentType:     "application/json",
				ContentEncoding: "identity",
				Priority:        3,
				CorrelationID:   "corr-1",
				ReplyTo:         "replies",
				Expiration:      "600000",
				MessageID:       "order-1",
				Timestamp:       at,
				Type:            "order.accepted",
				AppID:           "orders-api",
				Body:            []byte(`{"id":1}`),
			}
			errs := publishErrs(t, c(t), m)
			require.NoError(t, errs[0])

			got := takeReady(t, q)
			require.Len(t, got, 1)
			d := got[0]
			require.Equal(t, amqp.Persistent, d.DeliveryMode, "published persistent")
			require.Equal(t, amqp.Table{"tenant": "acme", "attempt": int32(1)}, d.Headers)
			require.Equal(t, "application/json", d.ContentType)
			require.Equal(t, "identity", d.ContentEncoding)
			require.EqualValues(t, 3, d.Priority)
			require.Equal(t, "corr-1", d.CorrelationId)
			require.Equal(t, "replies", d.ReplyTo)
			require.Equal(t, "600000", d.Expiration)
			require.Equal(t, "order-1", d.MessageId)
			require.True(t, at.Equal(d.Timestamp), "timestamp %v, want %v", d.Timestamp, at)
			require.Equal(t, "order.accepted", d.Type)
			require.Equal(t, "orders-api", d.AppId)
			require.Equal(t, []byte(`{"id":1}`), d.Body)
		})
	}
}

func TestPublisher_an_unbound_routing_key_is_ErrUnroutable(t *testing.T) {
	t.Parallel()

	errs := publishErrs(t, plainText(t), rabbitmq.Publishing{
		Exchange:   "amq.direct",
		RoutingKey: unique("nobody-binds"),
		Body:       []byte("lost"),
	})
	require.ErrorIs(t, errs[0], rabbitmq.ErrUnroutable)
}

func TestPublisher_a_missing_exchange_is_404_NOT_FOUND_and_the_next_publish_is_confirmed(t *testing.T) {
	t.Parallel()

	q := declareQueue(t)
	errs := publishErrs(t, plainText(t),
		rabbitmq.Publishing{Exchange: unique("missing"), RoutingKey: q, Body: []byte("lost")},
		rabbitmq.Publishing{RoutingKey: q, MessageID: "kept", Body: []byte("kept")},
	)
	ae, ok := errors.AsType[*amqp.Error](errs[0])
	require.True(t, ok, "err = %v", errs[0])
	require.Equal(t, amqp.NotFound, ae.Code)

	require.NoError(t, errs[1])
	got := takeReady(t, q)
	require.Len(t, got, 1)
	require.Equal(t, "kept", got[0].MessageId)
}

func TestPublisher_carries_its_trace_to_the_Service_that_consumes_the_message(t *testing.T) {
	t.Parallel()

	q := declareQueue(t)
	var (
		got       recorder
		processed = make(chan struct{})
		once      sync.Once
		mu        sync.Mutex
		inSpan    trace.SpanContext
		pubErr    error
	)
	tel := humustest.NewTelemetry()
	_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := rabbitmq.DefaultPublisherSettings()
		settings.Connection = plainText(t)
		pub, err := rabbitmq.NewPublisher(s, settings)
		if err != nil {
			return nil, err
		}
		svc, err := rabbitmq.NewService(s, consumerSettings(plainText(t), q), queue.ProcessorFunc[rabbitmq.Message](func(ctx context.Context, m rabbitmq.Message) error {
			mu.Lock()
			inSpan = trace.SpanContextFromContext(ctx)
			mu.Unlock()
			got.add(m)
			once.Do(func() { close(processed) })
			return nil
		}))
		if err != nil {
			return nil, err
		}
		// The job publishes, and ends the App once the Service has
		// processed what it published.
		return []humus.Service{svc, job.NewService(s, func(ctx context.Context) error {
			err := pub.Publish(ctx, rabbitmq.Publishing{RoutingKey: q, MessageID: "traced", Body: []byte("traced")})
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
	}, humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(tel.Builder())))
	st := exit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	mu.Lock()
	require.NoError(t, pubErr)
	mu.Unlock()
	require.Equal(t, []string{"traced"}, got.ids())

	sends := spansNamed(tel, "send amq.default")
	require.Len(t, sends, 1)
	send := sends[0]
	require.Equal(t, trace.SpanKindProducer, send.SpanKind())
	require.Equal(t, q, spanAttrs(send)["messaging.destination.name"].AsString())

	processes := spansNamed(tel, "process "+q)
	require.Len(t, processes, 1)
	process := processes[0]
	require.Equal(t, send.SpanContext().TraceID(), process.SpanContext().TraceID(), "the consumer continues the publisher's trace")
	require.Equal(t, send.SpanContext().SpanID(), process.Parent().SpanID(), "the consumer's span is a child of the Publisher's send span")
	require.True(t, process.Parent().IsRemote(), "the parent came through the message's headers")
	mu.Lock()
	require.Equal(t, process.SpanContext(), inSpan, "Process's context carries the process span")
	mu.Unlock()
}

// TestPublisher_a_Publish_right_after_close_all_connections_is_confirmed_on_a_new_connection
// closes every connection to the broker, so it doesn't run in parallel with
// the other tests. Whether the Publisher learns of the close before its
// next Publish or during it, that Publish connects again and is confirmed.
func TestPublisher_a_Publish_right_after_close_all_connections_is_confirmed_on_a_new_connection(t *testing.T) {
	q := declareQueue(t)

	host := humus.OSHost()
	var dials atomic.Int32
	dial := host.Dial
	host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return dial(ctx, network, address)
	}
	settings := rabbitmq.DefaultPublisherSettings()
	settings.Connection = plainText(t)

	var (
		firstErr, secondErr error
		published           = make(chan struct{})
		closed              = make(chan struct{})
	)
	_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		pub, err := rabbitmq.NewPublisher(s, settings)
		if err != nil {
			return nil, err
		}
		return []humus.Service{job.NewService(s, func(ctx context.Context) error {
			firstErr = pub.Publish(ctx, rabbitmq.Publishing{RoutingKey: q, MessageID: "before", Body: []byte("before")})
			close(published)
			select {
			case <-closed:
			case <-ctx.Done():
				return ctx.Err()
			}
			secondErr = pub.Publish(ctx, rabbitmq.Publishing{RoutingKey: q, MessageID: "after", Body: []byte("after")})
			return nil
		})}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))

	<-published
	closeAllConnections(t)
	close(closed)
	st := exit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.NoError(t, firstErr)
	require.NoError(t, secondErr, "the Publish after close_all_connections was confirmed")
	require.Equal(t, int32(2), dials.Load(), "the Publisher connected again")

	got := takeReady(t, q)
	ids := make([]string, 0, len(got))
	for _, d := range got {
		ids = append(ids, d.MessageId)
	}
	require.Contains(t, ids, "before")
	require.Contains(t, ids, "after")
}
