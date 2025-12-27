// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app provides interfaces and utilities for building and running applications.
//
// The package supports post-run hooks for resource cleanup through the WithHooks builder.
// Hooks are executed after the inner runtime completes, allowing for graceful cleanup
// of resources like database connections, file handles, and external service clients.
package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// Builder is a generic interface for building application components.
type Builder[T any] interface {
	Build(context.Context) (T, error)
}

// BuilderFunc is a function type that implements the Builder interface.
type BuilderFunc[T any] func(context.Context) (T, error)

// Build implements the [Builder] interface for BuilderFunc.
func (f BuilderFunc[T]) Build(ctx context.Context) (T, error) {
	return f(ctx)
}

// Build creates a Builder from a function.
func Build[T any](f func(context.Context) (T, error)) Builder[T] {
	return BuilderFunc[T](f)
}

// Bind chains two Builders together, where the output of the first is used to create the second.
func Bind[A, B any](builder Builder[A], binder func(A) Builder[B]) Builder[B] {
	return BuilderFunc[B](func(ctx context.Context) (B, error) {
		appA, err := builder.Build(ctx)
		if err != nil {
			var zero B
			return zero, err
		}
		return binder(appA).Build(ctx)
	})
}

// Runtime is an interface representing a runnable application component.
type Runtime interface {
	Run(context.Context) error
}

// RuntimeFunc is a function type that implements the Runtime interface.
type RuntimeFunc func(context.Context) error

// Run implements the [Runtime] interface for RuntimeFunc.
func (f RuntimeFunc) Run(ctx context.Context) error {
	return f(ctx)
}

// Run builds and runs the application using the provided Builder.
func Run[T Runtime](ctx context.Context, builder Builder[T]) error {
	sigCtx, cancel := signal.NotifyContext(ctx, os.Interrupt, os.Kill, syscall.SIGTERM)
	defer cancel()

	rt, err := builder.Build(sigCtx)
	if err != nil {
		return err
	}

	return rt.Run(sigCtx)
}

// LogError logs an error using the provided slog.Handler.
func LogError(handler slog.Handler, err error) {
	if err == nil {
		return
	}

	log := slog.New(handler)
	log.Error("application error", slog.Any("error", err))
}
