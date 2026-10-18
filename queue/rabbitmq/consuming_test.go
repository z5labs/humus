// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// consuming reports whether m, a Consuming monitor, is healthy, and fails
// the test if it returns an error.
func consuming(t *testing.T, m health.Monitor) bool {
	t.Helper()
	ok, err := m.Healthy(context.Background())
	require.NoError(t, err, "Consuming's monitor never returns an error")
	return ok
}

// nop is a Processor that does nothing.
var nop = queue.ProcessorFunc[rabbitmq.Message](func(context.Context, rabbitmq.Message) error { return nil })

func TestService_Consuming_is_the_same_monitor_each_time(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), nop)
		require.True(t, u.svc.Consuming() == u.svc.Consuming(), "Consuming returns the same monitor each time")
	})
}

func TestService_Consuming_turns_healthy_on_the_consume_and_stays_so_until_Serve_returns(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), nop)
		// Taken before Serve, as the end user's Init does.
		m := u.svc.Consuming()
		require.False(t, consuming(t, m), "unhealthy before Serve")

		u.ch.holdConsumes()
		served := u.serve(t.Context())

		// The monitor is checked from another goroutine, every
		// millisecond, for as long as Serve runs, as a probes server does.
		stopPolling := make(chan struct{})
		polled := make(chan struct{})
		go func() {
			defer close(polled)
			for {
				select {
				case <-stopPolling:
					return
				case <-time.After(time.Millisecond):
					_, _ = m.Healthy(context.Background())
				}
			}
		}()

		synctest.Wait()
		require.Equal(t, []string{qos10, consume}, u.ch.Calls(), "the Service sent its basic.consume")
		require.False(t, consuming(t, m), "unhealthy until the broker answers the consume")
		time.Sleep(time.Minute)
		require.False(t, consuming(t, m), "still unhealthy while the consume goes unanswered")

		u.ch.answerConsume()
		synctest.Wait()
		require.True(t, consuming(t, m), "healthy once the broker answered the consume")

		// The broker closes the connection, as a node that shuts down for
		// an upgrade does, and the Service connects again: twice.
		for i := range 2 {
			u.channel(t, i).lose(forced)
			synctest.Wait()
			require.True(t, consuming(t, m), "healthy through lost connection %d", i+1)
			next := u.channel(t, i+1)
			require.Equal(t, []string{qos10, consume}, next.Calls(), "the Service consumed again after lost connection %d", i+1)
			require.True(t, consuming(t, m), "healthy after reconnect %d", i+1)
		}
		require.Len(t, records(u.tel.Logs(), "lost the connection"), 2)

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
		require.False(t, consuming(t, m), "unhealthy once Serve has returned")
		require.False(t, consuming(t, u.svc.Consuming()), "a monitor taken after Serve is unhealthy too")

		close(stopPolling)
		<-polled
	})
}

func TestService_Consuming_stays_healthy_while_it_connects_again(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), nop)
		// The first connection is the stand-in, and every attempt after it
		// fails, as while the broker is down.
		var opened atomic.Int32
		rabbitmq.SetOpen(u.svc, func(ctx context.Context) (rabbitmq.Channel, io.Closer, error) {
			if opened.Add(1) == 1 {
				return u.ch.open(ctx)
			}
			return nil, nil, errors.New("dial tcp rabbit:5672: connect: connection refused")
		})
		served := u.serve(t.Context())
		synctest.Wait()
		require.True(t, consuming(t, u.svc.Consuming()))

		u.ch.lose(forced)
		time.Sleep(2 * time.Minute)
		require.Greater(t, opened.Load(), int32(2), "the Service kept trying to connect")
		require.NotEmpty(t, records(u.tel.Logs(), "connecting failed"))
		require.True(t, consuming(t, u.svc.Consuming()), "a broker outage after consuming doesn't make it unhealthy")

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
		require.False(t, consuming(t, u.svc.Consuming()), "unhealthy once Serve has returned")
	})
}

func TestService_Consuming_stays_unhealthy_while_each_consume_fails(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), nop)
		// Every consume fails with an error a later attempt may fix, so
		// Serve keeps running, and the monitor is checked while it does.
		var attempts atomic.Int32
		rabbitmq.SetOpen(u.svc, func(ctx context.Context) (rabbitmq.Channel, io.Closer, error) {
			attempts.Add(1)
			ch, conn, err := newStandIn().open(ctx)
			return refusingConsume{Channel: ch, err: forced}, conn, err
		})
		served := u.serve(t.Context())

		time.Sleep(2 * time.Minute)
		require.Greater(t, attempts.Load(), int32(2), "the Service kept trying to consume")
		require.NotEmpty(t, records(u.tel.Logs(), "connecting failed"))
		require.False(t, consuming(t, u.svc.Consuming()), "a consume that failed doesn't make it healthy")

		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestService_Consuming_stays_unhealthy_when_Serve_ends_on_a_refused_queue(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), nop)
		notFound := &amqp.Error{Code: amqp.NotFound, Reason: "NOT_FOUND - no queue 'orders' in vhost '/'", Server: true}
		rabbitmq.SetOpen(u.svc, func(ctx context.Context) (rabbitmq.Channel, io.Closer, error) {
			ch, conn, err := u.ch.open(ctx)
			return refusingConsume{Channel: ch, err: notFound}, conn, err
		})
		require.ErrorIs(t, u.svc.Serve(t.Context()), notFound, "a queue the broker refuses ends Serve")
		require.False(t, consuming(t, u.svc.Consuming()), "unhealthy once Serve has returned")
	})
}

func TestService_Shutdown_while_the_consume_is_unanswered_returns_and_it_never_turns_healthy(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		u := newUnit(t, settings(), nop)
		u.ch.holdConsumes()
		served := u.serve(t.Context())
		synctest.Wait()
		require.Equal(t, []string{qos10, consume}, u.ch.Calls(), "the Service sent its basic.consume")
		require.False(t, consuming(t, u.svc.Consuming()), "unhealthy while the consume is unanswered")

		// A pod terminated before it ever turned ready: Shutdown closes the
		// connection, which ends the unanswered consume.
		require.NoError(t, u.svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
		require.Equal(t, []string{qos10, consume, closeConn}, u.ch.Calls(), "the connection was closed")
		require.False(t, consuming(t, u.svc.Consuming()))
	})
}

func TestService_Consuming_stays_unhealthy_with_nothing_listening(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t) // nothing listens at rabbit:5672
		tel := humustest.NewTelemetry()
		svcs := make(chan *rabbitmq.Service, 1)
		p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := rabbitmq.NewService(s, settings(), nop)
			if err != nil {
				return nil, err
			}
			svcs <- svc
			return []humus.Service{svc}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		svc := <-svcs
		m := svc.Consuming()

		failed := 0
		for range 4 {
			time.Sleep(30 * time.Second)
			select {
			case st := <-status:
				t.Fatalf("the App exited while the broker was unreachable: %v", st.Cause())
			default:
			}
			attempts := len(records(tel.Logs(), "connecting failed"))
			require.Greater(t, attempts, failed, "the Service is still trying to connect")
			failed = attempts
			require.False(t, consuming(t, m), "never consumed, so unhealthy while the Service retries")
		}

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, records(tel.Logs(), "consuming"), "it never consumed")
	})
}

// probedConsumer returns an Init for a consumer's pod, which has no server
// of its own: it consumes settings on ch, and serves its probes on
// probeSettings with httphealth's probes server, whose readiness waits for
// the consumer to consume.
func probedConsumer(settings rabbitmq.Settings, ch *standIn, probeSettings httphealth.Settings) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		consumer, err := rabbitmq.NewService(s, settings, nop)
		if err != nil {
			return nil, err
		}
		rabbitmq.SetOpen(consumer, ch.open)
		probes, err := httphealth.NewService(s, probeSettings, health.Readiness(consumer.Consuming()))
		if err != nil {
			return nil, err
		}
		return []humus.Service{consumer, probes}, nil
	}
}

// probe GETs url with h's HTTP client, trusting only ca, and returns the
// status code.
func probe(t *testing.T, h *humustest.Host, ca *humustest.CA, url string) int {
	t.Helper()
	c := h.HTTPClient()
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	resp, err := c.Get(url)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

func TestInit_a_consumers_probes_server_is_ready_once_it_consumes(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const readiness = "https://localhost:8081/health/readiness"
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		ch := newStandIn()
		ch.holdConsumes()
		probeSettings := httphealth.Settings{
			Addr: ":8081",
			TLS:  &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}},
		}

		p, status := humustest.Start(t, probedConsumer(settings(), ch, probeSettings),
			humustest.WithApp(humus.WithHost(h.Host())),
		)
		<-h.Listening("localhost:8081")
		synctest.Wait()
		require.Equal(t, []string{qos10, consume}, ch.Calls(), "the consumer sent its basic.consume")
		require.Equal(t, http.StatusServiceUnavailable, probe(t, h, ca, readiness), "ready before the consumer consumes")
		require.Equal(t, http.StatusOK, probe(t, h, ca, "https://localhost:8081/health/liveness"))

		ch.answerConsume()
		synctest.Wait()
		require.Equal(t, http.StatusOK, probe(t, h, ca, readiness), "not ready once the consumer consumes")

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}
