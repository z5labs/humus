// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package rest supports creating RESTful applications.
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
	"github.com/z5labs/humus/internal"
	"github.com/z5labs/humus/internal/httpserver"
	"github.com/z5labs/humus/internal/try"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/app"
	"github.com/z5labs/bedrock/appbuilder"
	"github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/lifecycle"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

//go:embed default_config.yaml
var defaultConfig []byte

// DefaultConfig is the default [config.Source] which aligns with the
// [Config] type.
func DefaultConfig() config.Source {
	return config.MultiSource(
		humus.DefaultConfig(),
		internal.ConfigSource(bytes.NewReader(defaultConfig)),
	)
}

// WithDefaultConfig extends the [config.Source] returned by [DefaultConfig]
// to include values from the given [io.Reader]. The [io.Reader] can provide
// custom values, as well as, override default values.
func WithDefaultConfig(r io.Reader) config.Source {
	return config.MultiSource(
		DefaultConfig(),
		internal.ConfigSource(r),
	)
}

// ListenerProvider initializes a [net.Listener].
type ListenerProvider interface {
	Listener(context.Context) (net.Listener, error)
}

// HttpServerProvider initializes a [http.Server].
type HttpServerProvider interface {
	HttpServer(context.Context, http.Handler) (*http.Server, error)
}

// Configer is leveraged to constrain the custom config type into
// supporting specific initialization behaviour required by [Run].
type Configer interface {
	appbuilder.OTelInitializer
	ListenerProvider
	HttpServerProvider
}

// Config is the default config which can be easily embedded into a
// more custom app specific config.
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

// Listener implements the [Configer] interface.
func (c Config) Listener(ctx context.Context) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", c.HTTP.Port))
}

// HttpServer implements the [Configer] interface.
func (c Config) HttpServer(ctx context.Context, h http.Handler) (*http.Server, error) {
	s := &http.Server{
		Handler:  h,
		ErrorLog: slog.NewLogLogger(humus.Logger("rest").Handler(), slog.LevelError),
	}
	return s, nil
}

// Builder initializes a [bedrock.AppBuilder] for your [Api].
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

// RunOptions are used for configuring the running of a [Api].
type RunOptions struct {
	logger *slog.Logger
}

// RunOption sets a value on [RunOptions].
type RunOption interface {
	ApplyRunOption(*RunOptions)
}

type runOptionFunc func(*RunOptions)

func (f runOptionFunc) ApplyRunOption(ro *RunOptions) {
	f(ro)
}

// LogHandler overrides the default [slog.Handler] used for logging
// any error encountered while building or running the [Api].
func LogHandler(h slog.Handler) RunOption {
	return runOptionFunc(func(ro *RunOptions) {
		ro.logger = slog.New(h)
	})
}

// Run begins by reading, parsing and unmarshaling your custom config into
// the type T. Then it calls the providing function to initialize your [Api]
// implementation. Once it has the [Api] implementation, it begins serving
// the [Api] over HTTP. Various middlewares are applied at different stages
// for your convenience. Some middlewares include, automattic panic recovery,
// OTel SDK initialization and shutdown, and OS signal based shutdown.
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
