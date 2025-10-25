// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus provides utilities for building and running bedrock applications
// with integrated OpenTelemetry logging and error handling.
//
// The package simplifies application lifecycle management by providing:
//   - OpenTelemetry-integrated structured logging via [Logger] and [LogHandler]
//   - Application runners with customizable error handling via [Runner]
//   - Standard patterns for building and running bedrock applications
//
// # Basic Usage
//
// Create a logger with OpenTelemetry integration:
//
//	log := humus.Logger("myapp")
//	log.Info("application started")
//
// Build and run an application with error handling:
//
//	runner := humus.NewRunner(
//	    appBuilder,
//	    humus.OnError(humus.ErrorHandlerFunc(func(err error) {
//	        log.Printf("Error: %v", err)
//	    })),
//	)
//	runner.Run(context.Background(), config)
package humus

import (
	"context"
	"log/slog"
	"os"

	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/contrib/bridges/otelslog"
)

// Logger creates a new structured logger with OpenTelemetry integration.
// The logger automatically bridges log records to OpenTelemetry, enabling
// correlation between logs and traces.
//
// The name parameter identifies the logger and appears in log output,
// making it easier to filter and identify log sources.
//
// Example:
//
//	log := humus.Logger("rest-api")
//	log.Info("server started", slog.Int("port", 8080))
//	log.Error("connection failed", slog.String("error", err.Error()))
func Logger(name string) *slog.Logger {
	return otelslog.NewLogger(name)
}

// LogHandler creates a new slog.Handler with OpenTelemetry integration.
// This is useful when you need to create a custom logger with specific
// handler options while maintaining OpenTelemetry integration.
//
// Example:
//
//	handler := humus.LogHandler("myapp")
//	logger := slog.New(handler)
func LogHandler(name string) slog.Handler {
	return otelslog.NewHandler(name)
}

// RunnerOptions holds configuration parameters for a [Runner].
// These options control how the runner handles errors during application
// build and execution phases.
type RunnerOptions struct {
	errHandler ErrorHandler
}

// RunnerOption configures a [Runner].
// Use [OnError] to customize error handling behavior.
type RunnerOption interface {
	ApplyRunnerOption(*RunnerOptions)
}

type runnerOptionFunc func(*RunnerOptions)

func (f runnerOptionFunc) ApplyRunnerOption(ro *RunnerOptions) {
	f(ro)
}

// ErrorHandler defines custom error handling logic for a [Runner].
// The handler is called when errors occur during application building or running,
// allowing applications to implement custom error logging, reporting, or recovery.
//
// By default, the runner logs errors as JSON to stdout.
type ErrorHandler interface {
	HandleError(error)
}

// ErrorHandlerFunc is a function adapter that implements [ErrorHandler].
// It allows regular functions to be used as error handlers.
//
// Example:
//
//	handler := humus.ErrorHandlerFunc(func(err error) {
//	    log.Printf("Application error: %v", err)
//	    metrics.RecordError(err)
//	})
type ErrorHandlerFunc func(error)

// HandleError implements the [ErrorHandler] interface.
func (f ErrorHandlerFunc) HandleError(err error) {
	f(err)
}

// OnError configures a custom [ErrorHandler] for a [Runner].
// The error handler is invoked whenever the runner encounters an error
// during application build or execution.
//
// Example:
//
//	runner := humus.NewRunner(
//	    builder,
//	    humus.OnError(humus.ErrorHandlerFunc(func(err error) {
//	        log.Fatal(err)
//	    })),
//	)
func OnError(eh ErrorHandler) RunnerOption {
	return runnerOptionFunc(func(ro *RunnerOptions) {
		ro.errHandler = eh
	})
}

// Runner orchestrates the complete lifecycle of a [bedrock.App].
// It handles both the build phase (creating the app from configuration)
// and the run phase (executing the app), with customizable error handling
// for each stage.
//
// The runner uses the provided [bedrock.AppBuilder] to construct the application
// and invokes the configured [ErrorHandler] if any errors occur.
type Runner[T any] struct {
	builder    bedrock.AppBuilder[T]
	errHandler ErrorHandler
}

// NewRunner creates a new [Runner] with the specified app builder and options.
//
// By default, the runner logs errors as JSON to stdout. This behavior can be
// customized using the [OnError] option.
//
// Type parameter T represents the configuration type that will be passed to
// the app builder during the build phase.
//
// Example:
//
//	builder := appbuilder.FromConfig(myBuilder)
//	runner := humus.NewRunner(
//	    builder,
//	    humus.OnError(humus.ErrorHandlerFunc(func(err error) {
//	        fmt.Fprintf(os.Stderr, "Fatal error: %v\n", err)
//	        os.Exit(1)
//	    })),
//	)
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

// Run executes the complete application lifecycle using the provided configuration.
//
// The method performs two main steps:
//  1. Build: Constructs the application using the builder and configuration
//  2. Run: Executes the built application
//
// If an error occurs during either step, the configured error handler is invoked.
// The method does not return errors directly; instead, all error handling is
// delegated to the [ErrorHandler].
//
// The context is passed to both the build and run phases, allowing for proper
// cancellation and deadline handling throughout the application lifecycle.
//
// Example:
//
//	config := loadConfig()
//	runner := humus.NewRunner(appBuilder)
//	runner.Run(context.Background(), config)
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
