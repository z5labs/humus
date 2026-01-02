// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	onebrc "github.com/z5labs/humus/example/job/1brc-walkthrough/app"
	"github.com/z5labs/humus/otel"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

func main() {
	// Configure MinIO connection
	minioEndpoint := config.Or(
		config.Env("MINIO_ENDPOINT"),
		config.ReaderOf("localhost:9000"),
	)
	minioAccessKey := config.Or(
		config.Env("MINIO_ACCESS_KEY"),
		config.ReaderOf("minioadmin"),
	)
	minioSecretKey := config.Or(
		config.Env("MINIO_SECRET_KEY"),
		config.ReaderOf("minioadmin"),
	)
	minioBucket := config.Or(
		config.Env("MINIO_BUCKET"),
		config.ReaderOf("onebrc"),
	)

	// Configure 1BRC job parameters
	inputKey := config.Or(
		config.Env("ONEBRC_INPUT_KEY"),
		config.ReaderOf("measurements.txt"),
	)
	outputKey := config.Or(
		config.Env("ONEBRC_OUTPUT_KEY"),
		config.ReaderOf("results.txt"),
	)

	// Build application
	appBuilder := app.BuilderFunc[app.Runtime](func(ctx context.Context) (app.Runtime, error) {
		return onebrc.BuildRuntime(ctx, minioEndpoint, minioAccessKey, minioSecretKey, minioBucket, inputKey, outputKey)
	})

	// Configure OpenTelemetry SDK (disabled for this example)
	sdk := otel.SDK{
		TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
			return config.Value[trace.TracerProvider]{}, nil
		}),
		MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
			return config.Value[metric.MeterProvider]{}, nil
		}),
	}

	// Wrap with OpenTelemetry
	otelBuilder := otel.Build(sdk, appBuilder)

	// Run the application
	_ = app.Run(context.Background(), otelBuilder)
}
