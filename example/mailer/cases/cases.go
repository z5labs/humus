// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the mailer's black-box cases, run against a deployment of it.
package cases

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

const (
	requestExchange = "mail"
	eventsExchange  = "mail.events"
	deadExchange    = "mail.dead"

	sendKey    = "mail.send"
	sentKey    = "mail.sent"
	refusedKey = "mail.refused"

	deadReasonHeader = "x-mail-dead-reason"

	malformed = "malformed"
)

type Target struct {
	Addr     string
	Vhost    string
	TLS      *tls.Config
	Username string
	Password string

	WireMock *url.URL
	Client   *http.Client

	ProviderPoint    string
	ScenarioKey      string
	CloseConnections func(ctx context.Context) error
	Timeout          time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var Parallel = []Case{
	{Name: "Sent", Run: Sent},
	{Name: "SentOnce", Run: SentOnce},
	{Name: "Refused", Run: Refused},
	{Name: "Malformed", Run: Malformed},
}

var Alone = []Case{}

func Sent(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(ctx, t, target)
	r := newRequest("example.com")

	b.publish(ctx, t, r)
	event := b.outcome(ctx, t, r.messageID)

	require.Equal(t, sentKey, event.RoutingKey)
	require.Equal(t, r.messageID, event.MessageId)
	require.Equal(t, r.correlationID, event.CorrelationId)
	require.NotEmpty(t, providerID(t, event))

	sent := journal(ctx, t, target, r.to)
	require.Len(t, sent, 1)
	require.Equal(t, r.messageID, sent[0].header("Idempotency-Key"))
	require.Regexp(t, `^Bearer \S+$`, sent[0].header("Authorization"))
}

func SentOnce(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(ctx, t, target)
	r := newRequest("example.com")

	b.publish(ctx, t, r)
	b.publish(ctx, t, r)
	events := b.events(ctx, t, r.messageID, 2)

	id := providerID(t, events[0])
	require.NotEmpty(t, id)
	for _, event := range events {
		require.Equal(t, sentKey, event.RoutingKey)
		require.Equal(t, id, providerID(t, event))
	}

	sent := journal(ctx, t, target, r.to)
	require.NotEmpty(t, sent)
	for _, s := range sent {
		require.Equal(t, r.messageID, s.header("Idempotency-Key"))
	}
}

func Refused(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(ctx, t, target)
	r := newRequest("example.invalid")

	b.publish(ctx, t, r)
	event := b.outcome(ctx, t, r.messageID)

	require.Equal(t, refusedKey, event.RoutingKey)
	require.Equal(t, r.messageID, event.MessageId)
	require.Equal(t, r.correlationID, event.CorrelationId)
	var body struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(event.Body, &body))
	require.NotEmpty(t, body.Reason)
	require.Empty(t, b.deadLetters(r.messageID))

	require.Len(t, journal(ctx, t, target, r.to), 1)
}

func Malformed(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(ctx, t, target)
	r := newRequest("example.com")
	r.body = map[string]string{"to": r.to}

	b.publish(ctx, t, r)
	dead := b.outcome(ctx, t, r.messageID)

	require.Equal(t, deadExchange, dead.Exchange)
	require.Equal(t, r.messageID, dead.MessageId)
	require.Equal(t, malformed, dead.Headers[deadReasonHeader])
	require.Empty(t, b.sent(r.messageID))

	require.Empty(t, journal(ctx, t, target, r.to))
}

func bounded(t testing.TB, target Target) context.Context {
	require.Positive(t, target.Timeout, "Target.Timeout")
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	t.Cleanup(cancel)
	return ctx
}

type request struct {
	messageID     string
	correlationID string
	to            string
	body          map[string]string
}

func newRequest(domain string) request {
	to := strings.ToLower(rand.Text()) + "@" + domain
	return request{
		messageID:     rand.Text(),
		correlationID: rand.Text(),
		to:            to,
		body: map[string]string{
			"to":      to,
			"subject": "Your order has shipped",
			"text":    "It arrives on Tuesday.",
		},
	}
}

type broker struct {
	publishing *amqp.Channel
	returns    <-chan amqp.Return
	eventsIn   <-chan amqp.Delivery
	deadIn     <-chan amqp.Delivery
	seen       []amqp.Delivery
}

func watch(ctx context.Context, t testing.TB, target Target) *broker {
	t.Helper()
	conn := dial(t, target)

	ch, err := conn.Channel()
	require.NoError(t, err)
	events := bind(ctx, t, ch, eventsExchange, "#")
	dead := bind(ctx, t, ch, deadExchange, "")

	publishing, err := conn.Channel()
	require.NoError(t, err)
	require.NoError(t, publishing.Confirm(false))
	returns := publishing.NotifyReturn(make(chan amqp.Return, 1))

	return &broker{publishing: publishing, returns: returns, eventsIn: events, deadIn: dead}
}

func dial(t testing.TB, target Target) *amqp.Connection {
	t.Helper()
	scheme := "amqp"
	if target.TLS != nil {
		scheme = "amqps"
	}
	conn, err := amqp.DialConfig(scheme+"://"+target.Addr, amqp.Config{
		SASL:            []amqp.Authentication{&amqp.PlainAuth{Username: target.Username, Password: target.Password}},
		Vhost:           target.Vhost,
		TLSClientConfig: target.TLS,
	})
	require.NoError(t, err, "connecting to the broker at %s", target.Addr)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func bind(ctx context.Context, t testing.TB, ch *amqp.Channel, exchange, key string) <-chan amqp.Delivery {
	t.Helper()
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	require.NoError(t, err)
	require.NoError(t, ch.QueueBind(q.Name, key, exchange, false, nil))
	deliveries, err := ch.ConsumeWithContext(ctx, q.Name, "", true, true, false, false, nil)
	require.NoError(t, err)
	return deliveries
}

func (b *broker) publish(ctx context.Context, t testing.TB, r request) {
	t.Helper()
	body, err := json.Marshal(r.body)
	require.NoError(t, err)
	confirm, err := b.publishing.PublishWithDeferredConfirmWithContext(ctx, requestExchange, sendKey, true, false, amqp.Publishing{
		ContentType:   "application/json",
		DeliveryMode:  amqp.Persistent,
		MessageId:     r.messageID,
		CorrelationId: r.correlationID,
		Body:          body,
	})
	require.NoError(t, err)
	acked, err := confirm.WaitContext(ctx)
	require.NoError(t, err)
	require.True(t, acked, "the broker nacked the request %s", r.messageID)
	select {
	case ret := <-b.returns:
		require.FailNowf(t, "unroutable", "the broker returned the request %s: %s", ret.MessageId, ret.ReplyText)
	default:
	}
}

func (b *broker) outcome(ctx context.Context, t testing.TB, messageID string) amqp.Delivery {
	t.Helper()
	b.await(ctx, t, func() bool { return len(b.of(messageID)) > 0 }, "the outcome of %s", messageID)
	return b.of(messageID)[0]
}

func (b *broker) events(ctx context.Context, t testing.TB, messageID string, n int) []amqp.Delivery {
	t.Helper()
	b.await(ctx, t, func() bool { return len(b.sent(messageID)) >= n }, "%d events of %s", n, messageID)
	return b.sent(messageID)
}

func (b *broker) await(ctx context.Context, t testing.TB, done func() bool, format string, args ...any) {
	t.Helper()
	for !done() {
		select {
		case d, ok := <-b.eventsIn:
			require.True(t, ok, "the consumer of %s was cancelled", eventsExchange)
			b.seen = append(b.seen, d)
		case d, ok := <-b.deadIn:
			require.True(t, ok, "the consumer of %s was cancelled", deadExchange)
			b.seen = append(b.seen, d)
		case <-ctx.Done():
			require.FailNowf(t, "timed out", "waiting for "+format+": %v", append(args, context.Cause(ctx))...)
		}
	}
}

func (b *broker) of(messageID string) []amqp.Delivery {
	var out []amqp.Delivery
	for _, d := range b.seen {
		if d.MessageId == messageID {
			out = append(out, d)
		}
	}
	return out
}

func (b *broker) sent(messageID string) []amqp.Delivery {
	var out []amqp.Delivery
	for _, d := range b.of(messageID) {
		if d.Exchange == eventsExchange {
			out = append(out, d)
		}
	}
	return out
}

func (b *broker) deadLetters(messageID string) []amqp.Delivery {
	var out []amqp.Delivery
	for _, d := range b.of(messageID) {
		if d.Exchange == deadExchange {
			out = append(out, d)
		}
	}
	return out
}

func providerID(t testing.TB, event amqp.Delivery) string {
	t.Helper()
	var body struct {
		ProviderID string `json:"provider_id"`
	}
	require.NoError(t, json.Unmarshal(event.Body, &body))
	return body.ProviderID
}

type logged struct {
	Headers map[string]any `json:"headers"`
}

func (l logged) header(name string) string {
	for k, v := range l.Headers {
		if strings.EqualFold(k, name) {
			return fmt.Sprint(v)
		}
	}
	return ""
}

func journal(ctx context.Context, t testing.TB, target Target, to string) []logged {
	t.Helper()
	query, err := json.Marshal(map[string]any{
		"method": http.MethodPost,
		"url":    "/v1/messages",
		"bodyPatterns": []any{
			map[string]any{"matchesJsonPath": map[string]any{"expression": "$.to", "equalTo": to}},
		},
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.WireMock.JoinPath("__admin", "requests", "find").String(), bytes.NewReader(query))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := target.Client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)

	var found struct {
		Requests []logged `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(body, &found))
	return found.Requests
}
