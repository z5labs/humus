// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"

	"github.com/sourcegraph/conc/pool"
)

// TCPListener is a configuration type for creating TCP network listeners.
// It provides a way to specify the network address through a config.Reader.
type TCPListener struct {
	Addr config.Reader[string]
}

// TCPListenerOption is a functional option for configuring a TCPListener.
type TCPListenerOption func(*TCPListener)

// Addr is a TCPListenerOption that sets the network address for the listener.
// The address should be in the form "host:port" or ":port".
func Addr(addr config.Reader[string]) TCPListenerOption {
	return func(tcpLn *TCPListener) {
		tcpLn.Addr = addr
	}
}

// NewTCPListener creates a new TCPListener with the given options.
// If no address is specified via options, the listener will default to ":8080".
func NewTCPListener(options ...TCPListenerOption) TCPListener {
	tcpLn := TCPListener{
		Addr: config.EmptyReader[string](),
	}

	for _, option := range options {
		option(&tcpLn)
	}

	return tcpLn
}

// Read creates a TCP listener on the configured address.
// If no address was configured, it defaults to ":8080".
// Returns a config.Value containing the net.Listener, or an error if the listener cannot be created.
func (tcpLn TCPListener) Read(ctx context.Context) (config.Value[net.Listener], error) {
	addr := config.MustOr(ctx, ":8080", tcpLn.Addr)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return config.Value[net.Listener]{}, err
	}

	return config.ValueOf(ln), nil
}

// TLSListener wraps a base listener with TLS encryption.
// It returns a config.Reader that creates a TLS listener using the provided base listener
// and TLS configuration when read.
func TLSListener(ln config.Reader[net.Listener], tlsConfig config.Reader[*tls.Config]) config.Reader[net.Listener] {
	return config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
		baseLn := config.Must(ctx, ln)
		cfg := config.Must(ctx, tlsConfig)

		tlsLn := tls.NewListener(baseLn, cfg)
		return config.ValueOf(tlsLn), nil
	})
}

// Server holds the configuration for an HTTP server.
// It provides options to configure timeouts, header limits, and other server behaviors.
type Server struct {
	Listener                     config.Reader[net.Listener]
	DisableGeneralOptionsHandler config.Reader[bool]
	ReadTimeout                  config.Reader[time.Duration]
	ReadHeaderTimeout            config.Reader[time.Duration]
	WriteTimeout                 config.Reader[time.Duration]
	IdleTimeout                  config.Reader[time.Duration]
	MaxHeaderBytes               config.Reader[int]
}

// ServerOption is a functional option for configuring a Server.
type ServerOption func(*Server)

// DisableGeneralOptionsHandler is a ServerOption that controls whether the server
// automatically replies to OPTIONS requests. When disabled, you must handle OPTIONS
// requests explicitly in your handler.
func DisableGeneralOptionsHandler(disable config.Reader[bool]) ServerOption {
	return func(srv *Server) {
		srv.DisableGeneralOptionsHandler = disable
	}
}

// DisableGeneralOptionsHandlerFromEnv returns a config.Reader that reads the
// HTTP_DISABLE_GENERAL_OPTIONS_HANDLER environment variable as a boolean value.
func DisableGeneralOptionsHandlerFromEnv() config.Reader[bool] {
	return config.BoolFromString(config.Env("HTTP_DISABLE_GENERAL_OPTIONS_HANDLER"))
}

// ReadTimeout is a ServerOption that sets the maximum duration for reading the
// entire request, including the body. The default is 5 seconds.
func ReadTimeout(d config.Reader[time.Duration]) ServerOption {
	return func(srv *Server) {
		srv.ReadTimeout = d
	}
}

// ReadTimeoutFromEnv returns a config.Reader that reads the HTTP_READ_TIMEOUT
// environment variable as a time.Duration value.
func ReadTimeoutFromEnv() config.Reader[time.Duration] {
	return config.DurationFromString(config.Env("HTTP_READ_TIMEOUT"))
}

// ReadHeaderTimeout is a ServerOption that sets the maximum duration for reading
// request headers. The default is 2 seconds.
func ReadHeaderTimeout(d config.Reader[time.Duration]) ServerOption {
	return func(srv *Server) {
		srv.ReadHeaderTimeout = d
	}
}

// ReadHeaderTimeoutFromEnv returns a config.Reader that reads the
// HTTP_READ_HEADER_TIMEOUT environment variable as a time.Duration value.
func ReadHeaderTimeoutFromEnv() config.Reader[time.Duration] {
	return config.DurationFromString(config.Env("HTTP_READ_HEADER_TIMEOUT"))
}

// WriteTimeout is a ServerOption that sets the maximum duration before timing out
// writes of the response. The default is 10 seconds.
func WriteTimeout(d config.Reader[time.Duration]) ServerOption {
	return func(srv *Server) {
		srv.WriteTimeout = d
	}
}

// WriteTimeoutFromEnv returns a config.Reader that reads the HTTP_WRITE_TIMEOUT
// environment variable as a time.Duration value.
func WriteTimeoutFromEnv() config.Reader[time.Duration] {
	return config.DurationFromString(config.Env("HTTP_WRITE_TIMEOUT"))
}

// IdleTimeout is a ServerOption that sets the maximum duration to wait for the
// next request when keep-alives are enabled. The default is 120 seconds.
func IdleTimeout(d config.Reader[time.Duration]) ServerOption {
	return func(srv *Server) {
		srv.IdleTimeout = d
	}
}

// IdleTimeoutFromEnv returns a config.Reader that reads the HTTP_IDLE_TIMEOUT
// environment variable as a time.Duration value.
func IdleTimeoutFromEnv() config.Reader[time.Duration] {
	return config.DurationFromString(config.Env("HTTP_IDLE_TIMEOUT"))
}

// MaxHeaderBytes is a ServerOption that sets the maximum number of bytes the
// server will read parsing the request header's keys and values, including the
// request line. The default is 1048576 bytes (1 MB).
func MaxHeaderBytes(n config.Reader[int]) ServerOption {
	return func(srv *Server) {
		srv.MaxHeaderBytes = n
	}
}

// MaxHeaderBytesFromEnv returns a config.Reader that reads the HTTP_MAX_HEADER_BYTES
// environment variable as an integer value.
func MaxHeaderBytesFromEnv() config.Reader[int] {
	return config.IntFromString(config.Env("HTTP_MAX_HEADER_BYTES"))
}

// NewServer creates a new Server with the given listener and options.
// The listener is required; all other settings have default values.
//
// Default values:
//   - DisableGeneralOptionsHandler: false
//   - ReadTimeout: 5 seconds
//   - ReadHeaderTimeout: 2 seconds
//   - WriteTimeout: 10 seconds
//   - IdleTimeout: 120 seconds
//   - MaxHeaderBytes: 1048576 bytes (1 MB)
func NewServer(listener config.Reader[net.Listener], options ...ServerOption) Server {
	srv := Server{
		Listener:                     listener,
		DisableGeneralOptionsHandler: config.EmptyReader[bool](),
		ReadTimeout:                  config.EmptyReader[time.Duration](),
		ReadHeaderTimeout:            config.EmptyReader[time.Duration](),
		WriteTimeout:                 config.EmptyReader[time.Duration](),
		IdleTimeout:                  config.EmptyReader[time.Duration](),
		MaxHeaderBytes:               config.EmptyReader[int](),
	}

	for _, option := range options {
		option(&srv)
	}

	return srv
}

// App represents a running HTTP server application.
// It manages the lifecycle of the HTTP server and handles graceful shutdown.
type App struct {
	ls  net.Listener
	srv *http.Server
}

// Run starts the HTTP server and blocks until the context is cancelled or an error occurs.
// When the context is cancelled, the server performs a graceful shutdown.
// Returns nil if the server shuts down cleanly, or an error if the server fails to start or serve.
func (a App) Run(ctx context.Context) error {
	pool := pool.New().WithContext(ctx)

	pool.Go(func(ctx context.Context) error {
		return a.srv.Serve(a.ls)
	})

	pool.Go(func(ctx context.Context) error {
		<-ctx.Done()
		return a.srv.Shutdown(context.Background())
	})

	err := pool.Wait()
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Build creates an app.Builder that constructs an HTTP server App.
// It takes a Server configuration and an http.Handler builder, and returns a builder
// that produces a runnable App.
//
// The builder applies the Server configuration to create an http.Server with the
// provided handler. If configuration values are not set, defaults are applied as
// documented in NewServer.
func Build(srv Server, b app.Builder[http.Handler]) app.Builder[App] {
	return app.Bind(b, func(h http.Handler) app.Builder[App] {
		return app.BuilderFunc[App](func(ctx context.Context) (App, error) {
			ln := config.Must(ctx, srv.Listener)

			httpServer := &http.Server{
				Handler:                      h,
				DisableGeneralOptionsHandler: config.MustOr(ctx, false, srv.DisableGeneralOptionsHandler),
				ReadTimeout:                  config.MustOr(ctx, 5*time.Second, srv.ReadTimeout),
				ReadHeaderTimeout:            config.MustOr(ctx, 2*time.Second, srv.ReadHeaderTimeout),
				WriteTimeout:                 config.MustOr(ctx, 10*time.Second, srv.WriteTimeout),
				IdleTimeout:                  config.MustOr(ctx, 120*time.Second, srv.IdleTimeout),
				MaxHeaderBytes:               config.MustOr(ctx, 1048576, srv.MaxHeaderBytes),
			}

			app := App{
				ls:  ln,
				srv: httpServer,
			}

			return app, nil
		})
	})
}
