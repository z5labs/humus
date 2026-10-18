// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

// The RabbitMQ page of the docs site quotes the code in this file, and
// TestDocs_code_samples checks that every quote still matches it. The tests
// in init_test.go run Init, secureInit and fromURI, and the orders
// Processor as a unit.

// events is what the orders Processor publishes with: the one method of
// *rabbitmq.Publisher it calls, behind an interface of the end user's own,
// so that its unit tests can fake it.
type events interface {
	Publish(ctx context.Context, m rabbitmq.Publishing) error
}

// orders is the end user's Processor: it stores each order once, by the
// message's ID, so an order delivered again is stored once, and publishes
// an order.accepted event for it.
type orders struct {
	mu     sync.Mutex
	stored map[string][]byte
	events events
}

// Handle stores the order m carries, and publishes an order.accepted event
// for it, with the order's ID as the event's message ID. It publishes on
// every delivery, a redelivery included: an event whose publish failed is
// published when the order comes again, and an event published twice is
// told apart by its ID, as any consumer under queue.AtLeastOnce does.
func (o *orders) Handle(ctx context.Context, m rabbitmq.Message) error {
	if m.MessageID == "" {
		return errors.New("an order has no ID")
	}
	o.store(m)
	// Returns once the broker has confirmed the event, so the order is acked only after.
	return o.events.Publish(ctx, rabbitmq.Publishing{
		Exchange:    "orders",
		RoutingKey:  "order.accepted",
		MessageID:   m.MessageID,
		ContentType: m.ContentType,
		Body:        m.Body,
	})
}

// store stores the order m carries, unless it is stored already. It is
// idempotent, as a Processor under queue.AtLeastOnce must be.
func (o *orders) store(m rabbitmq.Message) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.stored[m.MessageID]; ok {
		return // delivered again: already stored
	}
	o.stored[m.MessageID] = m.Body
}

// Init is the end user's Init: it sources the Settings with config.Struct,
// builds the Publisher and the Processor, and returns the Service from
// NewService.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := rabbitmq.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) rabbitmq.Settings {
		k := d
		// No .Default: the address, the login and the queue are required.
		k.Connection.Addr = r.Get(env("RABBITMQ_ADDR").Validate(rabbitmq.CheckAddr))
		k.Connection.SASL = &amqp.PlainAuth{
			Username: r.Get(env("RABBITMQ_USERNAME")),
			Password: r.Get(env("RABBITMQ_PASSWORD")),
		}
		k.Queue = r.Get(env("RABBITMQ_QUEUE").Validate(rabbitmq.CheckQueue))
		k.Prefetch = r.Get(env("RABBITMQ_PREFETCH").Parse(strconv.Atoi).Default(d.Prefetch).Validate(rabbitmq.CheckPrefetch))
		return k
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	pub, err := rabbitmq.NewPublisher(s, publisherOn(settings.Connection)) // opens no connection: the first Publish does
	if err != nil {
		return nil, err
	}
	h := &orders{stored: map[string][]byte{}, events: pub}

	svc, err := rabbitmq.NewService(s, settings, queue.ProcessorFunc[rabbitmq.Message](h.Handle))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// publisherOn is DefaultPublisherSettings with the consumer's
// Connection: the same broker and the same login, on a connection of the
// Publisher's own.
func publisherOn(c rabbitmq.Connection) rabbitmq.PublisherSettings {
	p := rabbitmq.DefaultPublisherSettings()
	p.Connection = c
	return p
}

// buildTelemetry is the consumer's telemetry.Builder, which names the
// service.
func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "orders-consumer"
	return telemetry.New(ctx, s, dial, p.Stdout)
}

// A consumer's main is one call to humus.Main, whose defaults are a
// consumer's, with an Init that sources the Settings, builds the Publisher
// its Processor publishes events with, and returns the Service from
// rabbitmq.NewService. This example has no Output comment, so it is
// compiled but never run: running it would exit the test binary.
func Example_main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}

// secureInit is an Init for a broker that requires TLS with a client
// certificate, which is also its login. It sources the broker's CA and the
// client's certificate from PEM files, read through s.Host.FS, and
// publishes over the same TLS.
func secureInit(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := rabbitmq.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) rabbitmq.Settings {
		k := d
		k.Connection.Addr = r.Get(env("RABBITMQ_ADDR").Validate(rabbitmq.CheckAddr))
		// The CA's file is read first, and the client's certificate only once it has been.
		k.Connection.TLS = r.Get(env("RABBITMQ_TLS_CA_FILE").Parse(readFile(s.Host.FS)).FlatMap(func(ca []byte) config.Value[*tls.Config] {
			return env("RABBITMQ_TLS_CERT_FILE").Parse(readFile(s.Host.FS)).Parse(clientTLS(ca))
		}))
		// The client certificate is the login: RabbitMQ's rabbitmq_auth_mechanism_ssl plugin takes it.
		k.Connection.SASL = &amqp.ExternalAuth{}
		k.Queue = r.Get(env("RABBITMQ_QUEUE").Validate(rabbitmq.CheckQueue))
		return k
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	pub, err := rabbitmq.NewPublisher(s, publisherOn(settings.Connection))
	if err != nil {
		return nil, err
	}
	h := &orders{stored: map[string][]byte{}, events: pub}
	svc, err := rabbitmq.NewService(s, settings, queue.ProcessorFunc[rabbitmq.Message](h.Handle))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.FS) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

// clientTLS returns a parser for config.Value.Parse that builds the TLS
// config of a client that trusts the CA in caPEM, and presents the
// certificate chain and private key in the PEM file it is given.
func clientTLS(caPEM []byte) func(certPEM []byte) (*tls.Config, error) {
	return func(certPEM []byte) (*tls.Config, error) {
		cert, err := tls.X509KeyPair(certPEM, certPEM)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("holds no PEM certificate")
		}
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      roots,
			MinVersion:   tls.VersionTLS12,
		}, nil
	}
}

// fromURI is a parser for config.Value.Parse that fills DefaultSettings'
// Connection from an AMQP URI, for an end user who keeps one. Its errors
// never quote the URI, which holds the password.
func fromURI(uri string) (rabbitmq.Connection, error) {
	u, err := amqp.ParseURI(uri)
	if err != nil {
		return rabbitmq.Connection{}, errors.New("is not an AMQP URI")
	}
	if u.Username == "guest" {
		// ParseURI fills in guest when the URI has no login, and guest is no login to deploy with.
		return rabbitmq.Connection{}, errors.New("has no login of its own")
	}
	c := rabbitmq.DefaultSettings().Connection
	c.Addr = net.JoinHostPort(u.Host, strconv.Itoa(u.Port)) // brackets an IPv6 host
	c.Vhost = u.Vhost
	c.SASL = u.PlainAuth()
	if u.Scheme == "amqps" {
		// A nil TLS would dial the TLS port, 5671 by default, in plain text.
		c.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return c, nil
}
