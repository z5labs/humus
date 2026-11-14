// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
	"github.com/z5labs/humus/telemetry"
)

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	store := newStore()
	e := newServer(store)
	e.Logger = slog.New(slog.NewJSONHandler(s.Process.Stdout, nil))
	e.Use(route)

	api, err := openapi.NewAPI(s,
		openapi.Info{Title: "Users", Version: "1.0.0"},
		openapi.Unmatched{
			NotFound:         e,
			MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
		},
		openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
		openapi.CatchAll(http.StatusInternalServerError, serverError, jsoncodec.Problem),
		(&users{store: store, validate: validator.New()}).operations(),
	)
	if err != nil {
		return nil, err
	}

	hs := health.NewService()
	mux := http.NewServeMux()
	mux.Handle("/", http.MaxBytesHandler(api, maxBodyBytes))
	mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))

	svc, err := httpserver.NewService(s, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, hs}, nil
}

func route(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Request().Pattern = c.Path()
		return next(c)
	}
}

func readSettings(ctx context.Context, s *humus.Setup) (httpserver.Settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	return config.Struct(func(r *config.Resolver) httpserver.Settings {
		settings := httpserver.DefaultSettings()
		settings.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		settings.TLS = r.Get(env("TLS_CERT_FILE").FlatMap(func(certFile string) config.Value[*tls.Config] {
			return env("TLS_KEY_FILE").Parse(serverTLS(s.Host.FS, certFile))
		}).Validate(httpserver.CheckTLS))
		return settings
	}).Read(ctx)
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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("users").Validate(telemetry.CheckServiceName))
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
