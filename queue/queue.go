// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package queue

import (
	"context"
	"errors"
)

// ErrEndOfQueue should be returned by [Consumer] implementations that are
// consuming from a finite queue, signalling to [Runtime] implementations
// to shut down gracefully.
var ErrEndOfQueue = errors.New("queue: no more items")

// Consumer consumes messages of type T from a queue.
//
// Implementations should return [ErrEndOfQueue] when the queue is exhausted.
type Consumer[T any] interface {
	Consume(context.Context) (T, error)
}

// ConsumerFunc is an adapter to allow the use of ordinary functions as [Consumer]s.
type ConsumerFunc[T any] func(context.Context) (T, error)

// Consume implements the [Consumer] interface.
func (f ConsumerFunc[T]) Consume(ctx context.Context) (T, error) {
	return f(ctx)
}

// Processor implements the business logic for processing messages of type T.
type Processor[T any] interface {
	Process(context.Context, T) error
}

// ProcessorFunc is an adapter to allow the use of ordinary functions as [Processor]s.
type ProcessorFunc[T any] func(context.Context, T) error

// Process implements the [Processor] interface.
func (f ProcessorFunc[T]) Process(ctx context.Context, t T) error {
	return f(ctx, t)
}

// Acknowledger confirms that messages of type T have been successfully processed.
type Acknowledger[T any] interface {
	Acknowledge(context.Context, T) error
}

// AcknowledgerFunc is an adapter to allow the use of ordinary functions as [Acknowledger]s.
type AcknowledgerFunc[T any] func(context.Context, T) error

// Acknowledge implements the [Acknowledger] interface.
func (f AcknowledgerFunc[T]) Acknowledge(ctx context.Context, t T) error {
	return f(ctx, t)
}

// Runtime orchestrates the message queue processing lifecycle.
type Runtime interface {
	ProcessQueue(context.Context) error
}

// RuntimeFunc is an adapter to allow the use of ordinary functions as [Runtime]s.
type RuntimeFunc func(context.Context) error

// ProcessQueue implements the [Runtime] interface.
func (f RuntimeFunc) ProcessQueue(ctx context.Context) error {
	return f(ctx)
}

// App wraps a [Runtime] and implements the bedrock.Runtime interface.
type App struct {
	runtime Runtime
}

// NewApp creates a new queue-based application with the provided runtime.
func NewApp(runtime Runtime) *App {
	return &App{runtime: runtime}
}

// Run implements the bedrock.Runtime interface.
func (a *App) Run(ctx context.Context) error {
	return a.runtime.ProcessQueue(ctx)
}
