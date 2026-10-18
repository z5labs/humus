// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file reach no broker. The Publisher's attempts run on a
// stand-in for amqp091's connection and channel, and its connecting code on
// a humustest Host with nothing listening, all inside testing/synctest.
// What needs RabbitMQ itself is in publisher_testcontainers_test.go.

// publisherSettings is DefaultPublisherSettings with an address and a
// login.
func publisherSettings() rabbitmq.PublisherSettings {
	s := rabbitmq.DefaultPublisherSettings()
	s.Connection.Addr = "rabbit:5672"
	s.Connection.SASL = &amqp.PlainAuth{Username: "orders", Password: rand.Text()}
	return s
}

// newPublisher returns a Publisher of settings on a new stand-in.
func newPublisher(t *testing.T, settings rabbitmq.PublisherSettings) (*rabbitmq.Publisher, *pubStandIn) {
	t.Helper()
	pub, err := rabbitmq.NewPublisher(setup(t), settings)
	require.NoError(t, err)
	s := newPubStandIn()
	rabbitmq.SetDial(pub, s.dial)
	return pub, s
}

// publishAsync calls Publish on a goroutine of its own, and returns the
// channel its error is sent on.
func publishAsync(ctx context.Context, pub *rabbitmq.Publisher, m rabbitmq.Publishing) <-chan error {
	done := make(chan error, 1)
	go func() { done <- pub.Publish(ctx, m) }()
	return done
}

// requirePending fails t if done has an error: the Publish has returned.
func requirePending(t *testing.T, done <-chan error) {
	t.Helper()
	synctest.Wait()
	select {
	case err := <-done:
		t.Fatalf("Publish returned before it was confirmed: %v", err)
	default:
	}
}

// accepted is the Publishing most tests publish.
var accepted = rabbitmq.Publishing{Exchange: "orders", RoutingKey: "accepted", Body: []byte("order 1")}

func TestPublisher_returns_nil_once_the_broker_confirms_and_publishes_every_field(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		pub, s := newPublisher(t, publisherSettings())
		at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		m := rabbitmq.Publishing{
			Exchange:        "orders",
			RoutingKey:      "accepted",
			Headers:         map[string]any{"tenant": "acme"},
			ContentType:     "application/json",
			ContentEncoding: "gzip",
			Priority:        5,
			CorrelationID:   "corr-1",
			ReplyTo:         "replies",
			Expiration:      "60000",
			MessageID:       "order-1",
			Timestamp:       at,
			Type:            "order.accepted",
			AppID:           "orders-api",
			Body:            []byte(`{"id":1}`),
		}
		done := publishAsync(t.Context(), pub, m)

		p := <-s.published
		requirePending(t, done)
		require.Equal(t, "orders", p.exchange)
		require.Equal(t, "accepted", p.key)
		require.True(t, p.mandatory, "the Publisher publishes with mandatory set")
		require.False(t, p.immediate)
		require.Equal(t, amqp.Publishing{
			Headers:         amqp.Table{"tenant": "acme"},
			ContentType:     "application/json",
			ContentEncoding: "gzip",
			DeliveryMode:    amqp.Persistent,
			Priority:        5,
			CorrelationId:   "corr-1",
			ReplyTo:         "replies",
			Expiration:      "60000",
			MessageId:       "order-1",
			Timestamp:       at,
			Type:            "order.accepted",
			AppId:           "orders-api",
			Body:            []byte(`{"id":1}`),
		}, p.msg)

		p.ack()
		require.NoError(t, <-done)
		require.Equal(t, 1, s.dials())
	})
}

func TestPublisher_Transient_publishes_with_delivery_mode_1(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		pub, s := newPublisher(t, publisherSettings())
		s.autoAck = true
		m := accepted
		m.Transient = true
		require.NoError(t, pub.Publish(t.Context(), m))
		require.Equal(t, amqp.Transient, (<-s.published).msg.DeliveryMode)
	})
}

func TestPublisher_never_changes_the_callers_Headers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		pub, s := newPublisher(t, publisherSettings())
		s.autoAck = true
		headers := map[string]any{"tenant": "acme", "attempt": int32(1)}
		m := accepted
		m.Headers = headers
		require.NoError(t, pub.Publish(t.Context(), m))

		p := <-s.published
		require.Equal(t, amqp.Table{"tenant": "acme", "attempt": int32(1)}, p.msg.Headers)
		// What the Publisher hands amqp091 is a copy.
		p.msg.Headers["traceparent"] = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
		require.Equal(t, map[string]any{"tenant": "acme", "attempt": int32(1)}, headers)
	})
}

func TestPublisher_a_return_before_the_confirm_is_ErrUnroutable(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		pub, s := newPublisher(t, publisherSettings())
		done := publishAsync(t.Context(), pub, accepted)
		p := <-s.published
		p.unroutable()
		err := <-done
		require.ErrorIs(t, err, rabbitmq.ErrUnroutable)
		require.ErrorContains(t, err, `exchange "orders" with routing key "accepted"`)

		// The channel carries the next publish.
		done = publishAsync(t.Context(), pub, accepted)
		next := <-s.published
		require.Same(t, p.ch, next.ch)
		next.ack()
		require.NoError(t, <-done)
	})
}

func TestPublisher_with_MaxAttempts_1_a_nack_is_returned_after_one_attempt(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.MaxAttempts = 1
		pub, s := newPublisher(t, settings)
		done := publishAsync(t.Context(), pub, accepted)
		(<-s.published).nack()
		err := <-done
		require.ErrorContains(t, err, "the broker nacked the message")
		require.NotErrorIs(t, err, rabbitmq.ErrUnroutable)
		synctest.Wait()
		require.Empty(t, s.published, "MaxAttempts 1 turns retries off")
	})
}

func TestPublisher_a_channel_error_is_returned_and_the_channel_replaced(t *testing.T) {
	t.Parallel()

	for name, code := range map[string]int{"404 NOT_FOUND": amqp.NotFound, "403 ACCESS_REFUSED": amqp.AccessRefused} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				pub, s := newPublisher(t, publisherSettings())
				done := publishAsync(t.Context(), pub, accepted)
				p := <-s.published
				p.ch.fail(&amqp.Error{Code: code, Reason: "refused", Server: true})
				err := <-done
				ae, ok := errors.AsType[*amqp.Error](err)
				require.True(t, ok, "err = %v", err)
				require.Equal(t, code, ae.Code)

				done = publishAsync(t.Context(), pub, accepted)
				next := <-s.published
				require.NotSame(t, p.ch, next.ch, "the next Publish gets a new channel")
				next.ack()
				require.NoError(t, <-done)
				require.Equal(t, 1, s.dials(), "on the same connection")
				require.Equal(t, 2, s.conn(0).channelCount())
			})
		})
	}
}

func TestPublisher_with_Channels_2_a_third_Publish_waits_for_a_confirm(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.Channels = 2
		pub, s := newPublisher(t, settings)

		first := publishAsync(t.Context(), pub, accepted)
		second := publishAsync(t.Context(), pub, accepted)
		third := publishAsync(t.Context(), pub, accepted)
		a, b := <-s.published, <-s.published
		require.NotSame(t, a.ch, b.ch, "each publish in flight has a channel of its own")
		synctest.Wait()
		require.Empty(t, s.published, "a third publish was made with two in flight")

		a.ack()
		c := <-s.published
		require.Same(t, a.ch, c.ch, "the third publish takes the channel the first freed")
		b.ack()
		c.ack()
		for _, done := range []<-chan error{first, second, third} {
			require.NoError(t, <-done)
		}
		require.Equal(t, 2, s.conn(0).channelCount())
	})
}

func TestPublisher_a_context_that_ends_before_the_confirm_ends_Publish_at_once(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		pub, s := newPublisher(t, publisherSettings())
		ctx, cancel := context.WithCancel(t.Context())
		done := publishAsync(ctx, pub, accepted)
		p := <-s.published
		requirePending(t, done)

		start := time.Now()
		cancel()
		err := <-done
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, time.Since(start))

		// A confirm may still arrive on the channel, so it is closed rather
		// than reused.
		synctest.Wait()
		closed, byPublisher := p.ch.isClosed()
		require.True(t, closed && byPublisher, "the Publisher did not close the channel")
		done = publishAsync(t.Context(), pub, accepted)
		next := <-s.published
		require.NotSame(t, p.ch, next.ch)
		next.ack()
		require.NoError(t, <-done)
	})
}

func TestPublisher_Timeout_ends_a_Publish_whose_confirm_never_comes(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.Timeout = 5 * time.Second
		pub, s := newPublisher(t, settings)

		start := time.Now()
		done := publishAsync(t.Context(), pub, accepted)
		<-s.published
		err := <-done
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, settings.Timeout, time.Since(start))
	})
}

func TestPublisher_an_earlier_deadline_of_the_callers_context_wins(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		pub, s := newPublisher(t, publisherSettings())
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		start := time.Now()
		done := publishAsync(ctx, pub, accepted)
		<-s.published
		require.ErrorIs(t, <-done, context.DeadlineExceeded)
		require.Equal(t, time.Second, time.Since(start))
	})
}

func TestPublisher_a_Publish_waiting_for_a_channel_ends_with_its_context(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.Channels = 1
		pub, s := newPublisher(t, settings)
		first := publishAsync(t.Context(), pub, accepted)
		p := <-s.published

		ctx, cancel := context.WithCancel(t.Context())
		second := publishAsync(ctx, pub, accepted)
		requirePending(t, second)
		cancel()
		err := <-second
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, "waiting for a channel")

		p.ack()
		require.NoError(t, <-first)
	})
}

func TestPublisher_concurrent_first_calls_connect_once(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		pub, s := newPublisher(t, publisherSettings())
		s.autoAck = true
		s.hold = make(chan struct{})

		var wg sync.WaitGroup
		errs := make([]error, 3)
		for i := range errs {
			wg.Go(func() { errs[i] = pub.Publish(t.Context(), accepted) })
		}
		synctest.Wait()
		s.hold <- struct{}{}
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		require.Equal(t, 1, s.dials())
	})
}

func TestPublisher_connects_again_after_the_connection_is_lost(t *testing.T) {
	t.Parallel()

	forced := &amqp.Error{Code: amqp.ConnectionForced, Reason: "CONNECTION_FORCED - closed via management plugin", Server: true}

	t.Run("while idle", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			pub, s := newPublisher(t, publisherSettings())
			s.autoAck = true
			require.NoError(t, pub.Publish(t.Context(), accepted))
			s.conn(0).lose(forced)

			require.NoError(t, pub.Publish(t.Context(), accepted))
			require.Equal(t, 2, s.dials())
		})
	})

	t.Run("with a publish in flight and MaxAttempts 1", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			settings := publisherSettings()
			settings.MaxAttempts = 1
			pub, s := newPublisher(t, settings)
			done := publishAsync(t.Context(), pub, accepted)
			<-s.published
			s.conn(0).lose(forced)
			err := <-done
			ae, ok := errors.AsType[*amqp.Error](err)
			require.True(t, ok, "err = %v", err)
			require.Equal(t, amqp.ConnectionForced, ae.Code)

			s.autoAck = true
			require.NoError(t, pub.Publish(t.Context(), accepted))
			require.Equal(t, 2, s.dials())
		})
	})
}

func TestPublisher_fails_with_the_dial_error_when_nothing_listens(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		pub, err := rabbitmq.NewPublisher(&humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}, publisherSettings())
		require.NoError(t, err)

		err = pub.Publish(t.Context(), accepted)
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
		require.ErrorContains(t, err, "rabbit:5672")
	})
}

// runPublisher runs an App whose Init builds a Publisher of settings on s,
// and whose one Service, a job, calls run with it.
func runPublisher(t *testing.T, s *pubStandIn, settings rabbitmq.PublisherSettings, run func(ctx context.Context, pub *rabbitmq.Publisher) error) (*rabbitmq.Publisher, int) {
	t.Helper()
	var pub *rabbitmq.Publisher
	_, status := humustest.Start(t, func(_ context.Context, setup *humus.Setup) ([]humus.Service, error) {
		p, err := rabbitmq.NewPublisher(setup, settings)
		if err != nil {
			return nil, err
		}
		rabbitmq.SetDial(p, s.dial)
		pub = p
		return []humus.Service{job.NewService(setup, func(ctx context.Context) error { return run(ctx, p) })}, nil
	}, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
	st := <-status
	return pub, st.Code()
}

func TestPublisher_OnStop_closes_the_connection_and_Publish_fails_at_once_after_it(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		s := newPubStandIn()
		s.autoAck = true
		pub, code := runPublisher(t, s, publisherSettings(), func(ctx context.Context, pub *rabbitmq.Publisher) error {
			return pub.Publish(ctx, accepted)
		})
		require.Zero(t, code)
		require.Equal(t, "the Publisher", s.conn(0).closer())

		start := time.Now()
		err := pub.Publish(t.Context(), accepted)
		require.ErrorContains(t, err, "the Publisher is stopped")
		require.Zero(t, time.Since(start))
		require.Equal(t, 1, s.dials(), "a Publish after OnStop connected")
	})
}

func TestPublisher_OnStop_runs_when_Init_fails_after_NewPublisher(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		s := newPubStandIn()
		s.autoAck = true
		errInit := errors.New("init failed")
		_, status := humustest.Start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			pub, err := rabbitmq.NewPublisher(setup, publisherSettings())
			if err != nil {
				return nil, err
			}
			rabbitmq.SetDial(pub, s.dial)
			if err := pub.Publish(ctx, accepted); err != nil {
				return nil, err
			}
			return nil, errInit
		}, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
		st := <-status
		require.ErrorIs(t, st.Cause(), errInit)
		require.Equal(t, "the Publisher", s.conn(0).closer())
	})
}

func TestPublisher_OnStop_ends_a_dial_in_progress_and_a_Publish_waiting_for_a_channel(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		s := newPubStandIn()
		s.hold = make(chan struct{})
		settings := publisherSettings()
		settings.Channels = 1

		errs := make(chan error, 2)
		_, code := runPublisher(t, s, settings, func(ctx context.Context, pub *rabbitmq.Publisher) error {
			// Both outlive the job, so the OnStop function finds one
			// Publish dialling and the other waiting for its channel.
			for range 2 {
				go func() { errs <- pub.Publish(context.WithoutCancel(ctx), accepted) }()
			}
			synctest.Wait()
			return nil
		})
		require.Zero(t, code)
		for range 2 {
			require.ErrorContains(t, <-errs, "the Publisher is stopped")
		}
		require.Zero(t, s.dials())
	})
}

func TestNewPublisher_opens_no_connection(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		l, err := h.Listen("tcp", "rabbit:5672")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()
		accepted := make(chan struct{})
		go func() {
			conn, err := l.Accept()
			if err == nil {
				_ = conn.Close()
				close(accepted)
			}
		}()

		// An Init that fails after NewPublisher: its OnStop function runs,
		// with no connection to close.
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			if _, err := rabbitmq.NewPublisher(s, publisherSettings()); err != nil {
				return nil, err
			}
			return nil, context.Canceled
		}, humustest.WithApp(humus.WithHost(h.Host())))
		st := <-status
		require.ErrorIs(t, st.Cause(), context.Canceled)
		synctest.Wait()
		select {
		case <-accepted:
			t.Fatal("NewPublisher connected to the broker")
		default:
		}
	})
}

func TestNewPublisher_accepts_DefaultPublisherSettings_with_Addr_and_SASL_set(t *testing.T) {
	t.Parallel()

	pub, err := rabbitmq.NewPublisher(setup(t), publisherSettings())
	require.NoError(t, err)
	require.NotNil(t, pub)
}

func TestNewPublisher_names_every_bad_field_together(t *testing.T) {
	t.Parallel()

	s := rabbitmq.DefaultPublisherSettings()
	s.Connection.Heartbeat = 0
	s.Channels = 0
	s.Timeout = 0
	s.MaxAttempts = 0
	s.BackoffBase = 0
	s.BackoffMax = -time.Second
	s.RetryBudget = retry.BudgetConfig{MaxTokens: 2, TokenRatio: 0}
	s.Breaker = circuit.Config{Window: 0, Probes: circuit.MaxCount + 1, OpenFor: 0, ProbeTimeout: -time.Second}
	s.BreakerPolicy = circuit.Policy{}
	pub, err := rabbitmq.NewPublisher(setup(t), s)
	require.Nil(t, pub)
	require.EqualError(t, err, "rabbitmq: NewPublisher: PublisherSettings.Connection.Addr is not host:port\n"+
		"PublisherSettings.Connection.SASL must be set\n"+
		"PublisherSettings.Connection.Heartbeat must be from 1s to 65535s\n"+
		"PublisherSettings.Channels must be from 1 to 65535\n"+
		"PublisherSettings.Timeout must be positive\n"+
		"PublisherSettings.MaxAttempts must be at least 1\n"+
		"PublisherSettings.BackoffBase must be positive\n"+
		"PublisherSettings.BackoffMax must be positive\n"+
		"PublisherSettings.RetryBudget.MaxTokens must be from 3 to 65536\n"+
		"PublisherSettings.RetryBudget.TokenRatio must be from 0.001 to 65536\n"+
		"PublisherSettings.Breaker.Window must be from 1 to 65536\n"+
		"PublisherSettings.Breaker.Probes must be from 1 to 65536\n"+
		"PublisherSettings.Breaker.OpenFor must be positive\n"+
		"PublisherSettings.Breaker.ProbeTimeout must be positive\n"+
		"PublisherSettings.BreakerPolicy.Trip must be set\n"+
		"PublisherSettings.BreakerPolicy.Probe must be set")
}

func TestNewPublisher_rejects_a_BreakerPolicy_with_only_one_function_set(t *testing.T) {
	t.Parallel()

	s := publisherSettings()
	s.BreakerPolicy.Probe = nil
	_, err := rabbitmq.NewPublisher(setup(t), s)
	require.EqualError(t, err, "rabbitmq: NewPublisher: PublisherSettings.BreakerPolicy.Probe must be set")

	s = publisherSettings()
	s.BreakerPolicy.Trip = nil
	_, err = rabbitmq.NewPublisher(setup(t), s)
	require.EqualError(t, err, "rabbitmq: NewPublisher: PublisherSettings.BreakerPolicy.Trip must be set")
}

func TestNewPublisher_names_a_BackoffMax_below_BackoffBase_with_the_other_bad_fields(t *testing.T) {
	t.Parallel()

	s := publisherSettings()
	s.Channels = 0
	s.BackoffBase = 2 * time.Second
	s.BackoffMax = time.Second
	s.RetryBudget.TokenRatio = 0
	pub, err := rabbitmq.NewPublisher(setup(t), s)
	require.Nil(t, pub)
	require.EqualError(t, err, "rabbitmq: NewPublisher: PublisherSettings.Channels must be from 1 to 65535\n"+
		"PublisherSettings.BackoffMax must be at least PublisherSettings.BackoffBase\n"+
		"PublisherSettings.RetryBudget.TokenRatio must be from 0.001 to 65536")
}

func TestNewPublisher_accepts_a_BackoffMax_equal_to_BackoffBase(t *testing.T) {
	t.Parallel()

	s := publisherSettings()
	s.BackoffBase = time.Second
	s.BackoffMax = time.Second
	_, err := rabbitmq.NewPublisher(setup(t), s)
	require.NoError(t, err)
}

func TestNewPublisher_checks_every_field(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		field  string
		change func(*rabbitmq.PublisherSettings)
	}{
		{field: "PublisherSettings.Connection.Addr", change: func(s *rabbitmq.PublisherSettings) { s.Connection.Addr = "amqp://rabbit" }},
		{field: "PublisherSettings.Connection.Vhost", change: func(s *rabbitmq.PublisherSettings) { s.Connection.Vhost = "" }},
		{field: "PublisherSettings.Connection.SASL", change: func(s *rabbitmq.PublisherSettings) { s.Connection.SASL = nil }},
		{field: "PublisherSettings.Connection.Heartbeat", change: func(s *rabbitmq.PublisherSettings) { s.Connection.Heartbeat = time.Millisecond }},
		{field: "PublisherSettings.Connection.ConnectTimeout", change: func(s *rabbitmq.PublisherSettings) { s.Connection.ConnectTimeout = 0 }},
		{field: "PublisherSettings.Channels", change: func(s *rabbitmq.PublisherSettings) { s.Channels = 65536 }},
		{field: "PublisherSettings.Timeout", change: func(s *rabbitmq.PublisherSettings) { s.Timeout = -time.Second }},
		{field: "PublisherSettings.MaxAttempts", change: func(s *rabbitmq.PublisherSettings) { s.MaxAttempts = 0 }},
		{field: "PublisherSettings.BackoffBase", change: func(s *rabbitmq.PublisherSettings) { s.BackoffBase = 0 }},
		{field: "PublisherSettings.BackoffMax", change: func(s *rabbitmq.PublisherSettings) { s.BackoffMax = 0 }},
		{field: "PublisherSettings.RetryBudget.MaxTokens", change: func(s *rabbitmq.PublisherSettings) { s.RetryBudget.MaxTokens = 2 }},
		{field: "PublisherSettings.RetryBudget.TokenRatio", change: func(s *rabbitmq.PublisherSettings) { s.RetryBudget.TokenRatio = 0.0001 }},
		{field: "PublisherSettings.Breaker.Window", change: func(s *rabbitmq.PublisherSettings) { s.Breaker.Window = 0 }},
		{field: "PublisherSettings.Breaker.Probes", change: func(s *rabbitmq.PublisherSettings) { s.Breaker.Probes = circuit.MaxCount + 1 }},
		{field: "PublisherSettings.Breaker.OpenFor", change: func(s *rabbitmq.PublisherSettings) { s.Breaker.OpenFor = 0 }},
		{field: "PublisherSettings.Breaker.ProbeTimeout", change: func(s *rabbitmq.PublisherSettings) { s.Breaker.ProbeTimeout = -time.Second }},
		{field: "PublisherSettings.BreakerPolicy.Trip", change: func(s *rabbitmq.PublisherSettings) { s.BreakerPolicy.Trip = nil }},
		{field: "PublisherSettings.BreakerPolicy.Probe", change: func(s *rabbitmq.PublisherSettings) { s.BreakerPolicy.Probe = nil }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			s := publisherSettings()
			tc.change(&s)
			_, err := rabbitmq.NewPublisher(setup(t), s)
			require.ErrorContains(t, err, "rabbitmq: NewPublisher: "+tc.field+" ")
		})
	}
}
