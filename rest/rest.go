// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal/httpserver"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/app"
	"github.com/z5labs/bedrock/appbuilder"
	bedrockcfg "github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/lifecycle"
	"github.com/z5labs/sdk-go/try"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

//go:embed default_config.yaml
var defaultConfig []byte

// DefaultConfig returns the default configuration source for REST applications.
// This provides baseline configuration values that align with the [Config] type.
//
// The default configuration includes:
//   - Standard logging and telemetry settings from humus
//   - Default HTTP server settings
//
// Use [WithDefaultConfig] to extend or override these defaults.
func DefaultConfig() bedrockcfg.Source {
	return bedrockcfg.MultiSource(
		humus.DefaultConfig(),
		humus.ConfigSource(bytes.NewReader(defaultConfig)),
	)
}

// WithDefaultConfig creates a configuration source that combines [DefaultConfig]
// with custom values from the provided reader.
//
// The reader should contain YAML configuration. Values from the reader will
// override the defaults.
//
// Example:
//
//	configFile, _ := os.Open("config.yaml")
//	defer configFile.Close()
//	cfg := rest.WithDefaultConfig(configFile)
func WithDefaultConfig(r io.Reader) bedrockcfg.Source {
	return bedrockcfg.MultiSource(
		DefaultConfig(),
		humus.ConfigSource(r),
	)
}

// ListenerProvider creates a network listener for the HTTP server.
// Custom config types can implement this to control how the server listens for connections.
type ListenerProvider interface {
	Listener(context.Context) (net.Listener, error)
}

// HttpServerProvider creates an HTTP server with custom configuration.
// Custom config types can implement this to control server settings like timeouts and TLS.
type HttpServerProvider interface {
	HttpServer(context.Context, http.Handler) (*http.Server, error)
}

// Configer represents the requirements for a configuration type used with [Run].
// It must support OpenTelemetry initialization, network listening, and HTTP server creation.
//
// The [Config] type implements this interface and can be embedded in custom config types.
type Configer interface {
	appbuilder.OTelInitializer
	ListenerProvider
	HttpServerProvider
}

// Config is the default configuration structure for REST applications.
// It can be embedded into custom application-specific configurations.
//
// Configuration fields:
//   - OpenApi.Title: The API title in the OpenAPI specification
//   - OpenApi.Version: The API version in the OpenAPI specification
//   - HTTP.Port: The port number to listen on
//
// Example YAML configuration:
//
//	openapi:
//	  title: "My API"
//	  version: "v1.0.0"
//	http:
//	  port: 8080
type Config struct {
	humus.Config `config:",squash"`

	OpenApi struct {
		Title   string `config:"title"`
		Version string `config:"version"`
	} `config:"openapi"`

	HTTP struct {
		Port uint `config:"port"`
	} `config:"http"`
}

// Listener implements [ListenerProvider] by creating a TCP listener on the configured port.
func (c Config) Listener(ctx context.Context) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", c.HTTP.Port))
}

// HttpServer implements [HttpServerProvider] by creating an HTTP server with the given handler.
// The server is configured with logging integrated with the humus logger.
func (c Config) HttpServer(ctx context.Context, h http.Handler) (*http.Server, error) {
	s := &http.Server{
		Handler:  h,
		ErrorLog: slog.NewLogLogger(humus.Logger("github.com/z5labs/humus/rest").Handler(), slog.LevelError),
	}
	return s, nil
}

// Builder creates an application builder for REST APIs using the bedrock framework.
// The builder handles application lifecycle, including:
//   - OpenTelemetry initialization
//   - Panic recovery
//   - Graceful shutdown
//   - Signal handling
//
// The provided function receives the parsed configuration and returns an initialized [Api].
//
// Example:
//
//	builder := rest.Builder(func(ctx context.Context, cfg rest.Config) (*rest.Api, error) {
//	    return rest.NewApi(cfg.OpenApi.Title, cfg.OpenApi.Version), nil
//	})
func Builder[T Configer](f func(context.Context, T) (*Api, error)) bedrock.AppBuilder[T] {
	return appbuilder.LifecycleContext(
		appbuilder.OTel(
			appbuilder.Recover(
				bedrock.AppBuilderFunc[T](func(ctx context.Context, cfg T) (bedrock.App, error) {
					api, err := f(ctx, cfg)
					if err != nil {
						return nil, err
					}

					ls, err := cfg.Listener(ctx)
					if err != nil {
						return nil, err
					}

					s, err := cfg.HttpServer(ctx, otelhttp.NewHandler(
						api,
						"rest",
						otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
					))
					if err != nil {
						defer try.Close(&err, ls)
						return nil, err
					}
					lc, _ := lifecycle.FromContext(ctx)
					lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
						return s.Shutdown(ctx)
					}))

					var base bedrock.App = httpserver.NewApp(ls, s)
					base = app.Recover(base)
					base = app.InterruptOn(base, os.Kill, os.Interrupt, syscall.SIGTERM)
					return base, nil
				}),
			),
		),
		&lifecycle.Context{},
	)
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
//	rest.Run(configReader, buildFunc, rest.LogHandler(handler))
func LogHandler(h slog.Handler) RunOption {
	return runOptionFunc(func(ro *RunOptions) {
		ro.logger = slog.New(h)
	})
}

// Run starts a REST API application with full lifecycle management.
//
// The function performs the following steps:
//  1. Reads and parses configuration from the reader into type T
//  2. Calls the provided function to build the [Api]
//  3. Initializes the HTTP server and starts serving requests
//  4. Sets up automatic features:
//     - Panic recovery with logging
//     - OpenTelemetry SDK initialization and shutdown
//     - Graceful shutdown on OS signals (SIGINT, SIGTERM, SIGKILL)
//
// The application runs until it receives a shutdown signal or encounters a fatal error.
//
// Example:
//
//	configFile, _ := os.Open("config.yaml")
//	defer configFile.Close()
//
//	rest.Run(configFile, func(ctx context.Context, cfg rest.Config) (*rest.Api, error) {
//	    getUserOp := rest.Handle(http.MethodGet, rest.BasePath("/users"), handler)
//	    return rest.NewApi(cfg.OpenApi.Title, cfg.OpenApi.Version, getUserOp), nil
//	})
func Run[T Configer](r io.Reader, f func(context.Context, T) (*Api, error), opts ...RunOption) {
	ro := &RunOptions{
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{})),
	}
	for _, opt := range opts {
		opt.ApplyRunOption(ro)
	}

	runner := humus.NewRunner(
		appbuilder.FromConfig(Builder(f)),
		humus.OnError(humus.ErrorHandlerFunc(func(err error) {
			ro.logger.Error("unexpected error while running rest app", slog.Any("error", err))
		})),
	)
	runner.Run(context.Background(), WithDefaultConfig(r))
}
