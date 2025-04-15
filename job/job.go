// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"log/slog"
	"os"
	"syscall"

	"github.com/z5labs/humus"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/app"
	"github.com/z5labs/bedrock/appbuilder"
	bedrockcfg "github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/lifecycle"
)

//go:embed default_config.yaml
var defaultConfig []byte

// DefaultConfig returns the default config source which corresponds to the [Config] type.
func DefaultConfig() bedrockcfg.Source {
	return bedrockcfg.MultiSource(
		humus.DefaultConfig(),
		humus.ConfigSource(bytes.NewReader(defaultConfig)),
	)
}

// Config is the default config which can be easily embedded into a
// more custom app specific config.
type Config struct {
	humus.Config `config:",squash"`
}

// Handler represents the core logic of your job.
type Handler interface {
	Handle(context.Context) error
}

// HandlerFunc is an adapter to allow the use of ordinary functions as [Handler]s.
type HandlerFunc func(context.Context) error

// Handle implements the [Handler] interface.
func (f HandlerFunc) Handle(ctx context.Context) error {
	return f(ctx)
}

// App is a [bedrock.App] which handles running your [Handler].
type App struct {
	h Handler
}

// NewApp initializes a new [App].
func NewApp(h Handler) *App {
	return &App{
		h: h,
	}
}

// Run implements the [bedrock.App] interface.
func (a *App) Run(ctx context.Context) error {
	return a.h.Handle(ctx)
}

// Configer is leveraged to constrain the custom config type into
// supporting specific initialization behaviour required by [Run].
type Configer interface {
	appbuilder.OTelInitializer
}

// Builder initializes a [bedrock.AppBuilder] for your [App].
func Builder[T Configer](f func(context.Context, T) (*App, error)) bedrock.AppBuilder[T] {
	return appbuilder.LifecycleContext(
		appbuilder.OTel(
			appbuilder.Recover(
				bedrock.AppBuilderFunc[T](func(ctx context.Context, cfg T) (bedrock.App, error) {
					a, err := f(ctx, cfg)
					if err != nil {
						return nil, err
					}

					bapp := app.InterruptOn(
						app.Recover(a),
						os.Kill,
						os.Interrupt,
						syscall.SIGTERM,
					)
					return bapp, nil
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
func Run[T Configer](r io.Reader, f func(context.Context, T) (*App, error), opts ...RunOption) {
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
	runner.Run(
		context.Background(),
		bedrockcfg.MultiSource(
			DefaultConfig(),
			humus.ConfigSource(r),
		),
	)
}
