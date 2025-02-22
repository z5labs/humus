// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus provides a base config and abstraction for running apps.
package humus

import (
	"context"
	"log/slog"
	"os"

	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/contrib/bridges/otelslog"
)

// Logger
func Logger(name string) *slog.Logger {
	return otelslog.NewLogger(name)
}

// RunnerOptions are configurable parameters of a [Runner].
type RunnerOptions struct {
	errHandler ErrorHandler
}

// RunnerOption sets a value on [RunnerOptions].
type RunnerOption interface {
	ApplyRunnerOption(*RunnerOptions)
}

type runnerOptionFunc func(*RunnerOptions)

func (f runnerOptionFunc) ApplyRunnerOption(ro *RunnerOptions) {
	f(ro)
}

// ErrorHandler allows custom error handling logic to be defined
// for when the [Runner] encounters an error while building or running
// a [bedrock.App].
type ErrorHandler interface {
	HandleError(error)
}

// ErrorHandlerFunc is a func type of the [ErrorHandler] interface.
type ErrorHandlerFunc func(error)

// HandleError implements the [ErrorHandler] inteface.
func (f ErrorHandlerFunc) HandleError(err error) {
	f(err)
}

// OnError registers the given [ErrorHandler] with the [Runner].
func OnError(eh ErrorHandler) RunnerOption {
	return runnerOptionFunc(func(ro *RunnerOptions) {
		ro.errHandler = eh
	})
}

// Runner orchestrates the building of a [bedrock.App] and running it.
type Runner[T any] struct {
	builder    bedrock.AppBuilder[T]
	errHandler ErrorHandler
}

// NewRunner initializes a [Runner].
func NewRunner[T any](builder bedrock.AppBuilder[T], opts ...RunnerOption) Runner[T] {
	ro := &RunnerOptions{
		errHandler: ErrorHandlerFunc(func(err error) {
			log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}))
			log.Error("failed to run", slog.String("error", err.Error()))
		}),
	}
	for _, opt := range opts {
		opt.ApplyRunnerOption(ro)
	}
	return Runner[T]{
		builder:    builder,
		errHandler: ro.errHandler,
	}
}

// Run builds a [bedrock.App], runs it, and handles any error
// returned from either of those steps.
func (r Runner[T]) Run(ctx context.Context, cfg T) {
	app, err := r.builder.Build(ctx, cfg)
	if err != nil {
		r.errHandler.HandleError(err)
		return
	}

	err = app.Run(ctx)
	if err == nil {
		return
	}
	r.errHandler.HandleError(err)
}
