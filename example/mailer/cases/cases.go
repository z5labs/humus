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
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/fault/jsonscenario"
)

const (
	requestExchange = "mail"
	eventsExchange  = "mail.events"
	deadExchange    = "mail.dead"
	retryExchange   = "mail.retry"

	sendKey    = "mail.send"
	sentKey    = "mail.sent"
	refusedKey = "mail.refused"

	deadReasonHeader   = "x-mail-dead-reason"
	deadAttemptsHeader = "x-mail-dead-attempts"
	attemptHeader      = "x-mail-attempt"
	baggageHeader      = "baggage"

	malformed = "malformed"
	gaveUp    = "gave-up"

	eventsPoint = "rabbitmq:" + eventsExchange + ":" + sentKey
)

const (
	slowProvider = 2 * time.Second
	slowInFlight = 3 * time.Second
	inFlight     = time.Second
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
	{Name: "SlowProvider", Run: SlowProvider},
	{Name: "UntrustedScenario", Run: UntrustedScenario},
}

var Alone = []Case{
	{Name: "LostConnection", Run: LostConnection},
	{Name: "ResultNotPublished", Run: ResultNotPublished},
	{Name: "ProviderDown", Run: ProviderDown},
}

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

func SlowProvider(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(ctx, t, target)
	r := rehearsal(t, target, delay(providerPoint(t, target), slowProvider))

	b.publish(ctx, t, r)
	event := b.outcome(ctx, t, r.messageID)

	require.Equal(t, sentKey, event.RoutingKey)
	require.False(t, carriesScenario(t, target, event), "the event carries the scenario")
	require.Empty(t, b.parked(r.messageID))

	require.Len(t, journal(ctx, t, target, r.to), 1)
}

func UntrustedScenario(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(ctx, t, target)
	r := rehearsal(t, target, abort(providerPoint(t, target)))
	r.userID = ""

	b.publish(ctx, t, r)
	event := b.outcome(ctx, t, r.messageID)

	require.Equal(t, sentKey, event.RoutingKey)
	require.False(t, carriesScenario(t, target, event), "the event carries the scenario")
	require.Empty(t, b.parked(r.messageID))

	require.Len(t, journal(ctx, t, target, r.to), 1)
}

func ProviderDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(ctx, t, target)
	r := rehearsal(t, target, abort(providerPoint(t, target)))

	b.publish(ctx, t, r)
	dead := b.outcome(ctx, t, r.messageID)

	attempts := b.gaveUp(ctx, t, target, r, dead)
	require.Empty(t, b.sent(r.messageID))
	require.Len(t, b.parked(r.messageID), attempts-1)

	require.Empty(t, journal(ctx, t, target, r.to))
}

func LostConnection(t testing.TB, target Target) {
	if target.CloseConnections == nil {
		t.Skip("Target.CloseConnections is nil")
	}
	ctx := bounded(t, target)
	b := watch(ctx, t, target)
	r := rehearsal(t, target, delay(providerPoint(t, target), slowInFlight))

	b.publish(ctx, t, r)
	pause(ctx, t, inFlight)
	require.NoError(t, target.CloseConnections(ctx))
	b = watch(ctx, t, target)
	event := b.outcome(ctx, t, r.messageID)

	require.Equal(t, sentKey, event.RoutingKey)
	require.Equal(t, r.messageID, event.MessageId)
	require.Equal(t, acceptedID(r.messageID), providerID(t, event))

	after := newRequest("example.com")
	b.publish(ctx, t, after)
	require.Equal(t, sentKey, b.outcome(ctx, t, after.messageID).RoutingKey)

	sent := journal(ctx, t, target, r.to)
	require.NotEmpty(t, sent)
	for _, s := range sent {
		require.Equal(t, r.messageID, s.header("Idempotency-Key"))
	}
}

func ResultNotPublished(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(ctx, t, target)
	r := rehearsal(t, target, abort(eventsPoint))

	b.publish(ctx, t, r)
	dead := b.outcome(ctx, t, r.messageID)

	attempts := b.gaveUp(ctx, t, target, r, dead)
	require.Empty(t, b.sent(r.messageID))
	require.Len(t, b.parked(r.messageID), attempts-1)

	sent := journal(ctx, t, target, r.to)
	require.Len(t, sent, attempts)
	for _, s := range sent {
		require.Equal(t, r.messageID, s.header("Idempotency-Key"))
	}
}

func (b *broker) gaveUp(ctx context.Context, t testing.TB, target Target, r request, dead amqp.Delivery) int {
	t.Helper()
	require.Equal(t, deadExchange, dead.Exchange)
	require.Equal(t, r.messageID, dead.MessageId)
	require.Equal(t, gaveUp, dead.Headers[deadReasonHeader])
	require.False(t, carriesScenario(t, target, dead), "the dead letter carries the scenario")
	attempts, ok := dead.Headers[deadAttemptsHeader].(int64)
	require.True(t, ok, "the dead letter's %s is %v", deadAttemptsHeader, dead.Headers[deadAttemptsHeader])
	require.Positive(t, attempts)

	b.await(ctx, t, func() bool { return len(b.parked(r.messageID)) >= int(attempts)-1 }, "%d parked copies of %s", attempts-1, r.messageID)
	for i, p := range b.parked(r.messageID) {
		require.True(t, carriesScenario(t, target, p), "parked copy %d has no scenario", i)
		require.EqualValues(t, i+2, p.Headers[attemptHeader])
	}
	return int(attempts)
}

func bounded(t testing.TB, target Target) context.Context {
	require.Positive(t, target.Timeout, "Target.Timeout")
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	t.Cleanup(cancel)
	return ctx
}

func pause(ctx context.Context, t testing.TB, d time.Duration) {
	t.Helper()
	select {
	case <-time.After(d):
	case <-ctx.Done():
		require.FailNowf(t, "timed out", "pausing for %s: %v", d, context.Cause(ctx))
	}
}

func providerPoint(t testing.TB, target Target) string {
	t.Helper()
	require.NotEmpty(t, target.ProviderPoint, "Target.ProviderPoint")
	return target.ProviderPoint
}

func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func delay(point string, d time.Duration) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Delay: d}}
}

func acceptedID(idempotencyKey string) string {
	return "msg_" + idempotencyKey
}

func codec(t testing.TB, target Target) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New(target.ScenarioKey)
	require.NoError(t, err, "Target.ScenarioKey")
	return c
}

func carriesScenario(t testing.TB, target Target, d amqp.Delivery) bool {
	t.Helper()
	header, _ := d.Headers[baggageHeader].(string)
	b, err := baggage.Parse(header)
	require.NoError(t, err)
	_, carried, err := codec(t, target).Decode(b)
	require.NoError(t, err)
	return carried
}

type request struct {
	messageID     string
	correlationID string
	userID        string
	baggage       string
	to            string
	body          map[string]string
}

func rehearsal(t testing.TB, target Target, rules ...fit.Rule) request {
	t.Helper()
	b, err := codec(t, target).Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	r := newRequest("example.com")
	r.userID = target.Username
	r.baggage = b.String()
	return r
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
	retryIn    <-chan amqp.Delivery
	seen       []amqp.Delivery
}

func watch(ctx context.Context, t testing.TB, target Target) *broker {
	t.Helper()
	conn := dial(t, target)

	ch, err := conn.Channel()
	require.NoError(t, err)
	events := bind(ctx, t, ch, eventsExchange, "#")
	dead := bind(ctx, t, ch, deadExchange, "")
	retry := bind(ctx, t, ch, retryExchange, sendKey)

	publishing, err := conn.Channel()
	require.NoError(t, err)
	require.NoError(t, publishing.Confirm(false))
	returns := publishing.NotifyReturn(make(chan amqp.Return, 1))

	return &broker{publishing: publishing, returns: returns, eventsIn: events, deadIn: dead, retryIn: retry}
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
	var headers amqp.Table
	if r.baggage != "" {
		headers = amqp.Table{baggageHeader: r.baggage}
	}
	confirm, err := b.publishing.PublishWithDeferredConfirmWithContext(ctx, requestExchange, sendKey, true, false, amqp.Publishing{
		Headers:       headers,
		ContentType:   "application/json",
		DeliveryMode:  amqp.Persistent,
		MessageId:     r.messageID,
		CorrelationId: r.correlationID,
		UserId:        r.userID,
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
	b.await(ctx, t, func() bool { return len(b.outcomes(messageID)) > 0 }, "the outcome of %s", messageID)
	return b.outcomes(messageID)[0]
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
		case d, ok := <-b.retryIn:
			require.True(t, ok, "the consumer of %s was cancelled", retryExchange)
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

func (b *broker) outcomes(messageID string) []amqp.Delivery {
	var out []amqp.Delivery
	for _, d := range b.of(messageID) {
		if d.Exchange != retryExchange {
			out = append(out, d)
		}
	}
	return out
}

func (b *broker) sent(messageID string) []amqp.Delivery {
	return b.from(messageID, eventsExchange)
}

func (b *broker) deadLetters(messageID string) []amqp.Delivery {
	return b.from(messageID, deadExchange)
}

func (b *broker) parked(messageID string) []amqp.Delivery {
	return b.from(messageID, retryExchange)
}

func (b *broker) from(messageID, exchange string) []amqp.Delivery {
	var out []amqp.Delivery
	for _, d := range b.of(messageID) {
		if d.Exchange == exchange {
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
