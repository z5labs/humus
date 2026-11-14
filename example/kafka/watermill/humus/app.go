// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

const (
	consumerGroup = "invoicer"

	ordersTopic     = "orders"
	invoicesTopic   = "invoices"
	retryTopic      = "orders.retry"
	deadLetterTopic = "orders.poison"

	defaultRetryAttempts = 4
	defaultRetryDelay    = 10 * time.Second
)

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	producer, err := kafka.NewProducer(s, settings.Producer)
	if err != nil {
		return nil, err
	}
	consumer, err := kafka.NewService(s, settings.Consumer, &invoicer{producer: producer, retry: settings.Retry})
	if err != nil {
		return nil, err
	}
	probes, err := httphealth.NewService(s, settings.Probes, health.Readiness(consumer.Joined()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{consumer, probes}, nil
}

type settings struct {
	Consumer kafka.Settings
	Producer kafka.ProducerSettings
	Retry    retry
	Probes   httphealth.Settings
}

type retry struct {
	Attempts int
	Delay    time.Duration
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	list := func(v string) []string { return strings.Split(v, ",") }

	return config.Struct(func(r *config.Resolver) settings {
		consumer := kafka.DefaultSettings()
		consumer.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
		consumer.GroupID = consumerGroup
		consumer.Topics = []string{ordersTopic, retryTopic}
		consumer.Delivery = queue.AtLeastOnce

		producer := kafka.DefaultProducerSettings()
		producer.Brokers = consumer.Brokers

		return settings{
			Consumer: consumer,
			Producer: producer,
			Retry: retry{
				Attempts: r.Get(env("RETRY_ATTEMPTS").Parse(strconv.Atoi).Default(defaultRetryAttempts).Validate(positive[int])),
				Delay:    r.Get(env("RETRY_DELAY").Parse(time.ParseDuration).Default(defaultRetryDelay).Validate(positive[time.Duration])),
			},
			Probes: httphealth.Settings{
				Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
				TLS: r.Get(env("PROBES_TLS_CERT_FILE").FlatMap(func(certFile string) config.Value[*tls.Config] {
					return env("PROBES_TLS_KEY_FILE").Parse(serverTLS(s.Host.FS, certFile))
				}).Validate(httphealth.CheckTLS)),
			},
		}
	}).Read(ctx)
}

func positive[T int | time.Duration](v T) error {
	if v <= 0 {
		return errors.New("must be positive")
	}
	return nil
}

func serverTLS(fsys humus.FS, certFile string) func(keyFile string) (*tls.Config, error) {
	return func(keyFile string) (*tls.Config, error) {
		certPEM, err := readFile(fsys, certFile)
		if err != nil {
			return nil, err
		}
		keyPEM, err := readFile(fsys, keyFile)
		if err != nil {
			return nil, err
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
	}
}

func readFile(fsys humus.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	d := telemetry.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(p.Environment, name) }
	exporter := func(name string, def telemetry.Exporter) config.Value[telemetry.Exporter] {
		return env(name).
			Map(func(s string) telemetry.Exporter { return telemetry.Exporter(s) }).
			Default(def).
			Validate(telemetry.CheckExporter)
	}

	s, err := config.Struct(func(r *config.Resolver) telemetry.Settings {
		s := d
		s.Disabled = r.Get(env("OTEL_SDK_DISABLED").Parse(strconv.ParseBool).Default(d.Disabled))
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("invoicer").Validate(telemetry.CheckServiceName))
		s.Traces = r.Get(exporter("OTEL_TRACES_EXPORTER", d.Traces))
		s.Metrics = r.Get(exporter("OTEL_METRICS_EXPORTER", d.Metrics))
		s.Logs = r.Get(exporter("OTEL_LOGS_EXPORTER", d.Logs))
		s.OTLP.Endpoint = r.Get(env("OTEL_EXPORTER_OTLP_ENDPOINT").Default(d.OTLP.Endpoint).Validate(telemetry.CheckOTLPEndpoint))
		return s
	}).Read(ctx)
	if err != nil {
		return telemetry.SDK{}, err
	}
	return telemetry.New(ctx, s, dial, p.Stdout)
}
