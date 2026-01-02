// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	problemdetails "github.com/z5labs/humus/example/rest/problem-details/app"
	httpserver "github.com/z5labs/humus/http"
	"github.com/z5labs/humus/otel"
	"github.com/z5labs/humus/rest"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

func main() {
	// Configure HTTP listener
	listener := httpserver.NewTCPListener(
		httpserver.Addr(config.Or(
			config.Env("HTTP_ADDR"),
			config.ReaderOf(":8080"),
		)),
	)

	// Configure HTTP server
	srv := httpserver.NewServer(listener)

	// Build API
	api := problemdetails.BuildApi(context.Background())

	// Build REST app
	appBuilder := rest.Build(srv, api)

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
