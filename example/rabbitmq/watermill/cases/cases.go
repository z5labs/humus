// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the invoicer's black-box cases, which the Watermill service the Watermill migration guide starts from and the humus service it ends with both pass.
package cases

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

const headerCorrelationID = "correlation_id"

type Target struct {
	Addr     string
	Vhost    string
	Username string
	Password string

	OrdersExchange     string
	InvoicesExchange   string
	DeadLetterExchange string

	Timeout time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "AnOrder", Run: AnOrder},
	{Name: "NotOrders", Run: NotOrders},
	{Name: "Orders", Run: Orders},
}

func AnOrder(t testing.TB, target Target) {
	ctx, b := begin(t, target)
	o := order(`{"id":"order-1","customer":"ada","total":4250}`)

	b.publish(ctx, t, o)
	got := b.settle(ctx, t, o)

	require.Len(t, got.invoices, 1)
	requireInvoice(t, got.invoices[0], o, `{"order_id":"order-1","customer":"ada","amount":4250}`)
	require.Empty(t, got.deadLetters)
}

func NotOrders(t testing.TB, target Target) {
	ctx, b := begin(t, target)
	notJSON := order("not JSON")
	noID := order(`{"customer":"ada","total":100}`)
	valid := order(`{"id":"order-4","customer":"grace","total":990}`)

	b.publish(ctx, t, notJSON, noID, valid)
	got := b.settle(ctx, t, notJSON, noID, valid)

	require.Len(t, got.deadLetters, 2)
	require.Equal(t, "not JSON", string(got.deadLetters[0].Body))
	require.Equal(t, notJSON.correlationID, header(got.deadLetters[0], headerCorrelationID))
	require.Equal(t, `{"customer":"ada","total":100}`, string(got.deadLetters[1].Body))
	require.Equal(t, noID.correlationID, header(got.deadLetters[1], headerCorrelationID))
	require.Len(t, got.invoices, 1)
	requireInvoice(t, got.invoices[0], valid, `{"order_id":"order-4","customer":"grace","amount":990}`)
}

func Orders(t testing.TB, target Target) {
	ctx, b := begin(t, target)
	var orders []placed
	for n := range 5 {
		orders = append(orders, order(fmt.Sprintf(`{"id":"order-%d","customer":"ada","total":%d}`, 10+n, 100*(n+1))))
	}

	b.publish(ctx, t, orders...)
	got := b.settle(ctx, t, orders...)

	require.Len(t, got.invoices, len(orders))
	for n, o := range orders {
		requireInvoice(t, got.invoices[n], o, fmt.Sprintf(`{"order_id":"order-%d","customer":"ada","amount":%d}`, 10+n, 100*(n+1)))
	}
	require.Empty(t, got.deadLetters)
}

type placed struct {
	correlationID string
	body          string
}

func order(body string) placed {
	return placed{correlationID: rand.Text(), body: body}
}

func requireInvoice(t testing.TB, d amqp.Delivery, o placed, want string) {
	t.Helper()
	require.JSONEq(t, want, string(d.Body))
	require.Equal(t, o.correlationID, header(d, headerCorrelationID))
}

func header(d amqp.Delivery, key string) string {
	v, _ := d.Headers[key].(string)
	return v
}

type broker struct {
	target     Target
	publishing *amqp.Channel
	returns    <-chan amqp.Return
	invoicesIn <-chan amqp.Delivery
	deadIn     <-chan amqp.Delivery
	invoices   []amqp.Delivery
	dead       []amqp.Delivery
}

func begin(t testing.TB, target Target) (context.Context, *broker) {
	t.Helper()
	require.NotEmpty(t, target.Addr, "Target.Addr")
	require.NotEmpty(t, target.Vhost, "Target.Vhost")
	require.NotEmpty(t, target.Username, "Target.Username")
	require.NotEmpty(t, target.OrdersExchange, "Target.OrdersExchange")
	require.NotEmpty(t, target.InvoicesExchange, "Target.InvoicesExchange")
	require.NotEmpty(t, target.DeadLetterExchange, "Target.DeadLetterExchange")
	require.Positive(t, target.Timeout, "Target.Timeout")

	ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
	t.Cleanup(cancel)

	conn, err := amqp.DialConfig("amqp://"+target.Addr, amqp.Config{
		SASL:  []amqp.Authentication{&amqp.PlainAuth{Username: target.Username, Password: target.Password}},
		Vhost: target.Vhost,
	})
	require.NoError(t, err, "connecting to the broker at %s", target.Addr)
	t.Cleanup(func() { _ = conn.Close() })

	watching, err := conn.Channel()
	require.NoError(t, err)
	b := &broker{
		target:     target,
		invoicesIn: watch(t, watching, target.InvoicesExchange),
		deadIn:     watch(t, watching, target.DeadLetterExchange),
	}

	b.publishing, err = conn.Channel()
	require.NoError(t, err)
	require.NoError(t, b.publishing.Confirm(false))
	b.returns = b.publishing.NotifyReturn(make(chan amqp.Return, 1))
	return ctx, b
}

func watch(t testing.TB, ch *amqp.Channel, exchange string) <-chan amqp.Delivery {
	t.Helper()
	require.NoError(t, ch.ExchangeDeclare(exchange, amqp.ExchangeFanout, true, false, false, false, nil), "declaring %s", exchange)
	q, err := ch.QueueDeclare("", false, false, true, false, nil)
	require.NoError(t, err)
	require.NoError(t, ch.QueueBind(q.Name, "", exchange, false, nil), "binding a queue to %s", exchange)
	deliveries, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	require.NoError(t, err)
	return deliveries
}

func (b *broker) publish(ctx context.Context, t testing.TB, orders ...placed) {
	t.Helper()
	for _, o := range orders {
		confirm, err := b.publishing.PublishWithDeferredConfirmWithContext(ctx, b.target.OrdersExchange, "", true, false, amqp.Publishing{
			Headers:      amqp.Table{headerCorrelationID: o.correlationID},
			DeliveryMode: amqp.Persistent,
			Body:         []byte(o.body),
		})
		require.NoError(t, err, "publishing to %s", b.target.OrdersExchange)
		acked, err := confirm.WaitContext(ctx)
		require.NoError(t, err)
		require.True(t, acked, "the broker nacked the order %s", o.correlationID)
		select {
		case ret := <-b.returns:
			require.FailNowf(t, "unroutable", "the broker returned the order from %s: %s", ret.Exchange, ret.ReplyText)
		default:
		}
	}
}

type results struct {
	invoices    []amqp.Delivery
	deadLetters []amqp.Delivery
}

func (b *broker) settle(ctx context.Context, t testing.TB, orders ...placed) results {
	t.Helper()
	for {
		got := b.of(orders)
		pending := len(orders) - len(got.invoices) - len(got.deadLetters)
		if pending <= 0 {
			return got
		}
		select {
		case d, ok := <-b.invoicesIn:
			require.True(t, ok, "the consumer of %s was cancelled", b.target.InvoicesExchange)
			b.invoices = append(b.invoices, d)
		case d, ok := <-b.deadIn:
			require.True(t, ok, "the consumer of %s was cancelled", b.target.DeadLetterExchange)
			b.dead = append(b.dead, d)
		case <-ctx.Done():
			require.FailNowf(t, "timed out", "waiting for the outcomes of %d orders: %v", pending, context.Cause(ctx))
		}
	}
}

func (b *broker) of(orders []placed) results {
	ids := map[string]bool{}
	for _, o := range orders {
		ids[o.correlationID] = true
	}
	var got results
	for _, d := range b.invoices {
		if ids[header(d, headerCorrelationID)] {
			got.invoices = append(got.invoices, d)
		}
	}
	for _, d := range b.dead {
		if ids[header(d, headerCorrelationID)] {
			got.deadLetters = append(got.deadLetters, d)
		}
	}
	return got
}
