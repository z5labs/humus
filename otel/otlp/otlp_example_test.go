// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otlp_test

import (
	"context"
	"fmt"

	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/otel/otlp"
)

func ExampleGrpcTraceExporterFromEnv() {
	// Configure via environment variables
	// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=localhost:4317
	exporter := otlp.GrpcTraceExporterFromEnv()

	ctx := context.Background()
	val, err := exporter.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	spanExporter, ok := val.Value()
	if !ok {
		fmt.Println("no exporter available")
		return
	}
	defer spanExporter.Shutdown(ctx)

	fmt.Println("gRPC trace exporter created successfully")
}

func ExampleGrpcTraceExporterFromEnv_withOverrides() {
	// Create exporter with custom configuration
	exporter := otlp.GrpcTraceExporterFromEnv(func(e *otlp.GrpcTraceExporter) {
		e.Conn = &otlp.GrpcConn{
			Target: config.ReaderOf("custom-endpoint:4317"),
		}
	})

	ctx := context.Background()
	val, err := exporter.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	spanExporter, ok := val.Value()
	if !ok {
		fmt.Println("no exporter available")
		return
	}
	defer spanExporter.Shutdown(ctx)

	fmt.Println("custom gRPC trace exporter created successfully")
}

func ExampleHttpTraceExporterFromEnv() {
	// Configure via environment variables
	// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=localhost:4318
	exporter := otlp.HttpTraceExporterFromEnv()

	ctx := context.Background()
	val, err := exporter.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	spanExporter, ok := val.Value()
	if !ok {
		fmt.Println("no exporter available")
		return
	}
	defer spanExporter.Shutdown(ctx)

	fmt.Println("HTTP trace exporter created successfully")
}

func ExampleSelectSpanExporter() {
	// Select exporter based on protocol configuration
	enabled := config.ReaderOf(true)
	protocol := config.ReaderOf(otlp.ProtocolGRPC)

	grpcExporter := otlp.GrpcTraceExporterFromEnv()
	httpExporter := otlp.HttpTraceExporterFromEnv()

	selectedExporter := otlp.SelectSpanExporter(
		enabled,
		protocol,
		grpcExporter,
		httpExporter,
	)

	ctx := context.Background()
	val, err := selectedExporter.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	spanExporter, ok := val.Value()
	if !ok {
		fmt.Println("no exporter selected")
		return
	}
	defer spanExporter.Shutdown(ctx)

	fmt.Println("span exporter selected based on protocol")
}

func ExampleGrpcMetricExporterFromEnv() {
	// Configure via environment variables
	// OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=localhost:4317
	exporter := otlp.GrpcMetricExporterFromEnv()

	ctx := context.Background()
	val, err := exporter.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	metricExporter, ok := val.Value()
	if !ok {
		fmt.Println("no exporter available")
		return
	}
	defer metricExporter.Shutdown(ctx)

	fmt.Println("gRPC metric exporter created successfully")
}

func ExampleHttpMetricExporterFromEnv() {
	// Configure via environment variables
	// OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=localhost:4318
	exporter := otlp.HttpMetricExporterFromEnv()

	ctx := context.Background()
	val, err := exporter.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	metricExporter, ok := val.Value()
	if !ok {
		fmt.Println("no exporter available")
		return
	}
	defer metricExporter.Shutdown(ctx)

	fmt.Println("HTTP metric exporter created successfully")
}

func ExampleGrpcLogExporterFromEnv() {
	// Configure via environment variables
	// OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=localhost:4317
	exporter := otlp.GrpcLogExporterFromEnv()

	ctx := context.Background()
	val, err := exporter.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	logExporter, ok := val.Value()
	if !ok {
		fmt.Println("no exporter available")
		return
	}
	defer logExporter.Shutdown(ctx)

	fmt.Println("gRPC log exporter created successfully")
}

func ExampleHttpLogExporterFromEnv() {
	// Configure via environment variables
	// OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=localhost:4318
	exporter := otlp.HttpLogExporterFromEnv()

	ctx := context.Background()
	val, err := exporter.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	logExporter, ok := val.Value()
	if !ok {
		fmt.Println("no exporter available")
		return
	}
	defer logExporter.Shutdown(ctx)

	fmt.Println("HTTP log exporter created successfully")
}

func ExampleTracesEnabledFromEnv() {
	// Set environment variable: OTEL_EXPORTER_OTLP_TRACES_ENABLED=true
	reader := otlp.TracesEnabledFromEnv()

	ctx := context.Background()
	enabled, err := config.Read(ctx, reader)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	if enabled {
		fmt.Println("traces export enabled")
	} else {
		fmt.Println("traces export disabled")
	}
}

func ExampleTracesProtocolFromEnv() {
	// Set environment variable: OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=grpc
	reader := otlp.TracesProtocolFromEnv()

	ctx := context.Background()
	protocol, err := config.Read(ctx, reader)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Println("protocol:", protocol)
}

func ExampleGrpcConn() {
	// Create a gRPC connection
	conn := &otlp.GrpcConn{
		Target: config.ReaderOf("localhost:4317"),
	}

	ctx := context.Background()
	val, err := conn.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	grpcConn, ok := val.Value()
	if !ok {
		fmt.Println("no connection available")
		return
	}
	defer grpcConn.Close()

	fmt.Println("gRPC connection established")
}
