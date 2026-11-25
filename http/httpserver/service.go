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

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal/httpchain"
	"github.com/z5labs/humus/internal/server"
)

// loggerName is the name of the logger the Service logs through.
const loggerName = "github.com/z5labs/humus/http/httpserver"

// The ALPN protocol IDs of HTTP/2 and HTTP/1.1.
const (
	protoH2    = "h2"
	protoHTTP1 = "http/1.1"
)

// Service is a humus.Service that serves an http.Handler over TLS. It is
// safe for concurrent use.
type Service struct {
	srv *server.Server

	// shuttingDown closes the channel ShuttingDown returns for the
	// Service's requests.
	shuttingDown func()
}

// Network is what a Service needs of a host: a way to listen. humus.Host
// implements it, and so does any network of the end user's with this
// method.
type Network interface {
	// ListenTCP announces on the local network address, for "tcp", "tcp4"
	// or "tcp6", like net.ListenConfig.Listen.
	ListenTCP(ctx context.Context, network, address string) (net.Listener, error)
}

// NewService runs every check on settings, reporting every bad field in one
// error that names each one, rejects a nil handler, and listens on
// settings.Addr through n, with ctx, which bounds the lookup of the
// address's host. A failure to listen, such as EADDRINUSE, is its error, so
// it fails the end user's Init. It registers the listener's close with
// s.OnStop, so a listener whose Service never serves, because Init failed
// after NewService, is still closed.
//
// The Service serves a clone of settings.TLS, so the end user's config is
// never changed. The clone's ALPN protocols are h2, then http/1.1 unless
// settings.HTTP2Only is set, then any others settings.TLS lists.
//
// h is served wrapped in the Service's instrumentation, which takes its
// providers and propagator from s.Telemetry, and which records a panic of h
// and aborts its response. Before h runs, the failure scenario a request
// carries is stripped with s.Faults, unless settings.TrustScenario trusts
// the caller.
func NewService(ctx context.Context, s *humus.Setup, n Network, settings Settings, h http.Handler) (*Service, error) {
	if err := checkSettings(settings, h); err != nil {
		return nil, fmt.Errorf("httpserver: NewService: %w", err)
	}

	cfg := settings.TLS.Clone()
	cfg.NextProtos = nextProtos(settings.TLS.NextProtos, settings.HTTP2Only)

	var protocols http.Protocols
	protocols.SetHTTP2(true)
	protocols.SetHTTP1(!settings.HTTP2Only)

	logger := s.Telemetry.LoggerProvider.Logger(loggerName)

	// The handler chain is the one every humus HTTP server serves through.
	// Its records go through logger, so they keep this package's scope.
	instrumented := httpchain.Wrap(s, logger, settings.TrustScenario, h)

	hs := &http.Server{
		Handler:           instrumented,
		TLSConfig:         cfg,
		ReadHeaderTimeout: settings.ReadHeaderTimeout,
		ReadTimeout:       settings.ReadTimeout,
		WriteTimeout:      settings.WriteTimeout,
		IdleTimeout:       settings.IdleTimeout,
		MaxHeaderBytes:    settings.MaxHeaderBytes,
		Protocols:         &protocols,
	}
	shuttingDown := watchShutdown(hs)

	// The server listens on settings.Addr, and registers the listener's
	// close with s.OnStop. Its error log and its "listening" record go
	// through logger, so they keep this package's scope.
	srv, err := server.New(ctx, s, n, "httpserver", settings.Addr, hs, logger)
	if err != nil {
		return nil, fmt.Errorf("httpserver: NewService: %w", err)
	}
	return &Service{srv: srv, shuttingDown: shuttingDown}, nil
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

// Addr returns the address the Service listens on, with the port the
// Network picked when settings.Addr has port 0.
func (svc *Service) Addr() net.Addr {
	return svc.srv.Addr()
}

// Serve serves until Shutdown. It keeps serving after ctx is done, so the
// Service answers requests through the App's drain. It returns nil once
// Shutdown has stopped it, including a Shutdown called before Serve.
func (svc *Service) Serve(ctx context.Context) error {
	return svc.srv.Serve(ctx)
}

// Shutdown stops accepting connections, closes the channel [ShuttingDown]
// returns for the Service's requests, closes idle connections and waits
// for in-flight requests to finish. If ctx is done first, or is done
// already, it closes every connection at once, which cancels the context of
// each in-flight request, and returns an error that wraps ctx's error. The
// channel is closed either way. Calling it again returns nil.
func (svc *Service) Shutdown(ctx context.Context) error {
	if ctx.Err() != nil {
		// The server is closed without its Shutdown, which is what runs
		// the function that closes the channel.
		svc.shuttingDown()
	}
	return svc.srv.Shutdown(ctx)
}
