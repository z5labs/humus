// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the mailer's Init: its settings, its Publishers, its Service and its probes.
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
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/mailer/internal/certfile"
	"github.com/z5labs/humus/example/mailer/mail"
	"github.com/z5labs/humus/example/mailer/provider"
	"github.com/z5labs/humus/example/mailer/worker"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/rabbitmq"
)

const (
	defaultPrefetch    = 16
	defaultConcurrency = 8
	defaultMaxAttempts = 5
)

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	retries, err := rabbitmq.NewPublisher(s, settings.Publisher)
	if err != nil {
		return nil, err
	}
	results, err := rabbitmq.NewPublisher(s, settings.Publisher)
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
		MaxAttempts: settings.MaxAttempts,
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
	Consumer    rabbitmq.Settings
	Publisher   rabbitmq.PublisherSettings
	MaxAttempts int
	Provider    provider.Settings
	Probes      httphealth.Settings
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
		conn.SASL = &amqp.PlainAuth{
			Username: r.Get(env("RABBITMQ_USERNAME").Validate(notEmpty)),
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
			Consumer:    consumer,
			Publisher:   publisher,
			MaxAttempts: r.Get(env("MAIL_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(defaultMaxAttempts).Validate(positive)),
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
	return read, nil
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
