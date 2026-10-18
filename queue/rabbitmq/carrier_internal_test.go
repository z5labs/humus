// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

func TestHeaderCarrier(t *testing.T) {
	t.Parallel()

	t.Run("Get returns a string header's value", func(t *testing.T) {
		t.Parallel()
		c := headerCarrier{"traceparent": "00-trace", "a": "1"}
		require.Equal(t, "00-trace", c.Get("traceparent"))
		require.Equal(t, "1", c.Get("a"))
		require.Empty(t, c.Get("missing"))
	})

	t.Run("Get ignores a header that is not a string", func(t *testing.T) {
		t.Parallel()
		c := headerCarrier{
			"bytes":  []byte("00-trace"),
			"number": int32(1),
			"table":  amqp.Table{"traceparent": "00-trace"},
			"nil":    nil,
		}
		for k := range c {
			require.Empty(t, c.Get(k), k)
		}
		require.Empty(t, c.Keys(), "Keys names only string headers")
	})

	t.Run("Keys returns the string headers' keys, sorted", func(t *testing.T) {
		t.Parallel()
		c := headerCarrier{"tracestate": "s", "baggage": "b", "traceparent": "p", "x-delivery-count": int64(2)}
		require.Equal(t, []string{"baggage", "traceparent", "tracestate"}, c.Keys())
	})

	t.Run("Get and Keys on no headers return nothing", func(t *testing.T) {
		t.Parallel()
		var c headerCarrier
		require.Empty(t, c.Get("traceparent"))
		require.Empty(t, c.Keys())
	})

	t.Run("Set sets a header in a message's headers, replacing what it held", func(t *testing.T) {
		t.Parallel()
		p := amqp.Publishing{Headers: amqp.Table{"traceparent": []byte("old"), "a": "1"}}
		c := (*headerCarrier)(&p.Headers)
		c.Set("traceparent", "new")
		c.Set("baggage", "k=v")
		require.Equal(t, amqp.Table{"traceparent": "new", "baggage": "k=v", "a": "1"}, p.Headers)
	})

	t.Run("Set makes the headers when there are none", func(t *testing.T) {
		t.Parallel()
		var p amqp.Publishing
		(*headerCarrier)(&p.Headers).Set("traceparent", "new")
		require.Equal(t, amqp.Table{"traceparent": "new"}, p.Headers)
	})
}

func TestDestinationName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                        string
		exchange, routingKey, queue string
		want                        string
	}{
		{name: "the default exchange", routingKey: "orders", queue: "orders", want: "orders"},
		{name: "a named exchange with a routing key", exchange: "shop", routingKey: "order.created", queue: "orders", want: "shop:order.created:orders"},
		{name: "a routing key equal to the queue's name", exchange: "shop", routingKey: "orders", queue: "orders", want: "shop:orders"},
		{name: "a named exchange with no routing key", exchange: "fanout", queue: "orders", want: "fanout:orders"},
		{name: "the default exchange with another routing key", routingKey: "other", queue: "orders", want: "other:orders"},
		{name: "nothing but the queue", queue: "orders", want: "orders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, destinationName(tc.exchange, tc.routingKey, tc.queue))
		})
	}
}

func TestDeliveryTag(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1, deliveryTag(1))
	require.Equal(t, int(^uint(0)>>1), deliveryTag(^uint64(0)), "a tag past math.MaxInt is math.MaxInt")
}
