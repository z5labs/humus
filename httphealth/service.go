// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/internal/server"
)

// protoHTTP1 is the ALPN protocol ID of HTTP/1.1, the only one the Service
// offers.
const protoHTTP1 = "http/1.1"

// The server's fixed limits. The two read timeouts bound a probe's request,
// which has no body. There is no write timeout: the probe's own timeout
// bounds a check. The idle timeout and the header limit are httpserver's
// defaults.
const (
	readHeaderTimeout = 2 * time.Second
	readTimeout       = 5 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 1 << 20
)

// Service is a humus.Service that serves NewHandler's handler, and nothing
// else, over HTTP/1.1 and TLS on a port of its own. It owns the App's
// readiness as a health.Service does, so the end user's Init returns no
// health Service next to it. It is safe for concurrent use.
type Service struct {
	hs  *health.Service
	srv *server.Server
}

// NewService checks settings and opts, reporting every problem in one
// error, and listens on settings.Addr through s.Host. It registers the
// listener's close with s.OnStop, so a listener whose Service never serves,
// because Init failed after NewService, is still closed.
//
// The Service answers from the probes of a health.Service built from opts:
//
//   - readiness is false until Serve starts, false for good once Serve's
//     context is done or Shutdown is called, and otherwise checks the
//     monitors health.Readiness adds, in the order they were added;
//   - liveness checks the monitors health.Liveness adds, in the order they
//     were added, and is healthy when there are none.
//
// NewService rejects a nil Option.
//
// The Service serves a clone of settings.TLS whose only ALPN protocol is
// http/1.1, so the end user's config is never changed, and it speaks
// HTTP/1.1 alone. It reads a request's headers within 2s and the whole
// request within 5s, and sets no write timeout. It records no span and no
// metric, and extracts no trace context.
func NewService(s *humus.Setup, settings Settings, opts ...health.Option) (*Service, error) {
	if err := checkSettings(settings, opts); err != nil {
		return nil, fmt.Errorf("httphealth: NewService: %w", err)
	}

	hs := health.NewService(opts...)

	cfg := settings.TLS.Clone()
	cfg.NextProtos = []string{protoHTTP1}

	var protocols http.Protocols
	protocols.SetHTTP1(true)

	// The handler is served as it is: no otelhttp, so a probe records no
	// span and no metric, and no trace context is extracted. The server's
	// error log and its "listening" record go through the handler's logger,
	// so they share this package's scope.
	srv, err := server.New(s, "httphealth", settings.Addr, &http.Server{
		Handler:           NewHandler(s, hs.Probes()),
		TLSConfig:         cfg,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		Protocols:         &protocols,
	}, s.Telemetry.LoggerProvider.Logger(loggerName))
	if err != nil {
		return nil, fmt.Errorf("httphealth: NewService: %w", err)
	}
	return &Service{hs: hs, srv: srv}, nil
}

// checkSettings runs every check on settings, and rejects a nil Option,
// naming its index. It reports every failure in one error, each naming its
// field.
func checkSettings(settings Settings, opts []health.Option) error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	check("Settings.Addr", CheckAddr(settings.Addr))
	check("Settings.TLS", CheckTLS(settings.TLS))
	for i, opt := range opts {
		if opt == nil {
			errs = append(errs, fmt.Errorf("opts[%d] must not be nil", i))
		}
	}
	return errors.Join(errs...)
}

// Addr returns the address the Service listens on, with the port the Host
// picked when settings.Addr has port 0.
func (svc *Service) Addr() net.Addr {
	return svc.srv.Addr()
}

// Serve marks the Service ready, and serves until Shutdown. It keeps
// serving after ctx is done, so the probes are answered, not ready, through
// the App's drain. It returns nil once Shutdown has stopped it, including a
// Shutdown called before Serve.
func (svc *Service) Serve(ctx context.Context) error {
	// The health Service runs with Serve's context, so readiness turns false
	// for good once ctx is done, while the server goes on answering.
	readiness := make(chan struct{})
	go func() {
		defer close(readiness)
		_ = svc.hs.Serve(ctx) // it always returns nil
	}()

	err := svc.srv.Serve(ctx)

	// The server has stopped, after Shutdown or on an error of its own: the
	// Service is not ready for good either way, and the health Service's
	// Serve returns before this one does.
	svc.stopReadiness()
	<-readiness
	return err
}

// Shutdown marks the Service not ready for good, stops accepting
// connections and waits for the probes in flight. If ctx is done first, or
// is done already, it closes every connection at once and returns an error
// that wraps ctx's error. Calling it again returns nil.
func (svc *Service) Shutdown(ctx context.Context) error {
	svc.stopReadiness()
	return svc.srv.Shutdown(ctx)
}

// stopReadiness marks the Service not ready for good, and stops its health
// Service's Serve, if it is running.
func (svc *Service) stopReadiness() {
	// The health Service's Serve returns as soon as it is stopped, so its
	// Shutdown never waits for long, and with a context that is never done
	// it returns no error.
	_ = svc.hs.Shutdown(context.Background())
}
