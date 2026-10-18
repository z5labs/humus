// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/rand"
	"testing"
	"testing/synctest"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

// noop is a Processor that does nothing.
var noop = queue.ProcessorFunc[rabbitmq.Message](func(context.Context, rabbitmq.Message) error { return nil })

// setup is a Setup with no telemetry, on a new humustest Host.
func setup(t *testing.T) *humus.Setup {
	t.Helper()
	return &humus.Setup{Host: humustest.NewHost(t).Host(), Telemetry: telemetry.Noop()}
}

func TestNewService_accepts_DefaultSettings_with_Addr_SASL_and_Queue_set(t *testing.T) {
	t.Parallel()

	d := rabbitmq.DefaultSettings()
	d.Connection.Addr = "rabbit:5672"
	d.Connection.SASL = &amqp.PlainAuth{Username: "orders", Password: rand.Text()}
	d.Queue = "orders"
	svc, err := rabbitmq.NewService(setup(t), d, noop)
	require.NoError(t, err)
	require.NotNil(t, svc)
}

func TestNewService_reports_every_bad_field_in_one_error(t *testing.T) {
	t.Parallel()

	s := settings()
	s.Queue = ""
	s.Prefetch = 0
	svc, err := rabbitmq.NewService(setup(t), s, noop)
	require.Nil(t, svc)
	require.EqualError(t, err, "rabbitmq: NewService: Settings.Queue must not be empty\n"+
		"Settings.Prefetch must be from 1 to 65535")
}

func TestNewService_names_every_field_of_DefaultSettings_that_has_no_default(t *testing.T) {
	t.Parallel()

	_, err := rabbitmq.NewService(setup(t), rabbitmq.DefaultSettings(), noop)
	require.EqualError(t, err, "rabbitmq: NewService: Settings.Connection.Addr is not host:port\n"+
		"Settings.Connection.SASL must be set\n"+
		"Settings.Queue must not be empty")
}

func TestNewService_checks_every_field(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		field  string
		change func(*rabbitmq.Settings)
	}{
		{field: "Settings.Connection.Addr", change: func(s *rabbitmq.Settings) { s.Connection.Addr = "amqp://rabbit" }},
		{field: "Settings.Connection.Vhost", change: func(s *rabbitmq.Settings) { s.Connection.Vhost = "" }},
		{field: "Settings.Connection.SASL", change: func(s *rabbitmq.Settings) { s.Connection.SASL = (*amqp.PlainAuth)(nil) }},
		{field: "Settings.Connection.Heartbeat", change: func(s *rabbitmq.Settings) { s.Connection.Heartbeat = 0 }},
		{field: "Settings.Connection.ConnectTimeout", change: func(s *rabbitmq.Settings) { s.Connection.ConnectTimeout = -1 }},
		{field: "Settings.Queue", change: func(s *rabbitmq.Settings) { s.Queue = "" }},
		{field: "Settings.Delivery", change: func(s *rabbitmq.Settings) { s.Delivery = "exactly-once" }},
		{field: "Settings.Prefetch", change: func(s *rabbitmq.Settings) { s.Prefetch = 65536 }},
		{field: "Settings.Concurrency", change: func(s *rabbitmq.Settings) { s.Concurrency = 0 }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			s := settings()
			tc.change(&s)
			_, err := rabbitmq.NewService(setup(t), s, noop)
			require.ErrorContains(t, err, "rabbitmq: NewService: "+tc.field+" ")
		})
	}
}

func TestNewService_rejects_a_Concurrency_above_Prefetch_naming_both_fields(t *testing.T) {
	t.Parallel()

	s := settings()
	s.Prefetch = 4
	s.Concurrency = 5
	_, err := rabbitmq.NewService(setup(t), s, noop)
	require.EqualError(t, err, "rabbitmq: NewService: Settings.Concurrency, 5, must be at most Settings.Prefetch, 4")

	s.Concurrency = 4
	_, err = rabbitmq.NewService(setup(t), s, noop)
	require.NoError(t, err, "Concurrency may equal Prefetch")
}

func TestNewService_rejects_a_nil_Processor(t *testing.T) {
	t.Parallel()

	_, err := rabbitmq.NewService(setup(t), settings(), nil)
	require.EqualError(t, err, "rabbitmq: NewService: the Processor must not be nil")
}

func TestNewService_opens_no_connection(t *testing.T) {
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

		// An Init that fails after NewService: the App runs no Service, and
		// no OnStop function.
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			if _, err := rabbitmq.NewService(s, settings(), noop); err != nil {
				return nil, err
			}
			return nil, context.Canceled
		}, humustest.WithApp(humus.WithHost(h.Host())))
		st := <-status
		require.ErrorIs(t, st.Cause(), context.Canceled)
		synctest.Wait()
		select {
		case <-accepted:
			t.Fatal("NewService connected to the broker")
		default:
		}
	})
}
