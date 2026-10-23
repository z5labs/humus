// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the mailer's Init: its settings, its telemetry, its Publishers, its Service and its probes.
package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/rabbitmq/mailer/internal/certfile"
	"github.com/z5labs/humus/example/rabbitmq/mailer/mail"
	"github.com/z5labs/humus/example/rabbitmq/mailer/provider"
	"github.com/z5labs/humus/example/rabbitmq/mailer/worker"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

const (
	defaultPrefetch    = 16
	defaultConcurrency = 8
	defaultMaxAttempts = 5
)

const scenarioKey = "acme.fault"

func Faults(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
	codec, err := jsonscenario.New(scenarioKey)
	if err != nil {
		return fault.Settings{}, err
	}
	settings := fault.DefaultSettings()
	settings.Decoder = codec
	settings.MaxDelay, err = config.Env(s.Process.Environment, "FAULT_MAX_DELAY").
		Parse(time.ParseDuration).
		Default(settings.MaxDelay).
		Validate(fault.CheckMaxDelay).
		Read(ctx)
	if err != nil {
		return fault.Settings{}, err
	}
	return settings, nil
}

func BuildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("mailer").Validate(telemetry.CheckServiceName))
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

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	retrySettings := settings.Publisher
	retrySettings.ForwardScenario = true
	retries, err := rabbitmq.NewPublisher(s, retrySettings)
	if err != nil {
		return nil, err
	}
	results, err := rabbitmq.NewPublisher(s, settings.Publisher)
	if err != nil {
		return nil, err
	}
	deadLetters, err := rabbitmq.NewPublisher(s, settings.Publisher)
	if err != nil {
		return nil, err
	}
	client, err := provider.New(s, settings.Provider)
	if err != nil {
		return nil, err
	}
	w, err := worker.New(worker.Settings{
		Provider:    client,
		Retries:     retries,
		Results:     results,
		DeadLetters: deadLetters,
		MaxAttempts: settings.MaxAttempts,
		User:        settings.Login,
		Telemetry:   s.Telemetry,
	})
	if err != nil {
		return nil, err
	}

	svc, err := rabbitmq.NewService(s, settings.Consumer, w)
	if err != nil {
		return nil, err
	}
	probes, err := httphealth.NewService(s, settings.Probes, health.Readiness(svc.Consuming()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, probes}, nil
}

type settings struct {
	Consumer          rabbitmq.Settings
	Publisher         rabbitmq.PublisherSettings
	Login             string
	TrustedPublishers []string
	MaxAttempts       int
	Provider          provider.Settings
	Probes            httphealth.Settings
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := readFile(s.Host.FS)

	read, err := config.Struct(func(r *config.Resolver) settings {
		consumer := rabbitmq.DefaultSettings()
		conn := consumer.Connection
		conn.Addr = r.Get(env("RABBITMQ_ADDR").Validate(rabbitmq.CheckAddr))
		conn.Vhost = r.Get(env("RABBITMQ_VHOST").Default(conn.Vhost).Validate(rabbitmq.CheckVhost))
		conn.TLS = r.Get(env("RABBITMQ_CA_FILE").Parse(file).Parse(certPool).Map(trusting).Default(nil))
		login := r.Get(env("RABBITMQ_USERNAME").Validate(notEmpty))
		conn.SASL = &amqp.PlainAuth{
			Username: login,
			Password: r.Get(env("RABBITMQ_PASSWORD_FILE").Parse(file).Parse(secret)),
		}
		consumer.Connection = conn
		consumer.Queue = mail.SendQueue
		consumer.Delivery = queue.AtLeastOnce
		consumer.Prefetch = r.Get(env("RABBITMQ_PREFETCH").Parse(strconv.Atoi).Default(defaultPrefetch).Validate(rabbitmq.CheckPrefetch))
		consumer.Concurrency = r.Get(env("RABBITMQ_CONCURRENCY").Parse(strconv.Atoi).Default(defaultConcurrency).Validate(rabbitmq.CheckConcurrency))

		publisher := rabbitmq.DefaultPublisherSettings()
		publisher.Connection = conn

		client := httpclient.DefaultSettings()
		return settings{
			Consumer:          consumer,
			Publisher:         publisher,
			Login:             login,
			TrustedPublishers: r.Get(env("MAIL_TRUSTED_PUBLISHERS").Parse(logins).Default(nil)),
			MaxAttempts:       r.Get(env("MAIL_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(defaultMaxAttempts).Validate(positive)),
			Provider: provider.Settings{
				URL:            r.Get(env("PROVIDER_URL").Parse(url.Parse).Validate(httpclient.CheckBaseURL)),
				CA:             r.Get(env("PROVIDER_CA_FILE").Parse(file).Parse(certPool).Default(nil)),
				Timeout:        r.Get(env("PROVIDER_TIMEOUT").Parse(time.ParseDuration).Default(client.Timeout).Validate(httpclient.CheckTimeout)),
				AttemptTimeout: r.Get(env("PROVIDER_ATTEMPT_TIMEOUT").Parse(time.ParseDuration).Default(client.AttemptTimeout).Validate(httpclient.CheckAttemptTimeout)),
				MaxAttempts:    r.Get(env("PROVIDER_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(client.MaxAttempts).Validate(httpclient.CheckMaxAttempts)),
				From:           r.Get(env("MAIL_FROM").Validate(notEmpty)),
				APIKey:         r.Get(env("PROVIDER_API_KEY_FILE").Parse(file).Parse(secret)),
			},
			Probes: httphealth.Settings{
				Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
				TLS:  r.Get(env("PROBES_TLS_PEM_FILE").Parse(pemTLS(s.Host.FS)).Validate(httphealth.CheckTLS)),
			},
		}
	}).Read(ctx)
	if err != nil {
		return settings{}, err
	}
	if read.Consumer.Concurrency > read.Consumer.Prefetch {
		return settings{}, &config.Error{
			Name: "RABBITMQ_CONCURRENCY",
			Err:  fmt.Errorf("must be at most RABBITMQ_PREFETCH, %d", read.Consumer.Prefetch),
		}
	}
	read.Consumer.TrustScenario = trustedBy(append([]string{read.Login}, read.TrustedPublishers...))
	return read, nil
}

func logins(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var users []string
	for user := range strings.SplitSeq(s, ",") {
		user = strings.TrimSpace(user)
		if user == "" {
			return nil, errors.New("must be RabbitMQ users separated by commas, with none empty")
		}
		users = append(users, user)
	}
	return users, nil
}

func trustedBy(users []string) func(m rabbitmq.Message) bool {
	trusted := make(map[string]bool, len(users))
	for _, user := range users {
		trusted[user] = true
	}
	return func(m rabbitmq.Message) bool {
		return trusted[m.UserID]
	}
}

func notEmpty(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func positive(n int) error {
	if n < 1 {
		return errors.New("must be positive")
	}
	return nil
}

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

func secret(b []byte) (string, error) {
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", errors.New("the file is empty")
	}
	return s, nil
}

func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}

func trusting(roots *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: roots}
}

func pemTLS(fsys humus.FS) func(name string) (*tls.Config, error) {
	return func(name string) (*tls.Config, error) {
		return certfile.Config(fsys, name, name)
	}
}
