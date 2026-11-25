// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal/httpchain"
)

// loggerName is the name of the logger the Service logs through: the
// package's import path, as every humus package's scope is.
const loggerName = "github.com/z5labs/humus/http3/http3server"

// Network is what a Service needs of a host: a way to bind a UDP socket.
// humus.Host implements it, and so does any network of the end user's with
// this method.
type Network interface {
	// ListenUDP binds a UDP socket to the local network address, for "udp",
	// "udp4" or "udp6", like net.ListenConfig.ListenPacket.
	ListenUDP(ctx context.Context, network, address string) (net.PacketConn, error)
}

// Service is a humus.Service that serves an http.Handler over HTTP/3. It
// is safe for concurrent use.
type Service struct {
	sock   net.PacketConn
	srv    *http3.Server
	drain  *drain
	logger log.Logger

	// shutdown is set by the first Shutdown.
	shutdown atomic.Bool
}

// NewService runs every check on settings, naming every bad field in one
// error, and rejects a nil handler. It binds settings.Addr through n, with
// ctx, which bounds the lookup of the address's host. A failure to bind,
// such as EADDRINUSE, is its error, so it fails the end user's Init. It
// registers the socket's close with s.OnStop, the only close the socket
// gets: quic-go never closes a socket it is handed, so a socket whose
// Service never serves, because Init failed after NewService, is closed
// all the same.
//
// ReadHeaderTimeout, or ReadTimeout when it is zero, bounds each request's
// headers: a stream whose headers miss it is reset with
// H3_REQUEST_INCOMPLETE, and no handler runs. IdleTimeout, or ReadTimeout
// when it is zero, closes a connection with no request in flight.
// ReadTimeout and WriteTimeout bound each request's stream from the end of
// its headers: once ReadTimeout has passed, a read of the body fails, and
// when WriteTimeout passes, the request's context is cancelled and its
// stream is reset with H3_INTERNAL_ERROR once the handler returns. A
// handler moves either deadline with http.ResponseController. Half the
// smallest of ReadHeaderTimeout, ReadTimeout and WriteTimeout that is set,
// or 5s when none is, bounds the QUIC handshake. MaxHeaderBytes bounds a
// request's headers. 0-RTT is never accepted.
//
// h is served wrapped in the same instrumentation, route naming, panic
// recording and scenario stripping as httpserver's, with s.Telemetry's
// providers and s.Faults. Before h runs, the failure scenario a request
// carries is stripped, unless settings.TrustScenario trusts the caller.
func NewService(ctx context.Context, s *humus.Setup, n Network, settings Settings, h http.Handler) (*Service, error) {
	if err := checkSettings(settings, h); err != nil {
		return nil, fmt.Errorf("http3server: NewService: %w", err)
	}

	sock, err := n.ListenUDP(ctx, "udp", settings.Addr)
	if err != nil {
		return nil, fmt.Errorf("http3server: NewService: %w", err)
	}
	s.OnStop(func(context.Context) error {
		if err := sock.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("http3server: closing the socket: %w", err)
		}
		return nil
	})

	logger := s.Telemetry.LoggerProvider.Logger(loggerName)
	d := newDrain()
	srv := newServer(settings, d.track(bound(settings, httpchain.Wrap(s, logger, settings.TrustScenario, h))), logger)
	srv.ConnContext = d.connContext
	return &Service{sock: sock, srv: srv, drain: d, logger: logger}, nil
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

// Addr returns the address the socket is bound to, with the port the
// Network picked when settings.Addr has port 0.
func (svc *Service) Addr() net.Addr {
	return svc.sock.LocalAddr()
}

// Serve logs "listening" at INFO, with the address as server.address, and
// serves HTTP/3 on the socket until Shutdown. It keeps serving after ctx is
// done, so the Service answers requests through the App's drain. It
// returns nil once Shutdown has stopped it, including a Shutdown called
// before Serve.
func (svc *Service) Serve(context.Context) error {
	emit(context.Background(), svc.logger, log.SeverityInfo, "listening",
		attribute.String("server.address", svc.Addr().String()),
	)
	err := svc.srv.Serve(svc.sock)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("http3server: serving: %w", err)
}

// Shutdown sends every connection a GOAWAY, rejects the requests that
// follow it with H3_REQUEST_REJECTED, and waits for in-flight requests to
// finish. It closes each connection with H3_NO_ERROR one second after that
// connection's last request finished, or one second after Shutdown began
// for a connection with none, so a client that keeps an idle connection
// open doesn't hold it. A connection opened after Shutdown has begun gets
// no answer, so its client waits out its handshake timeout.
//
// If ctx is done first, or is done already, it closes every connection at
// once, which cancels the context of each in-flight request, and returns an
// error that wraps ctx's error, without waiting for a handler that ignores
// its context. Calling it again returns nil.
func (svc *Service) Shutdown(ctx context.Context) error {
	if !svc.shutdown.CompareAndSwap(false, true) {
		return nil
	}
	if ctx.Err() == nil {
		// quic-go's Shutdown sends the GOAWAYs and waits for every
		// connection to close, which the drain sees to. Once ctx is done,
		// it closes the server, and that Close waits for every handler, so
		// it runs on a goroutine of its own, which ends when the last
		// handler returns.
		shut := make(chan error, 1)
		go func() { shut <- svc.srv.Shutdown(ctx) }()
		svc.drain.start()
		select {
		case err := <-shut:
			if err == nil {
				return nil
			}
			if ctx.Err() == nil {
				return fmt.Errorf("http3server: shutting down: %w", err)
			}
		case <-ctx.Done():
		}
		// quic-go's Close, on the goroutine above, closes every
		// connection too, but nothing orders it before this return.
		svc.drain.closeAll()
		return fmt.Errorf("http3server: shutting down: %w", ctx.Err())
	}
	// Close stops the server, then waits for every handler, so it runs on
	// a goroutine of its own. The drain closes every connection before
	// Shutdown returns, so no request outlives it with its context live.
	go func() { _ = svc.srv.Close() }()
	svc.drain.closeAll()
	return fmt.Errorf("http3server: shutting down: %w", ctx.Err())
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

// The QUIC limits the Service passes explicitly, so that a quic-go bump
// changes none of them.
const (
	maxIdleTimeout             = 30 * time.Second
	maxIncomingStreams         = 100
	maxIncomingUniStreams      = 100
	maxStreamReceiveWindow     = 6 << 20
	maxConnectionReceiveWindow = 15 << 20

	// noTimeoutHandshake is the handshake's bound when no timeout is set:
	// quic-go's own default.
	noTimeoutHandshake = 5 * time.Second
)

// newServer returns the http3.Server that serves h with settings, logging
// quic-go's Error records through logger.
func newServer(settings Settings, h http.Handler, logger log.Logger) *http3.Server {
	return &http3.Server{
		Handler:           h,
		TLSConfig:         settings.TLS.Clone(),
		QUICConfig:        quicConfig(settings),
		MaxHeaderBytes:    settings.MaxHeaderBytes,
		ReadHeaderTimeout: orReadTimeout(settings.ReadHeaderTimeout, settings),
		IdleTimeout:       orReadTimeout(settings.IdleTimeout, settings),
		Logger:            newSlogLogger(logger),
	}
}

// quicConfig returns the QUIC config of a Service with settings. Each limit
// is passed explicitly, 0-RTT is off, and the handshake is bounded by half
// the smallest timeout set: quic-go aborts a handshake silent that long, or
// twice that long in all.
func quicConfig(settings Settings) *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout:       handshakeIdleTimeout(settings),
		MaxIdleTimeout:             maxIdleTimeout,
		MaxIncomingStreams:         maxIncomingStreams,
		MaxIncomingUniStreams:      maxIncomingUniStreams,
		MaxStreamReceiveWindow:     maxStreamReceiveWindow,
		MaxConnectionReceiveWindow: maxConnectionReceiveWindow,
		Allow0RTT:                  false,
		KeepAlivePeriod:            0,
		EnableDatagrams:            false,
	}
}

// handshakeIdleTimeout returns half the smallest of settings'
// ReadHeaderTimeout, ReadTimeout and WriteTimeout that is set, or 5s when
// none is.
func handshakeIdleTimeout(settings Settings) time.Duration {
	smallest := time.Duration(0)
	for _, d := range []time.Duration{settings.ReadHeaderTimeout, settings.ReadTimeout, settings.WriteTimeout} {
		if d > 0 && (smallest == 0 || d < smallest) {
			smallest = d
		}
	}
	if smallest == 0 {
		return noTimeoutHandshake
	}
	return smallest / 2
}

// orReadTimeout returns d, or settings.ReadTimeout when d is zero.
func orReadTimeout(d time.Duration, settings Settings) time.Duration {
	if d == 0 {
		return settings.ReadTimeout
	}
	return d
}
