// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// The tests in this file run the code of example_test.go, which the
// RabbitMQ page of the docs site quotes. None of them needs a broker.

// fakeEvents is the orders Processor's events in its unit tests: it records
// what is published, and fails each Publish with err while err is set.
type fakeEvents struct {
	mu        sync.Mutex
	published []rabbitmq.Publishing
	err       error
}

func (f *fakeEvents) Publish(_ context.Context, m rabbitmq.Publishing) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.published = append(f.published, m)
	return nil
}

func TestOrders_stores_an_order_once_and_publishes_its_event_on_each_delivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &fakeEvents{}
		h := &orders{stored: map[string][]byte{}, events: ev}
		m := rabbitmq.Message{Queue: "orders", MessageID: "order-1", ContentType: "application/json", Body: []byte(`{"total":42}`)}

		require.NoError(t, h.Handle(t.Context(), m))
		m.Redelivered = true
		require.NoError(t, h.Handle(t.Context(), m), "a redelivery is no error")

		require.Equal(t, map[string][]byte{"order-1": []byte(`{"total":42}`)}, h.stored, "stored once")
		require.Len(t, ev.published, 2, "an event on each delivery")
		for _, p := range ev.published {
			require.Equal(t, rabbitmq.Publishing{
				Exchange:    "orders",
				RoutingKey:  "order.accepted",
				MessageID:   "order-1",
				ContentType: "application/json",
				Body:        []byte(`{"total":42}`),
			}, p)
		}
	})
}

func TestOrders_fails_a_delivery_whose_event_the_broker_did_not_confirm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		unroutable := errors.Join(errors.New("rabbitmq: publishing to orders:order.accepted"), rabbitmq.ErrUnroutable)
		ev := &fakeEvents{err: unroutable}
		h := &orders{stored: map[string][]byte{}, events: ev}
		m := rabbitmq.Message{Queue: "orders", MessageID: "order-1", Body: []byte(`{"total":42}`)}

		err := h.Handle(t.Context(), m)
		require.ErrorIs(t, err, rabbitmq.ErrUnroutable, "under AtLeastOnce the order is delivered again")

		ev.err = nil
		m.Redelivered = true
		require.NoError(t, h.Handle(t.Context(), m))
		require.Len(t, h.stored, 1)
		require.Len(t, ev.published, 1, "the event is published when the order comes again")
	})
}

func TestOrders_fails_an_order_with_no_ID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &fakeEvents{}
		h := &orders{stored: map[string][]byte{}, events: ev}

		err := h.Handle(t.Context(), rabbitmq.Message{Queue: "orders", Body: []byte(`{"total":42}`)})
		require.EqualError(t, err, "an order has no ID")
		require.Empty(t, h.stored)
		require.Empty(t, ev.published)
	})
}

func TestInit_without_its_address_login_and_queue_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		require.EqualError(t, st.Cause(), "init: config RABBITMQ_ADDR: not found\n"+
			"config RABBITMQ_USERNAME: not found\n"+
			"config RABBITMQ_PASSWORD: not found\n"+
			"config RABBITMQ_QUEUE: not found")
	})
}

func TestInit_with_a_URI_for_its_address_exits_78_without_quoting_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		password := rand.Text()
		_, status := humustest.Start(t, Init, humustest.WithEnv(map[string]string{
			"RABBITMQ_ADDR":     "amqp://orders:" + password + "@rabbit:5672/",
			"RABBITMQ_USERNAME": "orders",
			"RABBITMQ_PASSWORD": password,
			"RABBITMQ_QUEUE":    "orders",
		}))

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		cerr, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.Equal(t, "RABBITMQ_ADDR", cerr.Name)
		require.EqualError(t, st.Cause(), "init: config RABBITMQ_ADDR: is a URI, not host:port")
	})
}

func TestInit_waits_for_a_broker_that_is_down_and_exits_0_on_SIGTERM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"RABBITMQ_ADDR":     "rabbit:5672",
				"RABBITMQ_USERNAME": "orders",
				"RABBITMQ_PASSWORD": rand.Text(),
				"RABBITMQ_QUEUE":    "orders",
			}),
		)
		<-p.Notified(syscall.SIGTERM)
		synctest.Wait()
		require.NotEmpty(t, records(tel.Logs(), "connecting failed"), "nothing listens, so it keeps trying")

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

// pemFiles returns, PEM-encoded, the CA's certificate, and cert's chain
// followed by its private key.
func pemFiles(t *testing.T, cert tls.Certificate) (caPEM, certPEM []byte) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[len(cert.Certificate)-1]})
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
	return caPEM, certPEM
}

func TestSecureInit_presents_its_client_certificate_to_the_broker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		caPEM, certPEM := pemFiles(t, ca.Issue(t, "orders-consumer").TLS())
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"etc/rabbitmq/ca.pem":   caPEM,
			"etc/rabbitmq/cert.pem": certPEM,
		}))

		// A broker that requires a client certificate the CA issued, and
		// then never speaks AMQP, so the Service waits on its handshake.
		l, err := h.Listen("tcp", "rabbit:5671")
		require.NoError(t, err)
		defer func() { _ = l.Close() }()
		type hello struct {
			serverName string
			client     string
		}
		hellos := make(chan hello, 1)
		go func() {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			tc := tls.Server(conn, &tls.Config{
				Certificates: []tls.Certificate{ca.Issue(t, "rabbit").TLS()},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    ca.Pool(),
				MinVersion:   tls.VersionTLS12,
			})
			if err := tc.HandshakeContext(context.Background()); err != nil {
				t.Errorf("handshake: %v", err)
				return
			}
			cs := tc.ConnectionState()
			hellos <- hello{serverName: cs.ServerName, client: cs.PeerCertificates[0].Subject.CommonName}
			_, _ = tc.Read(make([]byte, 1)) // until the Service hangs up
		}()

		p, status := humustest.Start(t, secureInit,
			humustest.WithApp(humus.WithHost(h.Host())),
			humustest.WithEnv(map[string]string{
				"RABBITMQ_ADDR":          "rabbit:5671",
				"RABBITMQ_TLS_CA_FILE":   "etc/rabbitmq/ca.pem",
				"RABBITMQ_TLS_CERT_FILE": "etc/rabbitmq/cert.pem",
				"RABBITMQ_QUEUE":         "orders",
			}),
		)
		got := <-hellos
		require.Equal(t, "rabbit", got.serverName, "ServerName defaults to Addr's host")
		require.NotEmpty(t, got.client, "the client presented its certificate")

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func TestSecureInit_without_the_client_certificate_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"etc/rabbitmq/ca.pem": []byte("not a certificate"),
		}))
		_, status := humustest.Start(t, secureInit,
			humustest.WithApp(humus.WithHost(h.Host())),
			humustest.WithEnv(map[string]string{
				"RABBITMQ_ADDR":        "rabbit:5671",
				"RABBITMQ_TLS_CA_FILE": "etc/rabbitmq/ca.pem",
				"RABBITMQ_QUEUE":       "orders",
			}),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		cerr, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.Equal(t, "RABBITMQ_TLS_CERT_FILE", cerr.Name)
	})
}

func TestFromURI_sources_a_Connection_from_one_variable(t *testing.T) {
	password := rand.Text()
	environment := bedrock.NewStaticEnvironment(map[string]string{
		"RABBITMQ_URI":   "amqps://orders:" + password + "@rabbit.internal/shop",
		"RABBITMQ_QUEUE": "orders",
	})
	env := func(name string) config.Value[string] { return config.Env(environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) rabbitmq.Settings {
		k := rabbitmq.DefaultSettings()
		// The address, the vhost, the login and whether to dial TLS, from one variable.
		k.Connection = r.Get(env("RABBITMQ_URI").Parse(fromURI))
		k.Queue = r.Get(env("RABBITMQ_QUEUE").Validate(rabbitmq.CheckQueue))
		return k
	}).Read(t.Context())
	require.NoError(t, err)

	c := settings.Connection
	require.Equal(t, "rabbit.internal:5671", c.Addr, "amqps's port is 5671")
	require.Equal(t, "shop", c.Vhost)
	require.Equal(t, &amqp.PlainAuth{Username: "orders", Password: password}, c.SASL)
	require.NotNil(t, c.TLS, "an amqps URI dials TLS")
	require.Equal(t, rabbitmq.DefaultSettings().Connection.Heartbeat, c.Heartbeat)
	require.NoError(t, rabbitmq.CheckAddr(c.Addr))
}

func TestFromURI_brackets_an_IPv6_host_and_dials_amqp_in_plain_text(t *testing.T) {
	c, err := fromURI("amqp://orders:" + rand.Text() + "@[::1]:5673")
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(c.Addr)
	require.NoError(t, err)
	require.Equal(t, "::1", host)
	require.Equal(t, "5673", port)
	require.Equal(t, "/", c.Vhost)
	require.Nil(t, c.TLS)
}

func TestFromURI_never_quotes_the_URI(t *testing.T) {
	password := rand.Text()
	for _, uri := range []string{
		"http://orders:" + password + "@rabbit:5672/",
		"amqp://rabbit:5672/",
		"amqp://guest:" + password + "@rabbit:5672/",
	} {
		_, err := fromURI(uri)
		require.Error(t, err, uri)
		require.NotContains(t, err.Error(), password)
		require.NotContains(t, err.Error(), "rabbit")
	}
}
