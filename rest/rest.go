// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package rest
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
	"github.com/z5labs/humus/buildcontext"
	"github.com/z5labs/humus/internal"
	"github.com/z5labs/humus/internal/httpserver"
	"github.com/z5labs/humus/rest/embedded"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/app"
	"github.com/z5labs/bedrock/appbuilder"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

//go:embed default_config.yaml
var DefaultConfig []byte

// Configer
type Configer interface {
	appbuilder.OTelInitializer

	Listener(context.Context) (net.Listener, error)
	HttpServer(context.Context, http.Handler) (*http.Server, error)
}

// Config
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

func (c Config) Listener(ctx context.Context) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", c.HTTP.Port))
}

func (c Config) HttpServer(ctx context.Context, h http.Handler) (*http.Server, error) {
	s := &http.Server{
		Handler:  h,
		ErrorLog: slog.NewLogLogger(humus.Logger("rest").Handler(), slog.LevelError),
	}
	return s, nil
}

// Api
type Api interface {
	embedded.Api

	http.Handler
}

// BuildContext
type BuildContext struct {
	postRunHooks []app.LifecycleHook
	signals      []os.Signal
}

// BuildContextFrom
func BuildContextFrom(ctx context.Context) (*BuildContext, bool) {
	return buildcontext.From[*BuildContext](ctx)
}

// OnPostRun
func (bc *BuildContext) OnPostRun(hook app.LifecycleHook) {
	bc.postRunHooks = append(bc.postRunHooks, hook)
}

// InterruptOn
func (bc *BuildContext) InterruptOn(signals ...os.Signal) {
	bc.signals = signals
}

// Run
func Run[T Configer](r io.Reader, f func(context.Context, T) (Api, error)) {
	err := bedrock.Run(
		context.Background(),
		// OTel middleware will handle shutting down OTel SDK components on PostRun
		// so we don't need to duplicate that in BuildContext.postRunHooks
		appbuilder.OTel(
			appbuilder.Recover(
				buildcontext.BuildWith(
					&BuildContext{
						signals: []os.Signal{os.Interrupt, os.Kill, syscall.SIGTERM},
					},
					buildcontext.AppBuilderFunc[*BuildContext, T](func(ctx context.Context, bc *BuildContext, cfg T) (bedrock.App, error) {
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

						var base bedrock.App = httpserver.NewApp(ls, s)
						base = app.Recover(base)
						base = app.WithLifecycleHooks(base, app.Lifecycle{
							PostRun: app.ComposeLifecycleHooks(bc.postRunHooks...),
						})
						base = app.WithSignalNotifications(base, bc.signals...)
						return base, nil
					}),
				),
			),
		),
		internal.ConfigSource(bytes.NewReader(humus.DefaultConfig)),
		internal.ConfigSource(bytes.NewReader(DefaultConfig)),
		internal.ConfigSource(r),
	)
	if err == nil {
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}))
	log.Error("failed to run rest app", slog.String("error", err.Error()))
}
