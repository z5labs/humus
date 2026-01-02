package main

import (
	"context"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	orders "github.com/z5labs/humus/example/rest/orders-walkthrough/app"
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

	// Configure service URLs
	dataURL := config.Or(
		config.Env("DATA_SERVICE_URL"),
		config.ReaderOf("http://localhost:8081"),
	)
	restrictionURL := config.Or(
		config.Env("RESTRICTION_SERVICE_URL"),
		config.ReaderOf("http://localhost:8082"),
	)
	eligibilityURL := config.Or(
		config.Env("ELIGIBILITY_SERVICE_URL"),
		config.ReaderOf("http://localhost:8083"),
	)

	// Build application
	appBuilder := app.BuilderFunc[httpserver.App](func(ctx context.Context) (httpserver.App, error) {
		// Build API
		api, err := orders.BuildApi(ctx, dataURL, restrictionURL, eligibilityURL)
		if err != nil {
			return httpserver.App{}, err
		}

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
