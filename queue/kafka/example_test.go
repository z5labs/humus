// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/scram"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

// The Consuming Kafka page of the docs site quotes the code in this file,
// and TestDocs_code_samples checks that every quote still matches it.
// TestInit_processes_and_commits_a_produced_record runs Init, and
// TestSecureInit_consumes_over_mTLS_with_SASL runs secureInit.

// orders is the end user's Processor: it stores each order once, by the
// record's key, so an order delivered again is skipped.
type orders struct {
	mu     sync.Mutex
	stored map[string]bool
	out    io.Writer
}

// Handle stores the order m carries, unless it is stored already. It is
// idempotent, as a Processor under queue.AtLeastOnce must be.
func (o *orders) Handle(ctx context.Context, m kafka.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	id := string(m.Key)
	if o.stored[id] {
		return nil // delivered again: already stored
	}
	o.stored[id] = true
	_, err := fmt.Fprintf(o.out, "stored order %s: %s\n", id, m.Value)
	return err
}

// Init is the end user's Init: it sources the Settings with config.Struct,
// builds the Processor, and returns the Service from NewService.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := kafka.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	list := func(v string) []string { return strings.Split(v, ",") }

	settings, err := config.Struct(func(r *config.Resolver) kafka.Settings {
		k := d
		// No .Default: Brokers, GroupID and Topics are required, so an unset variable fails as not found.
		k.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
		k.GroupID = r.Get(env("KAFKA_GROUP_ID").Validate(kafka.CheckGroupID))
		k.Topics = r.Get(env("KAFKA_TOPICS").Map(list).Validate(kafka.CheckTopics))
		k.SessionTimeout = r.Get(env("KAFKA_SESSION_TIMEOUT").Parse(time.ParseDuration).Default(d.SessionTimeout).Validate(kafka.CheckSessionTimeout))
		return k
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	h := &orders{stored: map[string]bool{}, out: s.Process.Stdout}
	svc, err := kafka.NewService(s, settings, queue.ProcessorFunc[kafka.Message](h.Handle))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// buildTelemetry is the consumer's telemetry.Builder, which names the
// service.
func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "orders-consumer"
	return telemetry.New(ctx, s, dial, p.Stdout)
}

// A consumer's main is one call to humus.Main, whose defaults are a
// consumer's, with an Init that returns the Service from kafka.NewService.
// This example has no Output comment, so it is compiled but never run:
// running it would exit the test binary.
func Example_main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}

// secureInit is an Init for brokers that require TLS with a client
// certificate, and SASL. It sources the brokers' CA and the client's
// certificate from PEM files, read through s.Host.FS, and SCRAM-SHA-512
// credentials.
func secureInit(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := kafka.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	list := func(v string) []string { return strings.Split(v, ",") }

	settings, err := config.Struct(func(r *config.Resolver) kafka.Settings {
		k := d
		k.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
		k.GroupID = r.Get(env("KAFKA_GROUP_ID").Validate(kafka.CheckGroupID))
		k.Topics = r.Get(env("KAFKA_TOPICS").Map(list).Validate(kafka.CheckTopics))
		// The CA's file is read first, and the client's certificate only once it has been.
		k.TLS = r.Get(env("KAFKA_TLS_CA_FILE").Parse(readFile(s.Host.FS)).FlatMap(func(ca []byte) config.Value[*tls.Config] {
			return env("KAFKA_TLS_CERT_FILE").Parse(readFile(s.Host.FS)).Parse(clientTLS(ca))
		}))
		k.SASL = r.Get(env("KAFKA_SASL_USER").FlatMap(func(user string) config.Value[sasl.Mechanism] {
			return env("KAFKA_SASL_PASSWORD").Map(func(password string) sasl.Mechanism {
				return scram.Auth{User: user, Pass: password}.AsSha512Mechanism()
			})
		}))
		return k
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	h := &orders{stored: map[string]bool{}, out: s.Process.Stdout}
	svc, err := kafka.NewService(s, settings, queue.ProcessorFunc[kafka.Message](h.Handle))
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

// twoDeliveries is an Init that consumes two topics with different
// Delivery values: two Services, each with a group ID of its own, returned
// together. Either one returning ends the App.
func twoDeliveries(brokers []string, p queue.ProcessorFunc[kafka.Message]) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		payments := kafka.DefaultSettings()
		payments.Brokers = brokers
		payments.GroupID = "payments-consumer"
		payments.Topics = []string{"payments"}

		clicks := kafka.DefaultSettings()
		clicks.Brokers = brokers
		clicks.GroupID = "clicks-consumer"
		clicks.Topics = []string{"clicks"}
		clicks.Delivery = queue.AtMostOnce // a click lost to a crash is not worth a second count

		paymentsSvc, err := kafka.NewService(s, payments, p)
		if err != nil {
			return nil, err
		}
		clicksSvc, err := kafka.NewService(s, clicks, p)
		if err != nil {
			return nil, err
		}
		return []humus.Service{paymentsSvc, clicksSvc}, nil
	}
}
