// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package queue

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/z5labs/humus/app"
)

// ErrEndOfQueue should be returned by [Consumer] that are consuming
// from a finite queue. This should then signify to [QueueRuntime]
// implementations to shut down.
var ErrEndOfQueue = errors.New("queue: no more items")

// Consumer consumes message(s), T, from a queue.
//
// Implementations should return [ErrEndOfQueue] when the queue is exhausted to signal
// graceful shutdown to [QueueRuntime] implementations.
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

// QueueRuntime orchestrates the message queue processing lifecycle.
//
// Implementations should coordinate [Consumer], [Processor], and [Acknowledger]
// to consume, process, and acknowledge messages. When ProcessQueue returns,
// the application will shut down gracefully.
type QueueRuntime interface {
	ProcessQueue(context.Context) error
}

// QueueRuntimeFunc is an adapter to allow the use of ordinary functions as [QueueRuntime]s.
type QueueRuntimeFunc func(context.Context) error

// ProcessQueue implements the [QueueRuntime] interface.
func (f QueueRuntimeFunc) ProcessQueue(ctx context.Context) error {
	return f(ctx)
}

// Runtime wraps a [QueueRuntime] and implements the [app.Runtime] interface.
//
// It provides the integration point between the Humus framework and
// the queue processing runtime implementation.
type Runtime struct {
	queueRuntime QueueRuntime
}

// Run implements [app.Runtime] interface.
func (rt Runtime) Run(ctx context.Context) error {
	return rt.queueRuntime.ProcessQueue(ctx)
}

// Build creates an app.Builder for a queue-based application.
//
// The returned builder constructs a queue Runtime that executes the provided
// queue runtime's ProcessQueue method.
//
// Parameters:
//   - queueRuntime: The queue processing runtime that implements message consumption,
//     processing, and acknowledgment logic
//
// Example:
//
//	runtime := &MyQueueRuntime{
//	    consumer:     myConsumer,
//	    processor:    myProcessor,
//	    acknowledger: myAcknowledger,
//	}
//	builder := queue.Build(runtime)
func Build(queueRuntime QueueRuntime) app.Builder[Runtime] {
	return app.BuilderFunc[Runtime](func(ctx context.Context) (Runtime, error) {
		return Runtime{queueRuntime: queueRuntime}, nil
	})
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
//	queue.Run(ctx, builder, queue.LogHandler(handler))
func LogHandler(h slog.Handler) RunOption {
	return runOptionFunc(func(ro *RunOptions) {
		ro.logger = slog.New(h)
	})
}

// Run builds and runs a queue-based application using the provided builder.
//
// This function orchestrates the complete lifecycle of a queue application:
//  1. Builds the queue application using the provided builder
//  2. Runs the queue processing runtime
//  3. Logs any errors that occur
//
// The context should typically be context.Background(). Signal handling
// is performed by app.Run, which will cancel the context on SIGINT, SIGKILL,
// or SIGTERM.
//
// Example:
//
//	runtime := &MyQueueRuntime{...}
//	builder := queue.Build(runtime)
//	queue.Run(context.Background(), builder)
func Run(ctx context.Context, builder app.Builder[Runtime], opts ...RunOption) error {
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
