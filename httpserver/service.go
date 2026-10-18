// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal/server"
)

// loggerName is the name of the logger the Service logs through.
const loggerName = "github.com/z5labs/humus/httpserver"

// The ALPN protocol IDs of HTTP/2 and HTTP/1.1.
const (
	protoH2    = "h2"
	protoHTTP1 = "http/1.1"
)

// Service is a humus.Service that serves an http.Handler over TLS. It is
// safe for concurrent use.
type Service struct {
	srv *server.Server
}

// NewService runs every check on settings, reporting every bad field in one
// error that names each one, rejects a nil handler, and listens on
// settings.Addr through s.Host. It registers the listener's close with
// s.OnStop, so a listener whose Service never serves, because Init failed
// after NewService, is still closed.
//
// The Service serves a clone of settings.TLS, so the end user's config is
// never changed. The clone's ALPN protocols are h2, then http/1.1 unless
// settings.HTTP2Only is set, then any others settings.TLS lists.
//
// h is served wrapped in the Service's instrumentation, which takes its
// providers and propagator from s.Telemetry, and which records a panic of h
// and aborts its response.
func NewService(s *humus.Setup, settings Settings, h http.Handler) (*Service, error) {
	if err := checkSettings(settings, h); err != nil {
		return nil, fmt.Errorf("httpserver: NewService: %w", err)
	}

	cfg := settings.TLS.Clone()
	cfg.NextProtos = nextProtos(settings.TLS.NextProtos, settings.HTTP2Only)

	var protocols http.Protocols
	protocols.SetHTTP2(true)
	protocols.SetHTTP1(!settings.HTTP2Only)

	logger := s.Telemetry.LoggerProvider.Logger(loggerName)

	// The operation name is empty, so otelhttp names each span after the
	// method and the request's route pattern, as the semantic conventions
	// do. Every provider and the propagator are passed, so otelhttp never
	// falls back to OpenTelemetry's globals. recordPanics runs inside it, so
	// a panic is recorded on the request's span before the span ends.
	instrumented := otelhttp.NewHandler(recordPanics{next: h, logger: logger}, "",
		otelhttp.WithTracerProvider(s.Telemetry.TracerProvider),
		otelhttp.WithMeterProvider(s.Telemetry.MeterProvider),
		otelhttp.WithPropagators(s.Telemetry.Propagator),
	)

	// The server listens on settings.Addr, and registers the listener's
	// close with s.OnStop. Its error log and its "listening" record go
	// through logger, so they keep this package's scope.
	srv, err := server.New(s, settings.Addr, &http.Server{
		Handler:           instrumented,
		TLSConfig:         cfg,
		ReadHeaderTimeout: settings.ReadHeaderTimeout,
		ReadTimeout:       settings.ReadTimeout,
		WriteTimeout:      settings.WriteTimeout,
		IdleTimeout:       settings.IdleTimeout,
		MaxHeaderBytes:    settings.MaxHeaderBytes,
		Protocols:         &protocols,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("httpserver: NewService: %w", err)
	}
	return &Service{srv: srv}, nil
}

// checkSettings runs every check on settings, and rejects a nil handler. It
// reports every failure in one error, each naming its field.
func checkSettings(settings Settings, h http.Handler) error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	check("Settings.Addr", CheckAddr(settings.Addr))
	check("Settings.TLS", CheckTLS(settings.TLS))
	check("Settings.ReadHeaderTimeout", CheckReadHeaderTimeout(settings.ReadHeaderTimeout))
	check("Settings.ReadTimeout", CheckReadTimeout(settings.ReadTimeout))
	check("Settings.WriteTimeout", CheckWriteTimeout(settings.WriteTimeout))
	check("Settings.IdleTimeout", CheckIdleTimeout(settings.IdleTimeout))
	check("Settings.MaxHeaderBytes", CheckMaxHeaderBytes(settings.MaxHeaderBytes))
	if h == nil {
		errs = append(errs, errors.New("the handler must not be nil"))
	}
	return errors.Join(errs...)
}

// nextProtos returns the ALPN protocols the Service offers: h2, then
// http/1.1 unless http2Only is set, then every other protocol in configured,
// in configured's order.
func nextProtos(configured []string, http2Only bool) []string {
	protos := []string{protoH2}
	if !http2Only {
		protos = append(protos, protoHTTP1)
	}
	for _, p := range configured {
		if p != protoH2 && p != protoHTTP1 {
			protos = append(protos, p)
		}
	}
	return protos
}

// Addr returns the address the Service listens on, with the port the Host
// picked when settings.Addr has port 0.
func (svc *Service) Addr() net.Addr {
	return svc.srv.Addr()
}

// Serve serves until Shutdown. It keeps serving after ctx is done, so the
// Service answers requests through the App's drain. It returns nil once
// Shutdown has stopped it, including a Shutdown called before Serve.
func (svc *Service) Serve(ctx context.Context) error {
	if err := svc.srv.Serve(ctx); err != nil {
		return fmt.Errorf("httpserver: %w", err)
	}
	return nil
}

// Shutdown stops accepting connections, closes idle ones and waits for
// in-flight requests to finish. If ctx is done first, or is done already, it
// closes every connection at once, which cancels the context of each
// in-flight request, and returns an error that wraps ctx's error. Calling it
// again returns nil.
func (svc *Service) Shutdown(ctx context.Context) error {
	if err := svc.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("httpserver: %w", err)
	}
	return nil
}

// emit emits one record at sev, with body and attrs, through logger.
func emit(ctx context.Context, logger log.Logger, sev log.Severity, body string, attrs ...attribute.KeyValue) {
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(sev.String())
	r.SetBody(attribute.StringValue(body))
	r.AddAttributes(attrs...)
	logger.Emit(ctx, r)
}
