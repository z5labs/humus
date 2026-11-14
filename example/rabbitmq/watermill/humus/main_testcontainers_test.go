//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/rabbitmq/watermill/cases"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	rabbitmqImage   = "docker.io/library/rabbitmq:4.3.6"
	definitionsFile = "/etc/rabbitmq/invoicer-definitions.json"
	ordersExchange  = "orders"
	waitLimit       = time.Minute
)

const retryPolicy = `{"message-ttl":1000,"dead-letter-exchange":"","dead-letter-routing-key":"orders_invoicer","dead-letter-strategy":"at-least-once","overflow":"reject-publish"}`

func TestInit_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			s := start(t)
			c.Run(t, s.target)
			s.stop(t)
		})
	}
}

func TestInit_parks_an_order_whose_invoice_no_queue_takes_then_dead_letters_it(t *testing.T) {
	t.Parallel()
	s := start(t)
	w := s.watch(t, retryExchange, deadLetterExchange)

	s.publish(t, "corr-1", `{"id":"order-1","customer":"ada","total":4250}`)
	dead := w.await(t, deadLetterExchange, 1)
	s.stop(t)

	parked := w.of(retryExchange)
	require.Len(t, parked, 2)
	for n, p := range parked {
		require.Equal(t, strconv.Itoa(n+1), header(p.d, headerAttempt))
		require.Contains(t, header(p.d, headerError), "unroutable")
		require.Equal(t, "corr-1", header(p.d, headerCorrelationID))
		require.Equal(t, `{"id":"order-1","customer":"ada","total":4250}`, string(p.d.Body))
	}
	require.GreaterOrEqual(t, parked[1].at.Sub(parked[0].at), 900*time.Millisecond, "the second attempt waited for the retry queue's message-ttl")

	require.Len(t, dead, 1)
	require.Equal(t, `{"id":"order-1","customer":"ada","total":4250}`, string(dead[0].d.Body))
	require.Equal(t, "corr-1", header(dead[0].d, headerCorrelationID))
	require.Equal(t, "3", header(dead[0].d, headerDeadAttempts))
	require.Contains(t, header(dead[0].d, headerDeadError), "unroutable")
	for k := range dead[0].d.Headers {
		require.False(t, strings.HasPrefix(k, "x-") || strings.HasPrefix(k, "retry."), "the dead letter carries %s", k)
	}
}

func TestInit_invoices_a_parked_order_once_a_queue_takes_its_invoice(t *testing.T) {
	t.Parallel()
	s := start(t)
	w := s.watch(t, retryExchange, deadLetterExchange)

	s.publish(t, "corr-2", `{"id":"order-2","customer":"grace","total":990}`)
	w.await(t, retryExchange, 1)
	invoices := s.watch(t, invoicesExchange)
	invoice := invoices.await(t, invoicesExchange, 1)
	s.stop(t)

	require.JSONEq(t, `{"order_id":"order-2","customer":"grace","amount":990}`, string(invoice[0].d.Body))
	require.Equal(t, "corr-2", header(invoice[0].d, headerCorrelationID))
	require.NotEmpty(t, w.of(retryExchange))
	require.Empty(t, w.of(deadLetterExchange))
}

type service struct {
	broker *rabbitMQ
	target cases.Target
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

func start(t *testing.T) *service {
	t.Helper()
	b := startRabbitMQ(t)
	invoicer := b.addUser(t, `^$`, `^(invoices|orders\.poison|orders_invoicer\.retry)$`, `^orders_invoicer$`)

	ca := humustest.NewCA(t)
	certPEM, keyPEM := pemPair(t, ca.Issue(t, "127.0.0.1"))
	dir := t.TempDir()
	probesAddr := freeAddr(t)
	env := map[string]string{
		"AMQP_URI":             (&url.URL{Scheme: "amqp", User: url.UserPassword(invoicer.username, invoicer.password), Host: b.addr, Path: "/"}).String(),
		"RETRY_ATTEMPTS":       "3",
		"PROBES_ADDR":          probesAddr,
		"PROBES_TLS_CERT_FILE": writeFile(t, dir, "tls.crt", certPEM),
		"PROBES_TLS_KEY_FILE":  writeFile(t, dir, "tls.key", keyPEM),
	}
	p, status := humustest.Start(t, Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(env),
	)

	probes := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}},
	}
	for ready := time.Now(); ; time.Sleep(100 * time.Millisecond) {
		select {
		case st := <-status:
			require.FailNowf(t, "the invoicer exited", "exit %d: %v", st.Code(), st.Cause())
		default:
		}
		if resp, err := probes.Get("https://" + probesAddr + "/health/readiness"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		require.Less(t, time.Since(ready), waitLimit, "the invoicer never turned ready")
	}

	return &service{
		broker: b,
		target: cases.Target{
			Addr:               b.addr,
			Vhost:              "/",
			Username:           b.container.AdminUsername,
			Password:           b.container.AdminPassword,
			OrdersExchange:     ordersExchange,
			InvoicesExchange:   invoicesExchange,
			DeadLetterExchange: deadLetterExchange,
			Timeout:            waitLimit,
		},
		p:      p,
		status: status,
	}
}

func (s *service) stop(t *testing.T) {
	t.Helper()
	select {
	case <-s.p.Notified(syscall.SIGTERM):
	case <-time.After(waitLimit):
		t.Fatal("the invoicer never listened for SIGTERM")
	}
	s.p.Signal(syscall.SIGTERM)
	select {
	case st := <-s.status:
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	case <-time.After(waitLimit):
		t.Fatal("the invoicer didn't exit")
	}
}

func (s *service) channel(t *testing.T) *amqp.Channel {
	t.Helper()
	conn, err := amqp.DialConfig("amqp://"+s.broker.addr, amqp.Config{
		SASL: []amqp.Authentication{&amqp.PlainAuth{Username: s.broker.container.AdminUsername, Password: s.broker.container.AdminPassword}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	require.NoError(t, err)
	return ch
}

func (s *service) publish(t *testing.T, correlationID, body string) {
	t.Helper()
	ch := s.channel(t)
	require.NoError(t, ch.Confirm(false))
	returns := ch.NotifyReturn(make(chan amqp.Return, 1))
	confirm, err := ch.PublishWithDeferredConfirmWithContext(t.Context(), ordersExchange, "", true, false, amqp.Publishing{
		Headers:      amqp.Table{headerCorrelationID: correlationID},
		DeliveryMode: amqp.Persistent,
		Body:         []byte(body),
	})
	require.NoError(t, err)
	acked, err := confirm.WaitContext(t.Context())
	require.NoError(t, err)
	require.True(t, acked)
	select {
	case ret := <-returns:
		require.FailNowf(t, "unroutable", "the broker returned the order: %s", ret.ReplyText)
	default:
	}
}

type received struct {
	d  amqp.Delivery
	at time.Time
}

type watcher struct {
	in   <-chan amqp.Delivery
	seen []received
}

func (s *service) watch(t *testing.T, exchanges ...string) *watcher {
	t.Helper()
	ch := s.channel(t)
	q, err := ch.QueueDeclare("", false, false, true, false, nil)
	require.NoError(t, err)
	for _, e := range exchanges {
		require.NoError(t, ch.QueueBind(q.Name, "", e, false, nil), "binding a queue to %s", e)
	}
	in, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	require.NoError(t, err)
	return &watcher{in: in}
}

func (w *watcher) await(t *testing.T, exchange string, n int) []received {
	t.Helper()
	timeout := time.After(waitLimit)
	for len(w.of(exchange)) < n {
		select {
		case d, ok := <-w.in:
			require.True(t, ok, "the watcher's consumer was cancelled")
			w.seen = append(w.seen, received{d: d, at: time.Now()})
		case <-timeout:
			require.FailNowf(t, "timed out", "%s never got %d messages", exchange, n)
		}
	}
	return w.of(exchange)
}

func (w *watcher) of(exchange string) []received {
	var out []received
	for _, r := range w.seen {
		if r.d.Exchange == exchange {
			out = append(out, r)
		}
	}
	return out
}

func header(d amqp.Delivery, key string) string {
	v, _ := d.Headers[key].(string)
	return v
}

type login struct {
	username string
	password string
}

type rabbitMQ struct {
	container *tcrabbitmq.RabbitMQContainer
	addr      string
}

func startRabbitMQ(t *testing.T) *rabbitMQ {
	t.Helper()
	c, err := tcrabbitmq.Run(t.Context(), rabbitmqImage,
		tcrabbitmq.WithAdminUsername("admin-"+strings.ToLower(rand.Text())),
		tcrabbitmq.WithAdminPassword(rand.Text()),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      "definitions.json",
			ContainerFilePath: definitionsFile,
			FileMode:          0o644,
		}),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting RabbitMQ in a container")
	addr, err := c.PortEndpoint(t.Context(), tcrabbitmq.DefaultAMQPPort, "")
	require.NoError(t, err)

	r := &rabbitMQ{container: c, addr: addr}
	r.ctl(t, "import_definitions", definitionsFile)
	r.ctl(t, "set_policy", "--vhost", "/", "--priority", "1", "--apply-to", "queues", "tests.orders_invoicer.retry", `^orders_invoicer\.retry$`, retryPolicy)
	return r
}

func (r *rabbitMQ) addUser(t *testing.T, configure, write, read string) login {
	t.Helper()
	l := login{username: strings.ToLower(rand.Text()), password: rand.Text()}
	r.ctl(t, "add_user", l.username, l.password)
	r.ctl(t, "set_permissions", "--vhost", "/", l.username, configure, write, read)
	return l
}

func (r *rabbitMQ) ctl(t *testing.T, args ...string) {
	t.Helper()
	code, out, err := r.container.Exec(t.Context(), append([]string{"rabbitmqctl"}, args...), tcexec.Multiplexed())
	require.NoError(t, err, "rabbitmqctl %s", args[0])
	b, err := io.ReadAll(out)
	require.NoError(t, err)
	require.Zero(t, code, "rabbitmqctl %s: %s", args[0], b)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func writeFile(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}
