// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/z5labs/humus/app"
	httpserver "github.com/z5labs/humus/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Build creates an [app.Builder] for a REST API application.
//
// The returned builder constructs an HTTP server that serves the provided API
// with OpenTelemetry instrumentation automatically applied.
//
// Parameters:
//   - srv: HTTP server configuration (listener, timeouts, etc.)
//   - api: The REST API to serve
//
// Example:
//
//	listener := httpserver.NewTCPListener(
//	    httpserver.Addr(config.ReaderOf(":8080")),
//	)
//	srv := httpserver.NewServer(listener)
//	api := rest.NewApi("My API", "v1.0.0")
//	builder := rest.Build(srv, api)
func Build(srv httpserver.Server, api *Api) app.Builder[httpserver.App] {
	// Create handler builder that wraps the API with OTel instrumentation
	handlerBuilder := app.BuilderFunc[http.Handler](func(ctx context.Context) (http.Handler, error) {
		return otelhttp.NewHandler(
			api,
			"rest",
			otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
		), nil
	})

	// Use the http package's Build function to create the app
	return httpserver.Build(srv, handlerBuilder)
}

// RunOptions holds configuration for [Run].
// These options control logging and other runtime behavior.
type RunOptions struct {
	logger *slog.Logger
}

// RunOption configures [Run] behavior.
// Use [LogHandler] to customize error logging.
type RunOption interface {
	ApplyRunOption(*RunOptions)
}

type runOptionFunc func(*RunOptions)

func (f runOptionFunc) ApplyRunOption(ro *RunOptions) {
	f(ro)
}

// LogHandler configures a custom log handler for errors during application startup and running.
// By default, errors are logged as JSON to stdout.
//
// Example:
//
//	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
//	rest.Run(ctx, builder, rest.LogHandler(handler))
func LogHandler(h slog.Handler) RunOption {
	return runOptionFunc(func(ro *RunOptions) {
		ro.logger = slog.New(h)
	})
}

// Run builds and runs a REST API application using the provided builder.
//
// This function orchestrates the complete lifecycle of a REST application:
//  1. Builds the HTTP application using the provided builder
//  2. Runs the HTTP server
//  3. Logs any errors that occur
//
// The context should typically be context.Background(). Signal handling
// is performed by app.Run, which will cancel the context on SIGINT, SIGKILL,
// or SIGTERM.
//
// Example:
//
//	listener := httpserver.NewTCPListener(
//	    httpserver.Addr(config.ReaderOf(":8080")),
//	)
//	srv := httpserver.NewServer(listener)
//	api := rest.NewApi("My API", "v1.0.0")
//	builder := rest.Build(srv, api)
//	rest.Run(context.Background(), builder)
func Run(ctx context.Context, builder app.Builder[httpserver.App], opts ...RunOption) error {
	ro := &RunOptions{
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{})),
	}
	for _, opt := range opts {
		opt.ApplyRunOption(ro)
	}

	err := app.Run(ctx, builder)
	if err != nil {
		app.LogError(ro.logger.Handler(), err)
	}
	return err
}
