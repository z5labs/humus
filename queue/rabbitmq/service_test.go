// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// The tests in this file reach no broker. The consume loop runs on a
// stand-in for amqp091's channel, and the connecting code on a humustest
// Host with nothing listening, all inside testing/synctest. What needs
// RabbitMQ itself is in service_testcontainers_test.go.

// queueName is the queue every unit test consumes.
const queueName = "orders"

// settings is DefaultSettings with an address, a login and queueName.
func settings() rabbitmq.Settings {
	s := rabbitmq.DefaultSettings()
	s.Connection.Addr = "rabbit:5672"
	s.Connection.SASL = &amqp.PlainAuth{Username: "orders", Password: rand.Text()}
	s.Queue = queueName
	return s
}

// The calls the Service makes on the stand-in when it starts, and when it
// closes, under the default Settings.
const (
	qos10        = "qos 10"
	consume      = "consume orders humus autoAck=false exclusive=false noLocal=false noWait=false args=map[]"
	cancel       = "cancel humus noWait=false"
	closeChannel = "close channel"
	closeConn    = "close connection"
)

// unit is a Service on a stand-in, with the telemetry it logs to.
type unit struct {
	svc *rabbitmq.Service
	ch  *standIn
	tel *humustest.Telemetry
}

// newUnit returns a Service of settings and p on a new stand-in. It must be
// called inside a synctest bubble.
func newUnit(t *testing.T, settings rabbitmq.Settings, p queue.ProcessorFunc[rabbitmq.Message]) *unit {
	t.Helper()
	tel := humustest.NewTelemetry()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	h := humustest.NewHost(t)
	svc, err := rabbitmq.NewService(&humus.Setup{Host: h.Host(), Telemetry: sdk}, settings, p)
	require.NoError(t, err)
	ch := newStandIn()
	rabbitmq.SetOpen(svc, ch.open)
	return &unit{svc: svc, ch: ch, tel: tel}
}

// serve calls Serve with ctx on a goroutine of its own, and returns the
// channel its error is sent on.
func (u *unit) serve(ctx context.Context) <-chan error {
	served := make(chan error, 1)
	go func() { served <- u.svc.Serve(ctx) }()
	return served
}

// seen records the IDs of the messages a Processor was given, in order.
type seen struct {
	mu  sync.Mutex
	ids []string
}

func (s *seen) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, id)
}

func (s *seen) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ids)
}

// gate holds the Processor on the messages of some IDs until it is opened.
type gate struct {
	held map[string]chan struct{}
}

func newGate(ids ...string) *gate {
	g := &gate{held: map[string]chan struct{}{}}
	for _, id := range ids {
		g.held[id] = make(chan struct{})
	}
	return g
}

// wait blocks until id is released, if it is held.
func (g *gate) wait(id string) {
	if c, ok := g.held[id]; ok {
		<-c
	}
}

func (g *gate) release(id string) {
	close(g.held[id])
}

// records returns the log records emitted under the Service's scope with
// body.
func records(logs []sdklog.Record, body string) []sdklog.Record {
	var out []sdklog.Record
	for _, r := range logs {
		if r.InstrumentationScope().Name == "github.com/z5labs/humus/queue/rabbitmq" && r.Body().AsString() == body {
			out = append(out, r)
		}
	}
	return out
}

// attrs returns the attributes of r, by key, as strings.
func attrs(r sdklog.Record) map[string]string {
	out := make(map[string]string)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value.String()
		return true
	})
	return out
}

func TestService_at_least_once_acks_each_delivery_once_Process_returns_nil(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var got seen
		u := newUnit(t, settings(), func(_ context.Context, m rabbitmq.Message) error {
			got.add(m.MessageID)
			return nil
		})
		served := u.serve(t.Context())
		u.ch.deliver("0", "1", "2")
		synctest.Wait()

		require.Equal(t, []string{"0", "1", "2"}, got.get())
		require.Equal(t, []string{qos10, consume, "ack 1", "ack 2", "ack 3"}, u.ch.Calls())

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
		// Nothing was left unprocessed, so nothing is nacked.
		require.Equal(t, []string{qos10, consume, "ack 1", "ack 2", "ack 3", cancel, closeChannel, closeConn}, u.ch.Calls())
	})
}

func TestService_at_least_once_failure_rejects_it_acks_in_flight_and_nacks_the_rest(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		bad := errors.New("cannot process")
		g := newGate("a")
		var got seen
		s := settings()
		s.Concurrency = 2
		u := newUnit(t, s, func(_ context.Context, m rabbitmq.Message) error {
			got.add(m.MessageID)
			g.wait(m.MessageID)
			if m.MessageID == "b" {
				return bad
			}
			return nil
		})
		served := u.serve(t.Context())
		u.ch.deliver("a")
		synctest.Wait() // a is in flight
		u.ch.deliver("b", "c", "d")
		synctest.Wait()

		// b failed while a is in flight: b is rejected and the consumer
		// cancelled, and a is still processing.
		require.Equal(t, []string{qos10, consume, "reject 2 requeue=true", cancel}, u.ch.Calls())
		select {
		case err := <-served:
			t.Fatalf("Serve returned before the delivery in flight finished: %v", err)
		default:
		}

		g.release("a")
		err := <-served
		require.ErrorIs(t, err, bad)
		require.ErrorContains(t, err, "orders")
		require.ErrorContains(t, err, "message b")
		require.ElementsMatch(t, []string{"a", "b"}, got.get(), "c and d were never processed")
		require.Equal(t, []string{
			qos10, consume,
			"reject 2 requeue=true",
			cancel,
			"ack 1",
			"nack 4 multiple=true requeue=true",
			closeChannel, closeConn,
		}, u.ch.Calls())
	})
}

func TestService_at_least_once_failure_without_a_message_ID_names_the_queue(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		bad := errors.New("cannot process")
		u := newUnit(t, settings(), func(context.Context, rabbitmq.Message) error { return bad })
		served := u.serve(t.Context())
		u.ch.deliver("")

		err := <-served
		require.ErrorIs(t, err, bad)
		require.EqualError(t, err, "rabbitmq: processing a message from orders: cannot process")
		require.Equal(t, []string{qos10, consume, "reject 1 requeue=true", cancel, closeChannel, closeConn}, u.ch.Calls())
	})
}

func TestService_at_most_once_acks_a_batch_and_waits_for_the_Qos_before_processing_it(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		bad := errors.New("cannot process")
		var got seen
		s := settings()
		s.Delivery = queue.AtMostOnce
		s.Prefetch = 3
		u := newUnit(t, s, func(_ context.Context, m rabbitmq.Message) error {
			got.add(m.MessageID)
			if m.MessageID == "1" {
				return bad
			}
			return nil
		})
		u.ch.holdQos = true
		u.ch.deliver("0", "1", "2", "3")
		served := u.serve(t.Context())
		synctest.Wait()

		// The first three, up to Prefetch, are acked with one basic.ack,
		// and nothing is processed until the Qos after it is answered.
		require.Equal(t, []string{"qos 3", "consume orders humus autoAck=false exclusive=false noLocal=false noWait=false args=map[]",
			"ack 3 multiple", "qos 3"}, u.ch.Calls())
		require.Empty(t, got.get())

		u.ch.answerQos()
		synctest.Wait()
		require.Equal(t, []string{"0", "1", "2"}, got.get(), "processing went on after 1 failed")
		require.Equal(t, "ack 4 multiple", u.ch.Calls()[4])
		require.Len(t, u.ch.Calls(), 6, "the next batch waits for its Qos")

		u.ch.answerQos()
		synctest.Wait()
		require.Equal(t, []string{"0", "1", "2", "3"}, got.get())

		failures := records(u.tel.Logs(), "processing a message failed")
		require.Len(t, failures, 1, "the failure is logged once")
		require.Equal(t, log.SeverityError, failures[0].Severity())
		a := attrs(failures[0])
		require.Equal(t, "1", a["messaging.message.id"])
		require.Equal(t, "orders", a["messaging.destination.name"])
		require.Equal(t, "cannot process", a["exception.message"])
		require.Equal(t, "rabbitmq", a["messaging.system"])

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
		require.Equal(t, []string{cancel, closeChannel, closeConn}, u.ch.Calls()[6:], "everything received was acked")
	})
}

func TestService_at_most_once_processes_the_rest_of_an_acked_batch_on_shutdown(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		g := newGate("0")
		var got seen
		s := settings()
		s.Delivery = queue.AtMostOnce
		s.Prefetch = 3
		u := newUnit(t, s, func(_ context.Context, m rabbitmq.Message) error {
			got.add(m.MessageID)
			g.wait(m.MessageID)
			return nil
		})
		u.ch.deliver("0", "1", "2")
		served := u.serve(t.Context())
		synctest.Wait()
		require.Equal(t, []string{"0"}, got.get())

		shutdown := make(chan error, 1)
		go func() { shutdown <- u.svc.Shutdown(context.Background()) }()
		synctest.Wait()
		require.Equal(t, cancel, u.ch.Calls()[len(u.ch.Calls())-1], "the consumer is cancelled at once")

		g.release("0")
		require.NoError(t, <-shutdown)
		require.NoError(t, <-served)
		require.Equal(t, []string{"0", "1", "2"}, got.get(), "the batch was acked, so all of it is processed")
		require.Equal(t, []string{"qos 3", "consume orders humus autoAck=false exclusive=false noLocal=false noWait=false args=map[]",
			"ack 3 multiple", "qos 3", cancel, closeChannel, closeConn}, u.ch.Calls())
	})
}

func TestService_Concurrency_1_keeps_the_delivery_order(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var got seen
		var mu sync.Mutex
		inFlight, most := 0, 0
		u := newUnit(t, settings(), func(_ context.Context, m rabbitmq.Message) error {
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			mu.Unlock()
			time.Sleep(time.Second)
			got.add(m.MessageID)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		})
		served := u.serve(t.Context())
		ids := []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}
		u.ch.deliver(ids...)
		time.Sleep(time.Minute)

		require.Equal(t, ids, got.get())
		require.Equal(t, 1, most)
		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestService_Concurrency_4_processes_four_deliveries_at_once(t *testing.T) {
	t.Parallel()

	for _, delivery := range []queue.Delivery{queue.AtLeastOnce, queue.AtMostOnce} {
		t.Run(string(delivery), func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				g := newGate("0", "1", "2", "3")
				var got seen
				s := settings()
				s.Delivery = delivery
				s.Concurrency = 4
				u := newUnit(t, s, func(_ context.Context, m rabbitmq.Message) error {
					got.add(m.MessageID)
					g.wait(m.MessageID)
					return nil
				})
				served := u.serve(t.Context())
				u.ch.deliver("0", "1", "2", "3", "4")
				synctest.Wait()

				require.ElementsMatch(t, []string{"0", "1", "2", "3"}, got.get(), "four are processed at once, and the fifth waits")
				for _, id := range []string{"0", "1", "2", "3"} {
					g.release(id)
				}
				synctest.Wait()
				require.ElementsMatch(t, []string{"0", "1", "2", "3", "4"}, got.get())

				require.NoError(t, u.svc.Shutdown(t.Context()))
				require.NoError(t, <-served)
			})
		})
	}
}

func TestService_shutdown_cancels_waits_acks_nacks_the_rest_and_closes_in_order(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		g := newGate("0")
		var got seen
		u := newUnit(t, settings(), func(_ context.Context, m rabbitmq.Message) error {
			got.add(m.MessageID)
			g.wait(m.MessageID)
			return nil
		})
		served := u.serve(t.Context())
		u.ch.deliver("0", "1", "2", "3")
		synctest.Wait()

		shutdown := make(chan error, 1)
		go func() { shutdown <- u.svc.Shutdown(context.Background()) }()
		synctest.Wait()
		// 1. The consumer is cancelled, and 2. nothing more is handed out,
		// while 0 is still in flight.
		require.Equal(t, []string{qos10, consume, cancel}, u.ch.Calls())

		g.release("0")
		require.NoError(t, <-shutdown)
		require.NoError(t, <-served)
		require.Equal(t, []string{"0"}, got.get())
		require.Equal(t, []string{
			qos10, consume,
			cancel,
			"ack 1",                             // 3. the delivery in flight is acked,
			"nack 4 multiple=true requeue=true", // 4. the rest are nacked, in one basic.nack,
			closeChannel, closeConn,             // 5. and the channel and the connection are closed.
		}, u.ch.Calls())
	})
}

func TestService_stops_when_Serves_context_is_done(t *testing.T) {
	t.Parallel()

	type key struct{}
	synctest.Test(t, func(t *testing.T) {
		g := newGate("0")
		ctxs := make(chan context.Context, 1)
		u := newUnit(t, settings(), func(ctx context.Context, m rabbitmq.Message) error {
			ctxs <- ctx
			g.wait(m.MessageID)
			return nil
		})
		ctx, cancelServe := context.WithCancel(context.WithValue(t.Context(), key{}, "value"))
		served := u.serve(ctx)
		u.ch.deliver("0", "1")
		synctest.Wait()

		processCtx := <-ctxs
		cancelServe()
		synctest.Wait()
		require.Equal(t, "value", processCtx.Value(key{}), "Process's context keeps Serve's values")
		require.NoError(t, processCtx.Err(), "Process's context is not cancelled with Serve's")

		g.release("0")
		require.NoError(t, <-served)
		require.Equal(t, []string{qos10, consume, cancel, "ack 1", "nack 2 multiple=true requeue=true", closeChannel, closeConn}, u.ch.Calls())
	})
}

func TestService_Shutdowns_context_cancels_Process_and_closes_the_connection_at_once(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		var cancelled time.Time
		u := newUnit(t, settings(), func(ctx context.Context, _ rabbitmq.Message) error {
			<-ctx.Done()
			cancelled = time.Now()
			return ctx.Err()
		})
		served := u.serve(t.Context())
		u.ch.deliver("0", "1")
		synctest.Wait()

		ctx, cancelShutdown := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancelShutdown()
		start := time.Now()
		err := u.svc.Shutdown(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, 2*time.Second, cancelled.Sub(start))
		require.NoError(t, <-served)
		// Nothing is acked or nacked: the broker requeues what was unacked
		// when the connection closed.
		require.Equal(t, []string{qos10, consume, cancel, closeConn}, u.ch.Calls())

		require.NoError(t, u.svc.Shutdown(t.Context()), "a second Shutdown returns nil")
	})
}

func TestService_Shutdown_before_Serve_makes_Serve_return_nil_at_once(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), func(context.Context, rabbitmq.Message) error { return nil })
		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, u.svc.Serve(t.Context()))
		require.Empty(t, u.ch.Calls(), "Serve never connected")
		require.NoError(t, u.svc.Shutdown(t.Context()))
	})
}

func TestService_a_lost_connection_ends_Serve_with_an_error_naming_the_queue(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		g := newGate("0")
		var got seen
		u := newUnit(t, settings(), func(_ context.Context, m rabbitmq.Message) error {
			got.add(m.MessageID)
			g.wait(m.MessageID)
			return nil
		})
		served := u.serve(t.Context())
		u.ch.deliver("0", "1")
		synctest.Wait()

		forced := &amqp.Error{Code: amqp.ConnectionForced, Reason: "CONNECTION_FORCED - broker forced connection closure", Server: true}
		u.ch.lose(forced)
		synctest.Wait()
		select {
		case err := <-served:
			t.Fatalf("Serve returned before the delivery in flight finished: %v", err)
		default:
		}

		// The delivery in flight finishes, and its ack fails and is
		// dropped: the broker requeued it with the connection.
		g.release("0")
		err := <-served
		require.ErrorIs(t, err, forced)
		require.ErrorContains(t, err, "orders")
		require.Equal(t, []string{"0"}, got.get())
		require.Equal(t, []string{qos10, consume, "connection lost", "ack 1", closeConn}, u.ch.Calls())
	})
}

func TestService_a_consumer_the_broker_cancels_ends_Serve_with_an_error(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), func(context.Context, rabbitmq.Message) error { return nil })
		served := u.serve(t.Context())
		synctest.Wait()

		// The broker cancels the consumer, as when its queue is deleted:
		// the deliveries end, and the channel stays open.
		u.ch.mu.Lock()
		u.ch.endDeliveries()
		u.ch.mu.Unlock()

		err := <-served
		require.ErrorContains(t, err, "lost the connection consuming orders")
		require.Equal(t, []string{qos10, consume, closeChannel, closeConn}, u.ch.Calls())
	})
}

func TestService_logs_consuming_on_each_connection(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), func(context.Context, rabbitmq.Message) error { return nil })
		served := u.serve(t.Context())
		synctest.Wait()

		consuming := records(u.tel.Logs(), "consuming")
		require.Len(t, consuming, 1)
		require.Equal(t, log.SeverityInfo, consuming[0].Severity())
		a := attrs(consuming[0])
		require.Equal(t, "orders", a["messaging.destination.name"])
		require.Equal(t, "rabbit", a["server.address"])
		require.Equal(t, "5672", a["server.port"])
		require.Equal(t, "rabbitmq", a["messaging.system"])

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestService_retries_a_failed_attempt_and_logs_it_at_WARN(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), func(context.Context, rabbitmq.Message) error { return nil })
		var attempts atomic.Int32
		rabbitmq.SetOpen(u.svc, func(ctx context.Context) (rabbitmq.Channel, io.Closer, error) {
			if attempts.Add(1) < 3 {
				return nil, nil, syscall.ECONNREFUSED
			}
			return u.ch.open(ctx)
		})
		served := u.serve(t.Context())
		time.Sleep(time.Minute)

		require.Equal(t, int32(3), attempts.Load())
		require.Equal(t, []string{qos10, consume}, u.ch.Calls())
		warns := records(u.tel.Logs(), "connecting failed")
		require.Len(t, warns, 2)
		for _, r := range warns {
			require.Equal(t, log.SeverityWarn, r.Severity())
			require.Equal(t, syscall.ECONNREFUSED.Error(), attrs(r)["exception.message"])
		}
		require.Len(t, records(u.tel.Logs(), "consuming"), 1)

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestService_stops_on_an_error_a_reconnect_would_not_fix(t *testing.T) {
	t.Parallel()

	for name, refusal := range map[string]error{
		"a wrong password": amqp.ErrCredentials,
		"a missing queue":  &amqp.Error{Code: amqp.NotFound, Reason: "NOT_FOUND - no queue 'orders' in vhost '/'", Server: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				u := newUnit(t, settings(), func(context.Context, rabbitmq.Message) error { return nil })
				var attempts atomic.Int32
				rabbitmq.SetOpen(u.svc, func(context.Context) (rabbitmq.Channel, io.Closer, error) {
					attempts.Add(1)
					return nil, nil, refusal
				})

				err := u.svc.Serve(t.Context())
				require.ErrorIs(t, err, refusal)
				require.Equal(t, int32(1), attempts.Load(), "it was not retried")
				require.Empty(t, records(u.tel.Logs(), "connecting failed"), "an error that ends the Service is returned, not logged")
			})
		})
	}
}

func TestService_a_queue_the_broker_refuses_stops_it_and_closes_the_connection(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), func(context.Context, rabbitmq.Message) error { return nil })
		locked := &amqp.Error{Code: amqp.ResourceLocked, Reason: "RESOURCE_LOCKED - cannot obtain exclusive access to locked queue 'orders'", Server: true}
		rabbitmq.SetOpen(u.svc, func(ctx context.Context) (rabbitmq.Channel, io.Closer, error) {
			ch, conn, err := u.ch.open(ctx)
			return refusingConsume{Channel: ch, err: locked}, conn, err
		})

		err := u.svc.Serve(t.Context())
		require.ErrorIs(t, err, locked)
		require.ErrorContains(t, err, "consuming orders")
		require.Equal(t, []string{qos10, closeConn}, u.ch.Calls())
	})
}

// refusingConsume is a channel whose consume fails with err.
type refusingConsume struct {
	rabbitmq.Channel
	err error
}

func (r refusingConsume) ConsumeWithContext(context.Context, string, string, bool, bool, bool, bool, amqp.Table) (<-chan amqp.Delivery, error) {
	return nil, r.err
}

// standInInit returns an Init whose only Service consumes settings with p, on
// ch.
func standInInit(settings rabbitmq.Settings, ch *standIn, p queue.ProcessorFunc[rabbitmq.Message]) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := rabbitmq.NewService(s, settings, p)
		if err != nil {
			return nil, err
		}
		rabbitmq.SetOpen(svc, ch.open)
		return []humus.Service{svc}, nil
	}
}

func TestService_a_grace_period_that_runs_out_cancels_Process_and_exits_1(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ch := newStandIn()
		ch.deliver("0")
		var cancelled time.Time
		p, status := humustest.Start(t, standInInit(settings(), ch, func(ctx context.Context, _ rabbitmq.Message) error {
			<-ctx.Done()
			cancelled = time.Now()
			return ctx.Err()
		}),
			humustest.WithApp(humus.WithHost(humustest.NewHost(t).Host())),
			humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"}),
		)
		synctest.Wait()
		start := time.Now()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		require.True(t, ok, "the cause is a *humus.GracePeriodError: %v", st.Cause())
		require.Equal(t, 2*time.Second, gerr.GracePeriod)
		require.Equal(t, 2*time.Second, cancelled.Sub(start), "Process's context was cancelled when the grace period ran out")
		require.Equal(t, []string{qos10, consume, cancel, closeConn}, ch.Calls(), "the delivery was neither acked nor rejected")
	})
}

func TestService_waits_for_a_broker_that_is_down_and_returns_nil_on_SIGTERM(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t) // nothing listens at rabbit:5672
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := rabbitmq.NewService(s, settings(), queue.ProcessorFunc[rabbitmq.Message](func(context.Context, rabbitmq.Message) error {
				return nil
			}))
			if err != nil {
				return nil, err
			}
			return []humus.Service{svc}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		time.Sleep(2 * time.Minute)
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		warns := records(tel.Logs(), "connecting failed")
		// Backing off up to 30s, two minutes hold more than a few
		// attempts, and far fewer than one every 100ms.
		require.Greater(t, len(warns), 4)
		require.Less(t, len(warns), 100)
		for _, r := range warns {
			require.Equal(t, log.SeverityWarn, r.Severity())
			require.Contains(t, attrs(r)["exception.message"], "connection refused")
			require.Equal(t, "rabbit", attrs(r)["server.address"])
		}
		require.Empty(t, records(tel.Logs(), "consuming"))
	})
}

func TestService_passes_the_Service_contract_with_no_broker(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		svc, err := rabbitmq.NewService(s, settings(), queue.ProcessorFunc[rabbitmq.Message](func(context.Context, rabbitmq.Message) error {
			return nil
		}))
		require.NoError(t, err)
		return svc
	})
}
