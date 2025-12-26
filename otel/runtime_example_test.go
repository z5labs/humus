// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel_test

import (
	"context"
	"fmt"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/otel"

	"go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func ExampleBuild() {
	// Define OpenTelemetry SDK configuration
	sdk := otel.SDK{
		TextMapPropagator: config.ReaderFunc[propagation.TextMapPropagator](func(ctx context.Context) (config.Value[propagation.TextMapPropagator], error) {
			return config.ValueOf[propagation.TextMapPropagator](propagation.TraceContext{}), nil
		}),
		TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
			return config.ValueOf[trace.TracerProvider](tracenoop.NewTracerProvider()), nil
		}),
		MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
			return config.ValueOf[metric.MeterProvider](metricnoop.NewMeterProvider()), nil
		}),
		LoggerProvider: config.ReaderFunc[log.LoggerProvider](func(ctx context.Context) (config.Value[log.LoggerProvider], error) {
			return config.ValueOf[log.LoggerProvider](lognoop.NewLoggerProvider()), nil
		}),
	}

	// Create a mock application runtime
	appBuilder := app.BuilderFunc[*mockApp](func(ctx context.Context) (*mockApp, error) {
		return &mockApp{}, nil
	})

	// Wrap application with OpenTelemetry runtime
	builder := otel.Build(sdk, appBuilder)

	ctx := context.Background()
	rt, err := builder.Build(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	_ = rt
	fmt.Println("runtime created with OTel providers")
	// Output: runtime created with OTel providers
}

func ExampleBuild_withDefaults() {
	// Use nil readers to get default providers
	sdk := otel.SDK{
		TextMapPropagator: config.ReaderFunc[propagation.TextMapPropagator](func(ctx context.Context) (config.Value[propagation.TextMapPropagator], error) {
			return config.Value[propagation.TextMapPropagator]{}, nil
		}),
		TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
			return config.Value[trace.TracerProvider]{}, nil
		}),
		MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
			return config.Value[metric.MeterProvider]{}, nil
		}),
		LoggerProvider: config.ReaderFunc[log.LoggerProvider](func(ctx context.Context) (config.Value[log.LoggerProvider], error) {
			return config.Value[log.LoggerProvider]{}, nil
		}),
	}

	appBuilder := app.BuilderFunc[*mockApp](func(ctx context.Context) (*mockApp, error) {
		return &mockApp{}, nil
	})

	builder := otel.Build(sdk, appBuilder)

	ctx := context.Background()
	rt, err := builder.Build(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	_ = rt
	fmt.Println("runtime created with default providers")
	// Output: runtime created with default providers
}

type mockApp struct{}

func (m *mockApp) Run(ctx context.Context) error {
	return nil
}
