// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the order status processor's Init: its settings, its pool, its Producers, its Service and its probes.
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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/orderstatus/internal/certfile"
	"github.com/z5labs/humus/example/orderstatus/projector"
	"github.com/z5labs/humus/example/orderstatus/store"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

const (
	defaultRetryAttempts = 10
	defaultRetryMinDelay = time.Second
	defaultRetryMaxDelay = 5 * time.Minute
)

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres)
}

type openStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (projector.Store, error)

func initWith(ctx context.Context, s *humus.Setup, open openStore) ([]humus.Service, error) {
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	st, err := open(ctx, s, set.Database)
	if err != nil {
		return nil, err
	}

	retries, err := kafka.NewProducer(s, set.Producer)
	if err != nil {
		return nil, err
	}
	results, err := kafka.NewProducer(s, set.Producer)
	if err != nil {
		return nil, err
	}
	p := projector.New(st, results, retries, results, set.Projector)

	consumer, err := kafka.NewService(s, set.Consumer, p)
	if err != nil {
		return nil, err
	}
	probes, err := httphealth.NewService(s, set.Probes, health.Readiness(consumer.Joined()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{consumer, probes}, nil
}

type settings struct {
	Consumer  kafka.Settings
	Producer  kafka.ProducerSettings
	Projector projector.Settings
	Database  *pgxpool.Config
	Probes    httphealth.Settings
}

type postgres struct {
	URL      *url.URL
	Password string
	CA       *x509.CertPool
}

type sourced struct {
	settings
	KafkaCA  *x509.CertPool
	KafkaTLS *tls.Config
	Postgres postgres
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }
	list := func(v string) []string { return strings.Split(v, ",") }

	got, err := config.Struct(func(r *config.Resolver) sourced {
		consumer := kafka.DefaultSettings()
		consumer.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
		consumer.GroupID = r.Get(env("KAFKA_GROUP_ID").Validate(kafka.CheckGroupID))
		consumer.Delivery = queue.AtLeastOnce
		consumer.SessionTimeout = r.Get(env("KAFKA_SESSION_TIMEOUT").Parse(time.ParseDuration).Default(consumer.SessionTimeout).Validate(kafka.CheckSessionTimeout))

		producer := kafka.DefaultProducerSettings()
		producer.DeliveryTimeout = r.Get(env("KAFKA_DELIVERY_TIMEOUT").Parse(time.ParseDuration).Default(producer.DeliveryTimeout).Validate(kafka.CheckDeliveryTimeout))
		producer.Breaker.OpenFor = r.Get(env("KAFKA_BREAKER_OPEN_FOR").Parse(time.ParseDuration).Default(producer.Breaker.OpenFor).Validate(kafka.CheckBreakerOpenFor))

		return sourced{
			settings: settings{
				Consumer: consumer,
				Producer: producer,
				Projector: projector.Settings{
					EventsTopic:     r.Get(env("KAFKA_EVENTS_TOPIC").Validate(notEmpty)),
					StatusTopic:     r.Get(env("KAFKA_STATUS_TOPIC").Validate(notEmpty)),
					RetryTopic:      r.Get(env("KAFKA_RETRY_TOPIC").Validate(notEmpty)),
					DeadLetterTopic: r.Get(env("KAFKA_DEAD_LETTER_TOPIC").Validate(notEmpty)),
					Attempts:        r.Get(env("RETRY_ATTEMPTS").Parse(strconv.Atoi).Default(defaultRetryAttempts).Validate(positive[int])),
					MinDelay:        r.Get(env("RETRY_MIN_DELAY").Parse(time.ParseDuration).Default(defaultRetryMinDelay).Validate(positive[time.Duration])),
					MaxDelay:        r.Get(env("RETRY_MAX_DELAY").Parse(time.ParseDuration).Default(defaultRetryMaxDelay).Validate(positive[time.Duration])),
				},
				Probes: httphealth.Settings{
					Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
					TLS:  r.Get(env("PROBES_TLS_PEM_FILE").Parse(pemTLS(s.Host.FS)).Validate(httphealth.CheckTLS)),
				},
			},
			KafkaCA:  r.Get(file("KAFKA_TLS_CA_FILE").Parse(certPool)),
			KafkaTLS: r.Get(env("KAFKA_TLS_CERT_FILE").Parse(pemTLS(s.Host.FS))),
			Postgres: postgres{
				URL:      r.Get(env("POSTGRES_URL").Parse(parsePostgresURL).Validate(checkPostgresURL)),
				Password: r.Get(file("POSTGRES_PASSWORD_FILE").Parse(secret)),
				CA:       r.Get(file("POSTGRES_TLS_CA_FILE").Parse(certPool)),
			},
		}
	}).Read(ctx)
	if err != nil {
		return settings{}, err
	}
	if err := checkTopics(got.Projector); err != nil {
		return settings{}, err
	}
	if got.Projector.MinDelay > got.Projector.MaxDelay {
		return settings{}, &config.Error{
			Name: "RETRY_MIN_DELAY",
			Err:  fmt.Errorf("must be at most RETRY_MAX_DELAY, %s", got.Projector.MaxDelay),
		}
	}

	set := got.settings
	set.Consumer.Topics = []string{set.Projector.EventsTopic, set.Projector.RetryTopic}
	kafkaTLS := got.KafkaTLS
	kafkaTLS.RootCAs = got.KafkaCA
	kafkaTLS.MinVersion = tls.VersionTLS12
	set.Consumer.TLS = kafkaTLS
	set.Producer.Brokers = set.Consumer.Brokers
	set.Producer.TLS = kafkaTLS
	set.Database, err = poolConfig(got.Postgres)
	if err != nil {
		return settings{}, err
	}
	return set, nil
}

func checkTopics(p projector.Settings) error {
	topics := []struct{ name, topic string }{
		{"KAFKA_EVENTS_TOPIC", p.EventsTopic},
		{"KAFKA_STATUS_TOPIC", p.StatusTopic},
		{"KAFKA_RETRY_TOPIC", p.RetryTopic},
		{"KAFKA_DEAD_LETTER_TOPIC", p.DeadLetterTopic},
	}
	var errs []error
	for i, t := range topics {
		for _, earlier := range topics[:i] {
			if t.topic == earlier.topic {
				errs = append(errs, &config.Error{Name: t.name, Err: fmt.Errorf("must not be %s's topic, %s", earlier.name, earlier.topic)})
				break
			}
		}
	}
	return errors.Join(errs...)
}

var postgresParams = []string{"password", "passfile", "service", "servicefile", "sslmode", "sslrootcert", "sslcert", "sslkey", "sslpassword"}

func parsePostgresURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, errors.New("must be a URL")
	}
	return u, nil
}

func checkPostgresURL(u *url.URL) error {
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return errors.New("must be a postgres URL")
	}
	if _, ok := u.User.Password(); ok {
		return errors.New("must not hold a password: POSTGRES_PASSWORD_FILE holds it")
	}
	var errs []error
	if u.Hostname() == "" || u.Port() == "" {
		errs = append(errs, errors.New("must have a host and a port"))
	}
	if u.User.Username() == "" {
		errs = append(errs, errors.New("must have a user"))
	}
	if strings.TrimPrefix(u.Path, "/") == "" {
		errs = append(errs, errors.New("must name a database"))
	}
	q := u.Query()
	for _, p := range postgresParams {
		if q.Has(p) {
			errs = append(errs, fmt.Errorf("must not set %s: Init sets it", p))
		}
	}
	return errors.Join(errs...)
}

func poolConfig(p postgres) (*pgxpool.Config, error) {
	u := *p.URL
	u.User = url.UserPassword(p.URL.User.Username(), p.Password)
	q := u.Query()
	q.Set("sslmode", "verify-full")
	for _, name := range []string{"sslrootcert", "sslcert", "sslkey"} {
		q.Set(name, "")
	}
	u.RawQuery = q.Encode()

	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, &config.Error{Name: "POSTGRES_URL", Err: errors.New("must be a PostgreSQL URL that pgx accepts")}
	}
	cfg.ConnConfig.TLSConfig = &tls.Config{
		RootCAs:    p.CA,
		ServerName: u.Hostname(),
		MinVersion: tls.VersionTLS12,
	}
	cfg.ConnConfig.Fallbacks = nil
	return cfg, nil
}

func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (projector.Store, error) {
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
	return store.New(pool), nil
}

func notEmpty(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func positive[T int | time.Duration](v T) error {
	if v <= 0 {
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

func pemTLS(fsys humus.FS) func(name string) (*tls.Config, error) {
	return func(name string) (*tls.Config, error) {
		return certfile.Config(fsys, name, name)
	}
}
