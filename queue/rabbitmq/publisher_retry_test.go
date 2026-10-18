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

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file show the Publisher's retries, on the stand-in of
// publisher_standin_test.go, inside testing/synctest, where the waits
// between attempts take no real time.

// requireNoMorePublishes fails t if the Publisher publishes again, now or
// within an hour.
func requireNoMorePublishes(t *testing.T, s *pubStandIn) {
	t.Helper()
	synctest.Wait()
	time.Sleep(time.Hour)
	synctest.Wait()
	require.Empty(t, s.published, "another attempt was made")
}

func TestPublisher_a_nack_and_then_a_confirm_succeeds_after_two_attempts(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		tp := newTracedPublisher(t)
		done := publishAsync(t.Context(), tp.pub, accepted)
		first := <-tp.s.published
		first.nack()
		second := <-tp.s.published
		require.Same(t, first.ch, second.ch, "a nacked channel carries the next attempt")
		second.ack()
		require.NoError(t, <-done)
		requireNoMorePublishes(t, tp.s)

		span := tp.onlySpan(t, "send orders")
		require.Equal(t, codes.Unset, span.Status().Code, "one span across both attempts, which succeeded")

		sent, ok := metricNamed(t, tp.tel, "messaging.client.sent.messages").Data.(metricdata.Sum[int64])
		require.True(t, ok)
		var total int64
		byErr := map[string]int64{}
		for _, dp := range sent.DataPoints {
			total += dp.Value
			byErr[setAttrs(dp.Attributes)["error.type"]] += dp.Value
		}
		require.Equal(t, int64(2), total, "messaging.client.sent.messages counts each attempt")
		require.Equal(t, map[string]int64{"nacked": 1, "": 1}, byErr)

		durations, ok := metricNamed(t, tp.tel, "messaging.client.operation.duration").Data.(metricdata.Histogram[float64])
		require.True(t, ok)
		var count uint64
		for _, dp := range durations.DataPoints {
			count += dp.Count
		}
		require.Equal(t, uint64(2), count, "messaging.client.operation.duration records each attempt")
	})
}

func TestPublisher_an_attempt_after_a_lost_channel_takes_a_new_one(t *testing.T) {
	t.Parallel()

	t.Run("on a new connection, when the connection was lost", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			pub, s := newPublisher(t, publisherSettings())
			done := publishAsync(t.Context(), pub, accepted)
			first := <-s.published
			s.conn(0).lose(&amqp.Error{Code: amqp.ConnectionForced, Reason: "CONNECTION_FORCED - broker forced connection closure with reason 'shutdown'", Server: true})

			second := <-s.published
			require.NotSame(t, first.ch, second.ch, "the attempt after a lost connection takes a new channel")
			second.ack()
			require.NoError(t, <-done)
			require.Equal(t, 2, s.dials(), "it connected again")
			require.Same(t, s.conn(1), second.ch.conn)
		})
	})

	t.Run("on the same connection, when only the channel closed", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			pub, s := newPublisher(t, publisherSettings())
			done := publishAsync(t.Context(), pub, accepted)
			first := <-s.published
			// The channel closes with no error of the broker's: its answer
			// is lost with it.
			first.ch.shutdown(nil)

			second := <-s.published
			require.NotSame(t, first.ch, second.ch)
			second.ack()
			require.NoError(t, <-done)
			require.Equal(t, 1, s.dials())
			require.Equal(t, 2, s.conn(0).channelCount())
		})
	})
}

func TestPublisher_what_the_broker_refused_is_returned_after_one_attempt(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		fail  func(*published)
		check func(*testing.T, error)
	}{
		"ErrUnroutable": {
			fail:  (*published).unroutable,
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, rabbitmq.ErrUnroutable) },
		},
		"404 NOT_FOUND": {
			fail: func(p *published) {
				p.ch.fail(&amqp.Error{Code: amqp.NotFound, Reason: "NOT_FOUND - no exchange 'orders' in vhost '/'", Server: true})
			},
			check: func(t *testing.T, err error) {
				ae, ok := errors.AsType[*amqp.Error](err)
				require.True(t, ok, "err = %v", err)
				require.Equal(t, amqp.NotFound, ae.Code)
			},
		},
		"403 ACCESS_REFUSED": {
			fail: func(p *published) {
				p.ch.fail(&amqp.Error{Code: amqp.AccessRefused, Reason: "ACCESS_REFUSED - write access to exchange 'orders' refused", Server: true})
			},
			check: func(t *testing.T, err error) {
				ae, ok := errors.AsType[*amqp.Error](err)
				require.True(t, ok, "err = %v", err)
				require.Equal(t, amqp.AccessRefused, ae.Code)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				pub, s := newPublisher(t, publisherSettings())
				done := publishAsync(t.Context(), pub, accepted)
				tc.fail(<-s.published)
				tc.check(t, <-done)
				requireNoMorePublishes(t, s)
			})
		})
	}
}

func TestPublisher_a_nack_is_retried_MaxAttempts_times_in_all(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.MaxAttempts = 3
		pub, s := newPublisher(t, settings)
		s.autoNack = true
		err := pub.Publish(t.Context(), accepted)
		require.ErrorContains(t, err, "the broker nacked the message")
		require.Len(t, s.published, 3)
	})
}

func TestPublisher_Timeout_ends_the_retries(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		settings := publisherSettings()
		settings.Timeout = time.Second
		// Neither the attempts nor the budget end the retries first.
		settings.MaxAttempts = 100
		settings.RetryBudget.MaxTokens = 1000
		pub, s := newPublisher(t, settings)

		// The broker nacks every attempt 400ms after it is published.
		var (
			mu      sync.Mutex
			started []time.Time
		)
		stop := make(chan struct{})
		var nacks sync.WaitGroup
		go func() {
			for {
				select {
				case p := <-s.published:
					mu.Lock()
					started = append(started, time.Now())
					mu.Unlock()
					nacks.Go(func() {
						time.Sleep(400 * time.Millisecond)
						p.nack()
					})
				case <-stop:
					return
				}
			}
		}()

		start := time.Now()
		err := pub.Publish(t.Context(), accepted)
		returned := time.Since(start)
		require.Error(t, err)
		require.LessOrEqual(t, returned, settings.Timeout, "Publish outlived its Timeout")

		synctest.Wait()
		time.Sleep(time.Hour)
		synctest.Wait()
		close(stop)
		nacks.Wait()
		mu.Lock()
		defer mu.Unlock()
		require.GreaterOrEqual(t, len(started), 2, "the first nack was retried")
		for i, at := range started {
			require.Less(t, at.Sub(start), settings.Timeout, "attempt %d started after the Timeout", i+1)
		}
		if returned == settings.Timeout {
			// The deadline cut an attempt short.
			require.ErrorIs(t, err, context.DeadlineExceeded)
		} else {
			// retry saw the deadline would come before the wait ended.
			require.ErrorContains(t, err, "the broker nacked the message")
		}
	})
}

func TestPublisher_a_caller_that_cancels_makes_no_further_attempt(t *testing.T) {
	t.Parallel()

	t.Run("while an attempt waits for its confirm", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			pub, s := newPublisher(t, publisherSettings())
			ctx, cancel := context.WithCancel(t.Context())
			done := publishAsync(ctx, pub, accepted)
			<-s.published
			requirePending(t, done)
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			requireNoMorePublishes(t, s)
		})
	})

	t.Run("while it waits to retry a nack", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			settings := publisherSettings()
			// A wait drawn from [0, 1h), within a Timeout of two hours.
			settings.Timeout = 2 * time.Hour
			settings.BackoffBase = time.Hour
			settings.BackoffMax = time.Hour
			pub, s := newPublisher(t, settings)
			ctx, cancel := context.WithCancel(t.Context())
			done := publishAsync(ctx, pub, accepted)
			(<-s.published).nack()
			requirePending(t, done)
			cancel()
			err := <-done
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "the broker nacked the message", "the caller sees why the attempt before failed")
			requireNoMorePublishes(t, s)
		})
	})
}

func TestPublisher_retries_stop_once_the_budget_is_down_to_half(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// The default budget: 10 tokens, each nack taking one, and no retry
		// while 5 or fewer are left. The breaker, which would open after the
		// fifth nack in a row, is turned off, so the budget alone decides.
		settings := publisherSettings()
		settings.BreakerPolicy = neverTrips
		pub, s := newPublisher(t, settings)
		s.autoNack = true

		var attempts []int
		for range 5 {
			require.ErrorContains(t, pub.Publish(t.Context(), accepted), "the broker nacked the message")
			attempts = append(attempts, len(s.published))
			for len(s.published) > 0 {
				<-s.published
			}
		}
		// 9, 8, 7: MaxAttempts. 6, 5: the budget is at half. Then one
		// attempt each.
		require.Equal(t, []int{3, 2, 1, 1, 1}, attempts)
	})
}

func TestPublisher_each_failed_dial_is_retried_MaxAttempts_times_in_all(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		host := h.Host()
		var (
			mu    sync.Mutex
			dials []time.Time
		)
		dial := host.Dial
		host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			dials = append(dials, time.Now())
			mu.Unlock()
			return dial(ctx, network, address)
		}
		settings := publisherSettings()
		settings.MaxAttempts = 4
		pub, err := rabbitmq.NewPublisher(&humus.Setup{Host: host, Telemetry: telemetry.Noop()}, settings)
		require.NoError(t, err)

		err = pub.Publish(t.Context(), accepted)
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
		require.ErrorContains(t, err, "rabbit:5672")
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, dials, settings.MaxAttempts, "every attempt dialled")
	})
}
