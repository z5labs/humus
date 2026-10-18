// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

func TestStops(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		err   error
		stops bool
	}{
		{name: "a wrong login", err: amqp.ErrCredentials, stops: true},
		{name: "no shared SASL mechanism", err: amqp.ErrSASL, stops: true},
		{name: "a vhost the login can't open", err: amqp.ErrVhost, stops: true},
		{name: "ACCESS_REFUSED from the broker", err: &amqp.Error{Code: amqp.AccessRefused, Reason: "ACCESS_REFUSED - Login was refused", Server: true}, stops: true},
		{name: "a queue that does not exist", err: &amqp.Error{Code: amqp.NotFound, Reason: "NOT_FOUND - no queue 'orders' in vhost '/'", Server: true}, stops: true},
		{name: "a queue the login can't read", err: &amqp.Error{Code: amqp.AccessRefused, Reason: "ACCESS_REFUSED - access to queue 'orders' in vhost '/' refused", Server: true}, stops: true},
		{name: "a queue another connection consumes exclusively", err: &amqp.Error{Code: amqp.ResourceLocked, Reason: "RESOURCE_LOCKED - cannot obtain exclusive access to locked queue 'orders'", Server: true}, stops: true},
		{name: "a refusal, wrapped", err: fmt.Errorf("rabbitmq: consuming orders: %w", &amqp.Error{Code: amqp.NotFound, Server: true}), stops: true},
		{name: "a refused dial", err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}},
		{name: "a timeout", err: fmt.Errorf("rabbitmq: AMQP handshake with rabbit:5672: %w", context.DeadlineExceeded)},
		{name: "a timeout that amqp091 read as a wrong login", err: fmt.Errorf("rabbitmq: AMQP handshake with rabbit:5672: %w (%v)", context.DeadlineExceeded, amqp.ErrCredentials)},
		{name: "a connection the broker forced closed", err: &amqp.Error{Code: amqp.ConnectionForced, Reason: "CONNECTION_FORCED - broker forced connection closure with reason 'shutdown'", Server: true}},
		{name: "a closed channel", err: amqp.ErrClosed},
		{name: "a precondition failure", err: &amqp.Error{Code: amqp.PreconditionFailed, Server: true}},
		{name: "any other error", err: errors.New("EOF")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.stops, stops(tc.err))
		})
	}
}

func TestStopsConsuming(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		err   error
		stops bool
	}{
		{name: "a delivery held past consumer_timeout", err: &amqp.Error{Code: amqp.PreconditionFailed, Reason: "PRECONDITION_FAILED - delivery acknowledgement on channel 1 timed out", Server: true}, stops: true},
		{name: "a refusal of the queue", err: &amqp.Error{Code: amqp.NotFound, Reason: "NOT_FOUND - no queue 'orders' in vhost '/'", Server: true}, stops: true},
		{name: "a refusal of the login", err: &amqp.Error{Code: amqp.AccessRefused, Server: true}, stops: true},
		{name: "a connection the broker forced closed", err: &amqp.Error{Code: amqp.ConnectionForced, Reason: "CONNECTION_FORCED - broker forced connection closure with reason 'shutdown'", Server: true}},
		{name: "a closed channel", err: amqp.ErrClosed},
		{name: "a consumer the broker cancelled", err: errLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.stops, stopsConsuming(tc.err))
		})
	}
}

func TestNewMessage(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	d := amqp.Delivery{
		Exchange:        "orders-exchange",
		RoutingKey:      "orders.created",
		Redelivered:     true,
		Headers:         amqp.Table{"tenant": "acme", "attempt": int32(2), "nested": amqp.Table{"a": "b"}},
		ContentType:     "application/json",
		ContentEncoding: "gzip",
		DeliveryMode:    amqp.Persistent,
		Priority:        5,
		CorrelationId:   "correlation-1",
		ReplyTo:         "replies",
		Expiration:      "60000",
		MessageId:       "message-1",
		Timestamp:       ts,
		Type:            "order.created",
		UserId:          "publisher",
		AppId:           "orders-api",
		Body:            []byte(`{"id":1}`),
		DeliveryTag:     7,
	}
	require.Equal(t, Message{
		Queue:           "orders",
		Exchange:        "orders-exchange",
		RoutingKey:      "orders.created",
		Redelivered:     true,
		Headers:         map[string]any{"tenant": "acme", "attempt": int32(2), "nested": amqp.Table{"a": "b"}},
		ContentType:     "application/json",
		ContentEncoding: "gzip",
		Persistent:      true,
		Priority:        5,
		CorrelationID:   "correlation-1",
		ReplyTo:         "replies",
		Expiration:      "60000",
		MessageID:       "message-1",
		Timestamp:       ts,
		Type:            "order.created",
		UserID:          "publisher",
		AppID:           "orders-api",
		Body:            []byte(`{"id":1}`),
	}, newMessage("orders", d))

	for name, mode := range map[string]uint8{"transient": amqp.Transient, "unset": 0} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			d := amqp.Delivery{DeliveryMode: mode}
			m := newMessage("orders", d)
			require.False(t, m.Persistent)
			require.Nil(t, m.Headers)
			require.Equal(t, "orders", m.Queue)
		})
	}
}
