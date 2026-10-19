// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the billing job's Init, its settings and its telemetry.
package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/billing/billing"
	"github.com/z5labs/humus/example/billing/payments"
	"github.com/z5labs/humus/example/billing/postgres"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

func Options() []humus.Option {
	return []humus.Option{
		humus.WithTelemetry(buildTelemetry),
	}
}

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres)
}

type openStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (billing.Invoices, error)

func initWith(ctx context.Context, s *humus.Setup, open openStore) ([]humus.Service, error) {
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	invoices, err := open(ctx, s, set.Database)
	if err != nil {
		return nil, err
	}

	client, err := payments.NewClient(s, set.Payments, set.APIKey)
	if err != nil {
		return nil, err
	}

	run := billing.Run{
		Invoices:    invoices,
		Payments:    client,
		Period:      set.Period,
		Concurrency: set.Concurrency,
	}
	return []humus.Service{job.NewService(s, run.Run)}, nil
}

type settings struct {
	Database    *pgxpool.Config
	Payments    httpclient.Settings
	APIKey      string
	Period      time.Time
	Concurrency int
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }

	return config.Struct(func(r *config.Resolver) settings {
		client := httpclient.DefaultSettings()
		client.BaseURL = r.Get(env("PAYMENTS_URL").Parse(url.Parse).Validate(checkPaymentsURL))
		client.TLS = r.Get(file("PAYMENTS_CA_FILE").Parse(certPool).Default(nil).Map(func(roots *x509.CertPool) *tls.Config {
			return &tls.Config{RootCAs: roots}
		}).Validate(httpclient.CheckTLS))
		client.Timeout = r.Get(env("PAYMENTS_TIMEOUT").Parse(time.ParseDuration).Default(client.Timeout).Validate(httpclient.CheckTimeout))
		client.AttemptTimeout = r.Get(env("PAYMENTS_ATTEMPT_TIMEOUT").Parse(time.ParseDuration).Default(client.AttemptTimeout).Validate(httpclient.CheckAttemptTimeout))
		client.MaxAttempts = r.Get(env("PAYMENTS_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(client.MaxAttempts).Validate(httpclient.CheckMaxAttempts))

		return settings{
			Database:    r.Get(file("DATABASE_URL_FILE").Parse(databaseURL)),
			Payments:    client,
			APIKey:      r.Get(file("PAYMENTS_API_KEY_FILE").Parse(apiKey)),
			Period:      r.Get(env("BILLING_PERIOD").Parse(parseMonth).Default(lastMonth(time.Now())).Validate(checkEnded)),
			Concurrency: r.Get(env("BILLING_CONCURRENCY").Parse(strconv.Atoi).Default(4).Validate(checkConcurrency)),
		}
	}).Read(ctx)
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

func databaseURL(b []byte) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(string(bytes.TrimSpace(b)))
	if err != nil {
		return nil, errors.New("must hold a PostgreSQL URL")
	}
	return cfg, nil
}

func apiKey(b []byte) (string, error) {
	key := string(bytes.TrimSpace(b))
	if key == "" {
		return "", errors.New("must hold the API key")
	}
	return key, nil
}

func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}

func checkPaymentsURL(u *url.URL) error {
	if err := httpclient.CheckBaseURL(u); err != nil {
		return err
	}
	if u.Scheme != "https" {
		return errors.New("must be an https URL")
	}
	return nil
}

func parseMonth(s string) (time.Time, error) {
	m, err := time.Parse("2006-01", s)
	if err != nil {
		return time.Time{}, errors.New("must be a month, YYYY-MM")
	}
	return m, nil
}

func lastMonth(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
}

func checkEnded(month time.Time) error {
	if month.AddDate(0, 1, 0).After(time.Now()) {
		return fmt.Errorf("%s hasn't ended", month.Format("2006-01"))
	}
	return nil
}

func checkConcurrency(n int) error {
	if n < 1 || n > 32 {
		return errors.New("must be from 1 to 32")
	}
	return nil
}

func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (billing.Invoices, error) {
	cfg.ConnConfig.DialFunc = s.Host.Dial
	cfg.ConnConfig.LookupFunc = func(_ context.Context, host string) ([]string, error) {
		return []string{host}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s.OnStop(func(context.Context) error {
		pool.Close()
		return nil
	})
	return postgres.New(pool), nil
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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("billing").Validate(telemetry.CheckServiceName))
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
