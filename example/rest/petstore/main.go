// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"database/sql"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	petstore "github.com/z5labs/humus/example/rest/petstore/app"
	httpserver "github.com/z5labs/humus/http"
	"github.com/z5labs/humus/otel"
	"github.com/z5labs/humus/rest"

	"github.com/sourcegraph/conc/pool"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// runtime wraps the HTTP app with database lifecycle management.
type runtime struct {
	httpApp httpserver.App
	db      *sql.DB
}

func (rt runtime) Run(ctx context.Context) error {
	p := pool.New().WithContext(ctx)

	// Run the HTTP app
	p.Go(rt.httpApp.Run)

	// Handle cleanup on context cancellation
	p.Go(func(ctx context.Context) error {
		<-ctx.Done()
		rt.db.Close()
		return nil
	})

	return p.Wait()
}

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

	// Build application with database lifecycle
	appBuilder := app.BuilderFunc[runtime](func(ctx context.Context) (runtime, error) {
		// Open database
		postgresURL := config.MustOr(ctx, "postgres://localhost:5432/petstore", config.Env("POSTGRES_URL"))
		db, err := petstore.OpenDatabase(ctx, postgresURL)
		if err != nil {
			return runtime{}, err
		}

		// Build API
		api := petstore.BuildApi(ctx, db)

		// Build REST app
		restBuilder := rest.Build(srv, api)
		httpApp, err := restBuilder.Build(ctx)
		if err != nil {
			db.Close()
			return runtime{}, err
		}

		return runtime{
			httpApp: httpApp,
			db:      db,
		}, nil
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
