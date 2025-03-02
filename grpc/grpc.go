// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpc

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"syscall"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal"
	"github.com/z5labs/humus/internal/grpcserver"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/app"
	"github.com/z5labs/bedrock/appbuilder"
	"github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/lifecycle"
)

//go:embed default_config.yaml
var defaultConfig []byte

// DefaultConfig is the default [config.Source] which aligns with the [Config] type.
func DefaultConfig() config.Source {
	return internal.ConfigSource(bytes.NewReader(defaultConfig))
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

// Config is the default config which can be easily embedded into
// a more custom app specific config.
type Config struct {
	humus.Config `config:",squash"`

	GRPC struct {
		Port uint `config:"port"`
	} `config:"grpc"`
}

// Listener implements the [ListenerProvider] interface.
func (c Config) Listener(ctx context.Context) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", c.GRPC.Port))
}

// ListenerProvider initializes a [net.Listener].
type ListenerProvider interface {
	Listener(context.Context) (net.Listener, error)
}

// Configer is leveraged to constrain the custom config type into
// supporting specific intialization behaviour required by [Run].
type Configer interface {
	appbuilder.OTelInitializer
	ListenerProvider
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

					var base bedrock.App = grpcserver.NewApp(ls, api.server)
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
			ro.logger.Error("unexpected error while running grpc app", slog.Any("error", err))
		})),
	)
	runner.Run(context.Background(), WithDefaultConfig(r))
}
