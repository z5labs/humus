// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package server serves an *http.Server over TLS as a humus.Service.
//
// It is the code every HTTP port of humus's core module is served with: it
// listens through the Setup's Host, serves the *http.Server it is given
// over TLS, through the drain, and shuts it down gracefully, then closes
// every connection once the grace period runs out. Its error log emits each
// of net/http's lines as one ERROR record, through the logger it is given,
// so each package that builds on it logs under its own scope.
//
// It knows nothing of HTTP/2, of instrumentation, or of
// httpserver.Settings: the TLS config, the timeouts, the limits, the
// protocols and the handler are the caller's, set on the *http.Server.
// httpserver builds on it with TLS required, HTTP/2 and otelhttp, and
// httphealth builds its probes server on it.
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus"
)

// Server is an *http.Server, its listener, and the state that makes it a
// humus.Service. It is safe for concurrent use.
type Server struct {
	ln     net.Listener
	srv    *http.Server
	logger log.Logger

	// shutdown is set by the first Shutdown.
	shutdown atomic.Bool
}

// New listens on addr with s.Host.Listen("tcp", addr). A failure to listen,
// such as EADDRINUSE, is its error. It registers a function with s.OnStop
// that closes the listener, so a listener whose Server never serves,
// because Init failed after New, is still closed.
//
// The Server serves srv, which the caller configures with its TLSConfig,
// which must hold the certificate, its timeouts, limits, Protocols and
// handler. New sets srv.ErrorLog, which emits each of net/http's lines as
// one ERROR record through logger, and the caller must not use srv after
// New, other than through the Server. The Server logs through logger too.
func New(s *humus.Setup, addr string, srv *http.Server, logger log.Logger) (*Server, error) {
	ln, err := s.Host.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.OnStop(func(context.Context) error {
		// Serve closes the listener when it returns, so it is usually
		// closed already.
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("closing the listener: %w", err)
		}
		return nil
	})

	srv.ErrorLog = stdlog.New(&errorLog{logger: logger}, "", 0)
	return &Server{ln: ln, srv: srv, logger: logger}, nil
}

// Addr returns the address the Server listens on, with the port the Host
// picked when addr has port 0.
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

// Serve logs "listening" at INFO, with the address as server.address, and
// serves over TLS until Shutdown. It keeps serving after ctx is done, so
// the Server answers requests through the App's drain. It returns nil once
// Shutdown has stopped it, including a Shutdown called before Serve.
func (s *Server) Serve(context.Context) error {
	emit(context.Background(), s.logger, log.SeverityInfo, "listening",
		attribute.String("server.address", s.Addr().String()),
	)

	// The certificate is in the TLS config, so ServeTLS is given no file
	// names.
	err := s.srv.ServeTLS(s.ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serving: %w", err)
}

// Shutdown stops accepting connections, closes idle ones and waits for
// in-flight requests to finish. If ctx is done first, or is done already, it
// closes every connection at once, which cancels the context of each
// in-flight request, and returns an error that wraps ctx's error. Calling it
// again returns nil.
func (s *Server) Shutdown(ctx context.Context) error {
	if !s.shutdown.CompareAndSwap(false, true) {
		return nil
	}
	if ctx.Err() == nil {
		err := s.srv.Shutdown(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() == nil {
			// Every connection was drained, and a listener failed to
			// close.
			return fmt.Errorf("shutting down: %w", err)
		}
	}
	// Close's only error is a listener's, and the Server is stopped
	// whatever it is: ctx's error says why it did not stop gracefully.
	_ = s.srv.Close()
	return fmt.Errorf("shutting down: %w", ctx.Err())
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

// errorLog is the writer of the http.Server's ErrorLog. net/http writes
// each of its lines with one call of the log.Logger, which is one Write, and
// errorLog emits each as an ERROR record, so nothing reaches the real
// standard error.
type errorLog struct {
	logger log.Logger
}

func (w *errorLog) Write(b []byte) (int, error) {
	emit(context.Background(), w.logger, log.SeverityError, string(bytes.TrimSuffix(b, []byte("\n"))))
	return len(b), nil
}
