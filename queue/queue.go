// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package queue

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
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

// ErrEndOfQueue should be returned by [Consumer] that are consuming
// from a finite queue. This should then signify to [Runtime]
// implementations to shut down.
var ErrEndOfQueue = errors.New("queue: no more items")

// Consumer consumes message(s), T, from a queue.
//
// Implementations should return [ErrEndOfQueue] when the queue is exhausted to signal
// graceful shutdown to [Runtime] implementations.
type Consumer[T any] interface {
	Consume(context.Context) (T, error)
}

// ConsumerFunc is an adapter to allow the use of ordinary functions as [Consumer]s.
type ConsumerFunc[T any] func(context.Context) (T, error)

// Consume implements the [Consumer] interface.
func (f ConsumerFunc[T]) Consume(ctx context.Context) (T, error) {
	return f(ctx)
}

// Processor implements the business logic for processing message(s), T.
//
// Process is called after a message is consumed and before it is acknowledged.
type Processor[T any] interface {
	Process(context.Context, T) error
}

// ProcessorFunc is an adapter to allow the use of ordinary functions as [Processor]s.
type ProcessorFunc[T any] func(context.Context, T) error

// Process implements the [Processor] interface.
func (f ProcessorFunc[T]) Process(ctx context.Context, t T) error {
	return f(ctx, t)
}

// Acknowledger tells the queue that message(s), T, have been successfully processed.
//
// Acknowledge is called after a message has been successfully processed to confirm
// completion back to the queue system.
type Acknowledger[T any] interface {
	Acknowledge(context.Context, T) error
}

// AcknowledgerFunc is an adapter to allow the use of ordinary functions as [Acknowledger]s.
type AcknowledgerFunc[T any] func(context.Context, T) error

// Acknowledge implements the [Acknowledger] interface.
func (f AcknowledgerFunc[T]) Acknowledge(ctx context.Context, t T) error {
	return f(ctx, t)
}

//go:embed default_config.yaml
var defaultConfig []byte

// DefaultConfig returns the default configuration source for queue-based applications.
//
// It combines the base Humus configuration (which includes OpenTelemetry defaults)
// with queue-specific configuration values from the embedded default_config.yaml file.
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

// Configer represents the requirements for a configuration type used with [Run].
// It must support OpenTelemetry initialization.
//
// The [Config] type implements this interface and can be embedded in custom config types.
type Configer interface {
	appbuilder.OTelInitializer
}

// Config is the base configuration type for queue-based applications.
//
// It embeds [humus.Config] which provides OpenTelemetry configuration including
// traces, metrics, and logs. Applications can embed this type in their own
// configuration structs to add application-specific settings.
type Config struct {
	humus.Config `config:",squash"`
}

// Runtime orchestrates the message queue processing lifecycle.
//
// Implementations should coordinate [Consumer], [Processor], and [Acknowledger]
// to consume, process, and acknowledge messages. When ProcessQueue returns,
// the application will shut down gracefully.
type Runtime interface {
	ProcessQueue(context.Context) error
}

// RuntimeFunc is an adapter to allow the use of ordinary functions as [Runtime]s.
type RuntimeFunc func(context.Context) error

// ProcessQueue implements the [Runtime] interface.
func (f RuntimeFunc) ProcessQueue(ctx context.Context) error {
	return f(ctx)
}

// App wraps a [Runtime] and implements the [bedrock.App] interface.
//
// It provides the integration point between the Humus framework and
// the queue processing runtime implementation.
type App struct {
	runtime Runtime
}

// NewApp creates a new queue-based application with the provided runtime.
//
// The runtime will be invoked when the application runs, orchestrating
// the message queue processing lifecycle.
func NewApp(runtime Runtime) *App {
	return &App{
		runtime: runtime,
	}
}

// Run implements [bedrock.App] interface.
func (a *App) Run(ctx context.Context) error {
	return a.runtime.ProcessQueue(ctx)
}

// Builder creates an application builder that wraps the provided initializer function
// with standardized middleware for queue-based applications.
//
// The builder automatically adds:
//   - OpenTelemetry SDK initialization for traces, metrics, and logs
//   - Panic recovery to prevent crashes from panics
//   - Lifecycle context management
//   - OS signal handling (SIGTERM, SIGINT, SIGKILL) for graceful shutdown
//
// The initializer function receives a context and configuration, and should return
// the constructed [App] or an error if initialization fails.
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
//	queue.Run(configReader, buildFunc, queue.LogHandler(handler))
func LogHandler(h slog.Handler) RunOption {
	return runOptionFunc(func(ro *RunOptions) {
		ro.logger = slog.New(h)
	})
}

// Run orchestrates the complete lifecycle of a queue-based application.
//
// It reads configuration from the provided reader (expected to be YAML format),
// builds the application using the provided initializer function, and runs it
// until completion or error. The function handles:
//   - Configuration parsing and validation
//   - Application initialization with OpenTelemetry
//   - Graceful shutdown on OS signals
//   - Error logging
//
// The initializer function receives the parsed configuration and should return
// the constructed [App]. If initialization fails, the error is logged and the
// function returns.
//
// Example:
//
//	func main() {
//	    configFile, _ := os.Open("config.yaml")
//	    defer configFile.Close()
//	    queue.Run(configFile, buildApp)
//	}
//
//	func buildApp(ctx context.Context, cfg Config) (*queue.App, error) {
//	    runtime := &MyRuntime{...}
//	    return queue.NewApp(runtime), nil
//	}
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
			ro.logger.Error("unexpected error while running app", slog.Any("error", err))
		})),
	)
	runner.Run(context.Background(), WithDefaultConfig(r))
}
