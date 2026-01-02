// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	petstore "github.com/z5labs/humus/example/rest/petstore/app"
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

	// Build application with database lifecycle using hooks
	appBuilder := app.WithHooks(func(ctx context.Context, h *app.HookRegistry) (httpserver.App, error) {
		// Open database
		postgresURL := config.MustOr(ctx, "postgres://localhost:5432/petstore", config.Env("POSTGRES_URL"))
		db, err := petstore.OpenDatabase(ctx, postgresURL)
		if err != nil {
			return httpserver.App{}, err
		}

		// Register cleanup hook for database
		h.OnPostRun(func(ctx context.Context) error {
			return db.Close()
		})

		// Build API with hook registry
		api := petstore.BuildApi(ctx, db, h)

		// Build REST app
		restBuilder := rest.Build(srv, api)
		return restBuilder.Build(ctx)
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
