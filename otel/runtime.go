// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import (
	"context"
	"errors"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"

	"github.com/z5labs/sdk-go/try"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// SDK defines the OpenTelemetry SDK configuration readers.
//
// All fields are optional. If a reader returns a zero value or is nil,
// the Runtime will use a default implementation:
//   - TextMapPropagator: Composite propagator (Baggage + TraceContext)
//   - TracerProvider: No-op tracer provider
//   - MeterProvider: No-op meter provider
//   - LoggerProvider: No-op logger provider
type SDK struct {
	TextMapPropagator config.Reader[propagation.TextMapPropagator] // Context propagation mechanism
	TracerProvider    config.Reader[trace.TracerProvider]          // Trace provider for creating tracers
	MeterProvider     config.Reader[metric.MeterProvider]          // Meter provider for creating meters
	LoggerProvider    config.Reader[log.LoggerProvider]            // Logger provider for creating loggers
}

// Runtime is an application runtime that sets up OpenTelemetry components.
//
// Runtime wraps an inner application runtime and manages the lifecycle of
// OpenTelemetry providers. It registers providers globally before running
// the inner application and shuts them down gracefully afterward.
//
// Do not create Runtime directly; use Build to construct it.
type Runtime struct {
	inner             app.Runtime
	textMapPropagator propagation.TextMapPropagator
	tracerProvider    trace.TracerProvider
	meterProvider     metric.MeterProvider
	loggerProvider    log.LoggerProvider
}

// Build constructs a Runtime builder that sets up OpenTelemetry components.
//
// Build wraps an application runtime builder with OpenTelemetry initialization.
// It reads the SDK configuration and creates providers, falling back to defaults
// for any missing or zero-valued readers.
//
// The returned builder creates a Runtime that:
//  1. Registers OpenTelemetry providers globally (via otel package and global.SetLoggerProvider)
//  2. Runs the inner application
//  3. Shuts down all providers gracefully on exit
//
// Type parameter T must implement app.Runtime.
func Build[T app.Runtime](sdk SDK, builder app.Builder[T]) app.Builder[Runtime] {
	return app.BuilderFunc[Runtime](func(ctx context.Context) (Runtime, error) {
		defaultTextMapPropagator := propagation.NewCompositeTextMapPropagator(
			propagation.Baggage{},
			propagation.TraceContext{},
		)
		var defaultTracerProvider trace.TracerProvider = tracenoop.NewTracerProvider()
		var defaultMeterProvider metric.MeterProvider = metricnoop.NewMeterProvider()
		var defaultLoggerProvider log.LoggerProvider = lognoop.NewLoggerProvider()

		tmp := config.MustOr(ctx, defaultTextMapPropagator, sdk.TextMapPropagator)
		tp := config.MustOr(ctx, defaultTracerProvider, sdk.TracerProvider)
		mp := config.MustOr(ctx, defaultMeterProvider, sdk.MeterProvider)
		lp := config.MustOr(ctx, defaultLoggerProvider, sdk.LoggerProvider)

		otel.SetTextMapPropagator(tmp)
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		global.SetLoggerProvider(lp)

		inner, err := builder.Build(ctx)
		if err != nil {
			return Runtime{}, err
		}

		return Runtime{
			inner:             inner,
			textMapPropagator: tmp,
			tracerProvider:    tp,
			meterProvider:     mp,
			loggerProvider:    lp,
		}, nil
	})
}

// Run sets up the OpenTelemetry components and runs the inner runtime.
//
// Run performs the following steps:
//  1. Runs the inner application runtime
//  2. Shuts down all providers (tracer, meter, logger) on return
//
// Shutdown occurs even if the inner runtime returns an error. All shutdown
// errors are collected and joined with the runtime error.
func (rt Runtime) Run(ctx context.Context) (err error) {
	defer try.Close(&err, shutdown(
		rt.tracerProvider,
		rt.meterProvider,
		rt.loggerProvider,
	))

	return rt.inner.Run(ctx)
}

type closerFunc func() error

func (f closerFunc) Close() error {
	return f()
}

type shutdowner interface {
	Shutdown(context.Context) error
}

func shutdown(vs ...any) closerFunc {
	return func() error {
		var allErrors error
		for _, v := range vs {
			c, ok := v.(shutdowner)
			if !ok {
				continue
			}

			err := c.Shutdown(context.Background())
			if err == nil {
				continue
			}

			allErrors = errors.Join(allErrors, err)
		}
		return allErrors
	}
}
