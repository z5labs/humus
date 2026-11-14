// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/queue/rabbitmq"
)

type fakePublisher struct {
	published []rabbitmq.Publishing
	refuse    map[string]error
}

func (f *fakePublisher) Publish(_ context.Context, p rabbitmq.Publishing) error {
	if err := f.refuse[p.Exchange]; err != nil {
		return err
	}
	f.published = append(f.published, p)
	return nil
}

func anOrder(headers map[string]any) rabbitmq.Message {
	return rabbitmq.Message{
		Queue:   ordersQueue,
		Headers: headers,
		Body:    []byte(`{"id":"order-1","customer":"ada","total":4250}`),
	}
}

func TestInvoicer_publishes_the_invoice_with_the_orders_correlation_ID(t *testing.T) {
	pub := &fakePublisher{}
	i := &invoicer{publisher: pub, attempts: 3}

	require.NoError(t, i.Process(t.Context(), anOrder(map[string]any{"correlation_id": "corr-1", "source": "shop"})))

	require.Len(t, pub.published, 1)
	require.Equal(t, invoicesExchange, pub.published[0].Exchange)
	require.Empty(t, pub.published[0].RoutingKey)
	require.Equal(t, map[string]any{"correlation_id": "corr-1"}, pub.published[0].Headers)
	require.JSONEq(t, `{"order_id":"order-1","customer":"ada","amount":4250}`, string(pub.published[0].Body))
}

func TestInvoicer_publishes_an_invoice_with_no_headers_for_an_order_with_no_correlation_ID(t *testing.T) {
	pub := &fakePublisher{}
	i := &invoicer{publisher: pub, attempts: 3}

	require.NoError(t, i.Process(t.Context(), anOrder(nil)))

	require.Len(t, pub.published, 1)
	require.Nil(t, pub.published[0].Headers)
}

func TestInvoicer_dead_letters_an_order_that_is_not_one_at_once(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON": "not JSON",
		"no id":    `{"customer":"ada","total":100}`,
	} {
		t.Run(name, func(t *testing.T) {
			pub := &fakePublisher{}
			i := &invoicer{publisher: pub, attempts: 3}
			m := anOrder(map[string]any{"correlation_id": "corr-2", "x-death": []any{amqp.Table{"count": int64(1)}}})
			m.Body = []byte(body)

			require.NoError(t, i.Process(t.Context(), m))

			require.Len(t, pub.published, 1)
			dead := pub.published[0]
			require.Equal(t, deadLetterExchange, dead.Exchange)
			require.Equal(t, body, string(dead.Body))
			require.Equal(t, "corr-2", dead.Headers["correlation_id"])
			require.NotEmpty(t, dead.Headers[headerDeadError])
			require.NotContains(t, dead.Headers, headerDeadAttempts)
			require.NotContains(t, dead.Headers, "x-death")
		})
	}
}

func TestInvoicer_parks_an_order_whose_invoice_failed_one_attempt_higher(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]any
		want    string
	}{
		{"its first attempt", map[string]any{"correlation_id": "corr-3"}, "1"},
		{"a parked order", map[string]any{"correlation_id": "corr-3", headerAttempt: "1", headerError: "before", "x-death": []any{}, "x-first-death-reason": "expired"}, "2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refused := errors.New("refused")
			pub := &fakePublisher{refuse: map[string]error{invoicesExchange: refused}}
			i := &invoicer{publisher: pub, attempts: 3}

			require.NoError(t, i.Process(t.Context(), anOrder(c.headers)))

			require.Len(t, pub.published, 1)
			parked := pub.published[0]
			require.Equal(t, retryExchange, parked.Exchange)
			require.Equal(t, `{"id":"order-1","customer":"ada","total":4250}`, string(parked.Body))
			require.Equal(t, map[string]any{
				"correlation_id": "corr-3",
				headerAttempt:    c.want,
				headerError:      "refused",
			}, parked.Headers)
		})
	}
}

func TestInvoicer_dead_letters_an_order_whose_invoice_failed_on_its_last_attempt(t *testing.T) {
	pub := &fakePublisher{refuse: map[string]error{invoicesExchange: errors.New("refused")}}
	i := &invoicer{publisher: pub, attempts: 3}

	require.NoError(t, i.Process(t.Context(), anOrder(map[string]any{"correlation_id": "corr-4", headerAttempt: "2", headerError: "refused"})))

	require.Len(t, pub.published, 1)
	require.Equal(t, rabbitmq.Publishing{
		Exchange: deadLetterExchange,
		Headers: map[string]any{
			"correlation_id":   "corr-4",
			headerDeadError:    "refused",
			headerDeadAttempts: "3",
		},
		Body: []byte(`{"id":"order-1","customer":"ada","total":4250}`),
	}, pub.published[0])
}

func TestInvoicer_dead_letters_an_order_whose_attempt_is_not_one_at_once(t *testing.T) {
	for name, v := range map[string]any{
		"not a number": "first",
		"zero":         "0",
		"not a string": int64(1),
	} {
		t.Run(name, func(t *testing.T) {
			pub := &fakePublisher{}
			i := &invoicer{publisher: pub, attempts: 3}

			require.NoError(t, i.Process(t.Context(), anOrder(map[string]any{headerAttempt: v})))

			require.Len(t, pub.published, 1)
			dead := pub.published[0]
			require.Equal(t, deadLetterExchange, dead.Exchange)
			require.Contains(t, dead.Headers[headerDeadError], "is not an attempt")
			require.NotContains(t, dead.Headers, headerAttempt)
			require.NotContains(t, dead.Headers, headerDeadAttempts)
		})
	}
}

func TestInvoicer_returns_the_error_when_the_order_can_be_neither_parked_nor_dead_lettered(t *testing.T) {
	stored := errors.New("not stored")
	cases := []struct {
		name   string
		m      rabbitmq.Message
		refuse map[string]error
		want   string
	}{
		{"parking", anOrder(nil), map[string]error{invoicesExchange: errors.New("refused"), retryExchange: stored}, "parking the order in " + retryExchange},
		{"dead-lettering an order that is not one", rabbitmq.Message{Body: []byte("not JSON")}, map[string]error{deadLetterExchange: stored}, "dead-lettering the order to " + deadLetterExchange},
		{"dead-lettering on the last attempt", anOrder(map[string]any{headerAttempt: "2"}), map[string]error{invoicesExchange: errors.New("refused"), deadLetterExchange: stored}, "dead-lettering the order to " + deadLetterExchange},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pub := &fakePublisher{refuse: c.refuse}
			i := &invoicer{publisher: pub, attempts: 3}

			err := i.Process(t.Context(), c.m)

			require.ErrorIs(t, err, stored)
			require.ErrorContains(t, err, c.want)
			require.Empty(t, pub.published)
		})
	}
}

func TestInvoicer_returns_the_invoices_error_once_its_context_is_done(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pub := &fakePublisher{refuse: map[string]error{invoicesExchange: context.Canceled}}
	i := &invoicer{publisher: pub, attempts: 3}

	require.ErrorIs(t, i.Process(ctx, anOrder(nil)), context.Canceled)
	require.Empty(t, pub.published)
}
