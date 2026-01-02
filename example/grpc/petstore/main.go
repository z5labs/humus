// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"net"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	petstore "github.com/z5labs/humus/example/grpc/petstore/app"
	"github.com/z5labs/humus/grpc"
	"github.com/z5labs/humus/otel"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

func main() {
	// Configure listener
	listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
		addr := config.MustOr(ctx, ":9090", config.Env("GRPC_ADDR"))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return config.Value[net.Listener]{}, err
		}
		return config.ValueOf(ln), nil
	})

	// Build API
	api := petstore.BuildApi(context.Background())

	// Build gRPC app
	appBuilder := grpc.Build(listener, api)

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
