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

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/app"
	"github.com/z5labs/bedrock/appbuilder"
	"github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/lifecycle"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

//go:embed default_config.yaml
var DefaultConfig []byte

// Configer is leveraged to constrain the custom config type into
// supporting specific initialization behaviour required by [Run].
type Configer interface {
	appbuilder.OTelInitializer

	Listener(context.Context) (net.Listener, error)
	HttpServer(context.Context, http.Handler) (*http.Server, error)
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

// Run begins by reading, parsing and unmarshaling your custom config into
// the type T. Then it calls the providing function to initialize your [Api]
// implementation. Once it has the [Api] implementation, it begins serving
// the [Api] over HTTP. Various middlewares are applied at different stages
// for your convenience. Some middlewares include, automattic panic recovery,
// OTel SDK initialization and shutdown, and OS signal based shutdown.
func Run[T Configer](r io.Reader, f func(context.Context, T) (*Api, error)) {
	cfg := config.MultiSource(
		internal.ConfigSource(bytes.NewReader(humus.DefaultConfig)),
		internal.ConfigSource(bytes.NewReader(DefaultConfig)),
		internal.ConfigSource(r),
	)

	builder := appbuilder.FromConfig(
		appbuilder.LifecycleContext(
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
		),
	)

	err := internal.Run(context.Background(), cfg, builder)
	if err == nil {
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}))
	log.Error("failed to run rest app", slog.String("error", err.Error()))
}
