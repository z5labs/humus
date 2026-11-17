// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the polls service's Init: its settings, its store, its listener, its Site and its probes.
package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/polls/internal/certfile"
	"github.com/z5labs/humus/example/http/polls/internal/latch"
	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/example/http/polls/ui"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/telemetry"
)

const loggerName = "github.com/z5labs/humus/example/http/polls"

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres)
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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("polls").Validate(telemetry.CheckServiceName))
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

type backend struct {
	store     ui.Store
	results   ui.Results
	listen    func(context.Context)
	reached   func(context.Context) error
	listening func(context.Context) error
}

type openBackend func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (backend, error)

func initWith(ctx context.Context, s *humus.Setup, open openBackend) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	b, err := open(ctx, s, settings.Database)
	if err != nil {
		return nil, err
	}

	u, err := ui.New(b.store, b.results, settings.HTTP.WriteTimeout)
	if err != nil {
		return nil, err
	}
	site, err := u.Site(s)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/health/", httphealth.NewHandler(s, health.Probes{
		Liveness:  health.And(),
		Readiness: health.And(latch.New(b.reached), latch.New(b.listening)),
	}))
	mux.Handle("/", site)

	svc, err := httpserver.NewService(s, settings.HTTP, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{newListener(b.listen), svc}, nil
}

func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (backend, error) {
	cfg.ConnConfig.DialFunc = s.Host.Dial
	cfg.ConnConfig.LookupFunc = func(_ context.Context, host string) ([]string, error) {
		return []string{host}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return backend{}, err
	}
	s.OnStop(func(context.Context) error {
		pool.Close()
		return nil
	})
	polls := store.New(pool)
	l := store.NewListener(cfg.ConnConfig.Copy(), polls, s.Telemetry.LoggerProvider.Logger(loggerName))
	return backend{
		store:     polls,
		results:   l,
		listen:    l.Run,
		reached:   pool.Ping,
		listening: l.Listening,
	}, nil
}

type listener struct {
	run  func(context.Context)
	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}

	mu      sync.Mutex
	serving bool
}

func newListener(run func(context.Context)) *listener {
	ctx, stop := context.WithCancel(context.Background())
	return &listener{run: run, ctx: ctx, stop: stop, done: make(chan struct{})}
}

func (l *listener) Serve(context.Context) error {
	l.mu.Lock()
	if l.serving || l.ctx.Err() != nil {
		l.mu.Unlock()
		return nil
	}
	l.serving = true
	l.mu.Unlock()

	defer close(l.done)
	l.run(l.ctx)
	return nil
}

func (l *listener) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	l.stop()
	serving := l.serving
	l.mu.Unlock()
	if !serving {
		return nil
	}
	select {
	case <-l.done:
		return nil
	default:
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("stopping the listener: %w", err)
	}
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stopping the listener: %w", ctx.Err())
	}
}

type settings struct {
	HTTP     httpserver.Settings
	Database *pgxpool.Config
}

type serverCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error)

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }
	timeout := func(name string, def time.Duration, check func(time.Duration) error) config.Value[time.Duration] {
		return env(name).Parse(time.ParseDuration).Default(def).Validate(check)
	}

	return config.Struct(func(r *config.Resolver) settings {
		server := d
		server.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		server.TLS = &tls.Config{
			GetCertificate: r.Get(env("TLS_CERT_FILE").Validate(holdsCertificate(s.Host.FS)).FlatMap(func(certFile string) config.Value[serverCertificate] {
				return env("TLS_KEY_FILE").Parse(func(keyFile string) (serverCertificate, error) {
					cfg, err := certfile.Config(s.Host.FS, certFile, keyFile)
					if err != nil {
						return nil, err
					}
					return cfg.GetCertificate, nil
				})
			})),
		}
		server.ReadHeaderTimeout = r.Get(timeout("READ_HEADER_TIMEOUT", d.ReadHeaderTimeout, httpserver.CheckReadHeaderTimeout))
		server.ReadTimeout = r.Get(timeout("READ_TIMEOUT", d.ReadTimeout, httpserver.CheckReadTimeout))
		server.WriteTimeout = r.Get(timeout("WRITE_TIMEOUT", d.WriteTimeout, httpserver.CheckWriteTimeout))
		server.IdleTimeout = r.Get(timeout("IDLE_TIMEOUT", d.IdleTimeout, httpserver.CheckIdleTimeout))
		return settings{
			HTTP:     server,
			Database: r.Get(file("DATABASE_URL_FILE").Parse(databaseURL)),
		}
	}).Read(ctx)
}

func holdsCertificate(fsys humus.FS) func(name string) error {
	return func(name string) error {
		b, err := readFile(fsys)(name)
		if err != nil {
			return err
		}
		for {
			var block *pem.Block
			block, b = pem.Decode(b)
			if block == nil {
				return errors.New("no PEM certificate in the file")
			}
			if block.Type == "CERTIFICATE" {
				return nil
			}
		}
	}
}

func databaseURL(b []byte) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(string(bytes.TrimSpace(b)))
	if err != nil {
		return nil, errors.New("must hold a PostgreSQL URL")
	}
	return cfg, nil
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
