//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app_test

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/mailer/app"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	scenarioKey   = "acme.fault"
	retryPoint    = "rabbitmq:mail.retry:mail.send"
	maxAttempts   = 3
	exitedOnError = 1
)

type faultsMailer struct {
	env        map[string]string
	probesAddr string
	probes     *http.Client
}

func newFaultsMailer(t *testing.T, broker *rabbitMQ, provider *url.URL, mailer login, trusted ...string) *faultsMailer {
	t.Helper()
	ca := humustest.NewCA(t)
	dir := t.TempDir()
	probesAddr := freeAddr(t)
	return &faultsMailer{
		env: map[string]string{
			"RABBITMQ_ADDR":           broker.addr,
			"RABBITMQ_USERNAME":       mailer.username,
			"RABBITMQ_PASSWORD_FILE":  writeFile(t, dir, "rabbitmq-password", []byte(mailer.password+"\n")),
			"MAIL_FROM":               "mailer@example.com",
			"MAIL_MAX_ATTEMPTS":       "3",
			"MAIL_TRUSTED_PUBLISHERS": strings.Join(trusted, ","),
			"PROVIDER_URL":            provider.String(),
			"PROVIDER_API_KEY_FILE":   writeFile(t, dir, "provider-api-key", []byte(rand.Text())),
			"PROBES_ADDR":             probesAddr,
			"PROBES_TLS_PEM_FILE":     writeFile(t, dir, "probes.pem", pemOf(t, ca.Issue(t, "127.0.0.1"))),
		},
		probesAddr: probesAddr,
		probes: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}},
		},
	}
}

func (m *faultsMailer) start(t *testing.T) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	t.Helper()
	return humustest.Start(t, app.Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(app.Faults)),
		humustest.WithEnv(m.env),
	)
}

func (m *faultsMailer) awaitReady(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		resp, err := m.probes.Get("https://" + m.probesAddr + "/health/readiness")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, waitLimit, 100*time.Millisecond, "the mailer didn't become ready")
}

func stop(t *testing.T, p *bedrocktest.Process, status <-chan bedrock.ExitStatus) {
	t.Helper()
	select {
	case <-p.Notified(syscall.SIGTERM):
	case <-time.After(waitLimit):
		t.Fatal("the mailer never listened for SIGTERM")
	}
	p.Signal(syscall.SIGTERM)
	select {
	case st := <-status:
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	case <-time.After(waitLimit):
		t.Fatal("the mailer didn't exit")
	}
}

type observer struct {
	in   <-chan amqp.Delivery
	seen []amqp.Delivery
}

func observe(t *testing.T, broker *rabbitMQ, l login, exchanges map[string]string) *observer {
	t.Helper()
	conn := connect(t, broker, l)
	ch, err := conn.Channel()
	require.NoError(t, err)
	in := make(chan amqp.Delivery)
	for exchange, key := range exchanges {
		q, err := ch.QueueDeclare("", false, true, true, false, nil)
		require.NoError(t, err)
		require.NoError(t, ch.QueueBind(q.Name, key, exchange, false, nil))
		deliveries, err := ch.ConsumeWithContext(t.Context(), q.Name, "", true, true, false, false, nil)
		require.NoError(t, err)
		go func() {
			for d := range deliveries {
				select {
				case in <- d:
				case <-t.Context().Done():
					return
				}
			}
		}()
	}
	return &observer{in: in}
}

func (o *observer) await(t *testing.T, what string, done func() bool) {
	t.Helper()
	timeout := time.After(waitLimit)
	for !done() {
		select {
		case d := <-o.in:
			o.seen = append(o.seen, d)
		case <-timeout:
			require.FailNowf(t, "timed out", "waiting for %s", what)
		}
	}
}

func (o *observer) of(messageID, exchange string) []amqp.Delivery {
	var out []amqp.Delivery
	for _, d := range o.seen {
		if d.MessageId == messageID && d.Exchange == exchange {
			out = append(out, d)
		}
	}
	return out
}

func connect(t *testing.T, broker *rabbitMQ, l login) *amqp.Connection {
	t.Helper()
	conn, err := amqp.DialConfig("amqp://"+broker.addr, amqp.Config{
		SASL:  []amqp.Authentication{&amqp.PlainAuth{Username: l.username, Password: l.password}},
		Vhost: "/",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

type requester struct {
	ch   *amqp.Channel
	user string
}

func newRequester(t *testing.T, broker *rabbitMQ, l login) *requester {
	t.Helper()
	ch, err := connect(t, broker, l).Channel()
	require.NoError(t, err)
	require.NoError(t, ch.Confirm(false))
	return &requester{ch: ch, user: l.username}
}

func (r *requester) request(t *testing.T, to string, rules ...fit.Rule) string {
	t.Helper()
	return r.publish(t, map[string]string{"to": to, "subject": "Your order has shipped", "text": "It arrives on Tuesday."}, rules...)
}

func (r *requester) publish(t *testing.T, body map[string]string, rules ...fit.Rule) string {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	messageID := rand.Text()
	confirm, err := r.ch.PublishWithDeferredConfirmWithContext(t.Context(), "mail", "mail.send", true, false, amqp.Publishing{
		Headers:       amqp.Table{"baggage": scenario(t, rules...)},
		ContentType:   "application/json",
		DeliveryMode:  amqp.Persistent,
		MessageId:     messageID,
		CorrelationId: rand.Text(),
		UserId:        r.user,
		Body:          b,
	})
	require.NoError(t, err)
	acked, err := confirm.WaitContext(t.Context())
	require.NoError(t, err)
	require.True(t, acked, "the broker nacked the request %s", messageID)
	return messageID
}

func codec(t *testing.T) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	return c
}

func scenario(t *testing.T, rules ...fit.Rule) string {
	t.Helper()
	b, err := codec(t).Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	return b.String()
}

func carriesScenario(t *testing.T, d amqp.Delivery) bool {
	t.Helper()
	header, _ := d.Headers["baggage"].(string)
	b, err := baggage.Parse(header)
	require.NoError(t, err)
	_, carried, err := codec(t).Decode(b)
	require.NoError(t, err)
	return carried
}

func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func address() string {
	return strings.ToLower(rand.Text()) + "@example.com"
}

func TestInit_rehearses_failures_from_a_trusted_scenario(t *testing.T) {
	broker := startRabbitMQ(t)
	provider := startWireMock(t)
	providerPoint := "http:" + provider.Host
	mailer := broker.addUser(t, `^$`, `^mail\.(retry|dead|events)$`, `^mail\.send$`)
	cased := broker.addUser(t, `^amq\.gen-.*$`, `^(mail|amq\.gen-.*)$`, `^(mail\.(events|dead)|amq\.gen-.*)$`)
	stranger := broker.addUser(t, `^$`, `^mail$`, `^$`)
	watcher := broker.addUser(t, `^amq\.gen-.*$`, `^amq\.gen-.*$`, `^(mail\.(events|dead|retry)|amq\.gen-.*)$`)
	client := &http.Client{Timeout: waitLimit}

	m := newFaultsMailer(t, broker, provider, mailer, cased.username)
	trusted := newRequester(t, broker, cased)
	untrusted := newRequester(t, broker, stranger)

	t.Run("while it runs", func(t *testing.T) {
		p, status := m.start(t)
		m.awaitReady(t)
		defer stop(t, p, status)

		t.Run("a scenario from a publisher that isn't trusted is stripped, and the request is sent", func(t *testing.T) {
			o := observe(t, broker, watcher, map[string]string{"mail.retry": "mail.send", "mail.dead": "", "mail.events": "#"})
			to := address()
			id := untrusted.request(t, to, abort(providerPoint))

			o.await(t, "the request to be sent", func() bool { return len(o.of(id, "mail.events")) > 0 })

			event := o.of(id, "mail.events")[0]
			require.Equal(t, "mail.sent", event.RoutingKey)
			require.False(t, carriesScenario(t, event), "the event carries the scenario")
			require.Empty(t, o.of(id, "mail.retry"))
			require.Empty(t, o.of(id, "mail.dead"))
			require.Len(t, journal(t, client, provider, to), 1)
		})

		t.Run("the events and the dead letters carry no scenario", func(t *testing.T) {
			o := observe(t, broker, watcher, map[string]string{"mail.dead": "", "mail.events": "#"})
			slow := fit.Rule{Point: providerPoint, Fault: fit.Fault{Delay: 10 * time.Millisecond}}
			sent := trusted.request(t, address(), slow)
			refused := trusted.request(t, strings.ToLower(rand.Text())+"@example.invalid", slow)
			malformed := trusted.publish(t, map[string]string{"to": address()}, slow)

			o.await(t, "every outcome", func() bool {
				return len(o.of(sent, "mail.events")) > 0 && len(o.of(refused, "mail.events")) > 0 && len(o.of(malformed, "mail.dead")) > 0
			})

			for name, d := range map[string]amqp.Delivery{
				"mail.sent":    o.of(sent, "mail.events")[0],
				"mail.refused": o.of(refused, "mail.events")[0],
				"dead letter":  o.of(malformed, "mail.dead")[0],
			} {
				require.False(t, carriesScenario(t, d), "the %s carries the scenario", name)
			}
			require.Equal(t, "mail.sent", o.of(sent, "mail.events")[0].RoutingKey)
			require.Equal(t, "mail.refused", o.of(refused, "mail.events")[0].RoutingKey)
			require.Equal(t, "malformed", o.of(malformed, "mail.dead")[0].Headers["x-mail-dead-reason"])
		})

		t.Run("a trusted scenario that aborts the provider fails each send, and parks the request with it", func(t *testing.T) {
			o := observe(t, broker, watcher, map[string]string{"mail.retry": "mail.send", "mail.dead": "", "mail.events": "#"})
			to := address()
			id := trusted.request(t, to, abort(providerPoint))

			o.await(t, "the request to be dead-lettered", func() bool { return len(o.of(id, "mail.dead")) > 0 })

			parked := o.of(id, "mail.retry")
			require.Len(t, parked, maxAttempts-1)
			for i, d := range parked {
				require.True(t, carriesScenario(t, d), "parked copy %d has no scenario", i)
				require.Equal(t, mailer.username, d.UserId)
				require.EqualValues(t, i+2, d.Headers["x-mail-attempt"])
			}
			dead := o.of(id, "mail.dead")[0]
			require.Equal(t, "gave-up", dead.Headers["x-mail-dead-reason"])
			require.EqualValues(t, maxAttempts, dead.Headers["x-mail-dead-attempts"])
			require.NotEmpty(t, dead.Headers["x-mail-dead-error"])
			require.False(t, carriesScenario(t, dead), "the dead letter carries the scenario")
			require.Empty(t, o.of(id, "mail.events"))
			require.Empty(t, journal(t, client, provider, to))
		})
	})

	t.Run("a request that can go nowhere is dead-lettered by its delivery limit", func(t *testing.T) {
		o := observe(t, broker, watcher, map[string]string{"mail.dead": ""})
		to := address()
		id := trusted.request(t, to, abort(providerPoint), abort(retryPoint))

		exits := 0
		exited := func(st bedrock.ExitStatus) {
			t.Helper()
			require.Equal(t, exitedOnError, st.Code(), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), id)
			exits++
		}
		for len(o.of(id, "mail.dead")) == 0 {
			require.Less(t, exits, 5, "the request was never dead-lettered")
			p, status := m.start(t)
			select {
			case st := <-status:
				exited(st)
				continue
			case d := <-o.in:
				o.seen = append(o.seen, d)
			case <-time.After(waitLimit):
				t.Fatal("the mailer neither exited nor dead-lettered the request")
			}
			select {
			case st := <-status:
				exited(st)
			case <-time.After(10 * time.Second):
				stop(t, p, status)
			}
		}

		require.Equal(t, 3, exits, "the delivery limit of 2 allows 3 deliveries")
		dead := o.of(id, "mail.dead")
		require.Len(t, dead, 1)
		require.Equal(t, "delivery_limit", dead[0].Headers["x-first-death-reason"])
		require.NotContains(t, dead[0].Headers, "x-mail-dead-reason")
		require.Empty(t, journal(t, client, provider, to))
	})
}

func journal(t *testing.T, client *http.Client, provider *url.URL, to string) []json.RawMessage {
	t.Helper()
	query, err := json.Marshal(map[string]any{
		"method": http.MethodPost,
		"url":    "/v1/messages",
		"bodyPatterns": []any{
			map[string]any{"matchesJsonPath": map[string]any{"expression": "$.to", "equalTo": to}},
		},
	})
	require.NoError(t, err)
	resp, err := client.Post(provider.JoinPath("__admin", "requests", "find").String(), "application/json", bytes.NewReader(query))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var found struct {
		Requests []json.RawMessage `json:"requests"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&found))
	return found.Requests
}
