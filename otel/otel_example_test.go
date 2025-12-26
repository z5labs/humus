// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel_test

import (
	"context"
	"fmt"
	"time"

	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/otel"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"
)

func ExampleNewResource() {
	// Set environment variables before calling
	// OTEL_SERVICE_NAME=my-service
	// OTEL_SERVICE_VERSION=1.0.0

	res := otel.NewResource(
		otel.ServiceName(otel.ServiceNameFromEnv()),
		otel.ServiceVersion(otel.ServiceVersionFromEnv()),
	)

	ctx := context.Background()
	val, err := res.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	rsc, ok := val.Value()
	if !ok {
		fmt.Println("no resource available")
		return
	}

	_ = rsc
	fmt.Println("resource created successfully")
	// Output: resource created successfully
}

func ExampleNewResource_withOverrides() {
	// Override configuration programmatically
	res := otel.NewResource(
		otel.ServiceName(config.ReaderOf("custom-service")),
		otel.ServiceVersion(config.ReaderOf("2.0.0")),
	)

	ctx := context.Background()
	val, err := res.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	rsc, ok := val.Value()
	if !ok {
		fmt.Println("no resource available")
		return
	}

	_ = rsc
	fmt.Println("resource created with custom configuration")
	// Output: resource created with custom configuration
}

func ExampleResource_Read() {
	// Create resource configuration directly
	res := otel.Resource{
		SchemaURL:      config.ReaderOf(""),
		ServiceName:    config.ReaderOf("example-service"),
		ServiceVersion: config.ReaderOf("1.0.0"),
	}

	ctx := context.Background()
	val, err := res.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	rsc, ok := val.Value()
	if !ok {
		fmt.Println("no resource available")
		return
	}

	_ = rsc
	fmt.Println("resource created successfully")
	// Output: resource created successfully
}

func ExampleNewTraceIDRatioBasedSampler() {
	// Set environment variable before calling
	// OTEL_TRACES_SAMPLER_RATIO=0.5

	sampler := otel.NewTraceIDRatioBasedSampler(
		otel.TraceIDSampleRatio(otel.TraceIDSampleRatioFromEnv()),
	)

	ctx := context.Background()
	val, err := sampler.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	s, ok := val.Value()
	if !ok {
		fmt.Println("no sampler available")
		return
	}

	_ = s
	fmt.Println("sampler created successfully")
	// Output: sampler created successfully
}

func ExampleNewTraceIDRatioBasedSampler_withOverrides() {
	// Override the sampling ratio
	sampler := otel.NewTraceIDRatioBasedSampler(
		otel.TraceIDSampleRatio(config.ReaderOf(0.25)),
	)

	ctx := context.Background()
	val, err := sampler.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	s, ok := val.Value()
	if !ok {
		fmt.Println("no sampler available")
		return
	}

	_ = s
	fmt.Println("custom sampler created")
	// Output: custom sampler created
}

func ExampleTraceIDRatioBasedSampler_Read() {
	// Create sampler configuration directly
	sampler := otel.TraceIDRatioBasedSampler{
		Ratio: config.ReaderOf(0.1),
	}

	ctx := context.Background()
	val, err := sampler.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	s, ok := val.Value()
	if !ok {
		fmt.Println("no sampler available")
		return
	}

	_ = s
	fmt.Println("sampler with 10% ratio created")
	// Output: sampler with 10% ratio created
}

func ExampleNewBatchSpanProcessor() {
	// Set environment variables before calling
	// OTEL_BSP_EXPORT_INTERVAL=10s
	// OTEL_BSP_MAX_EXPORT_BATCH_SIZE=1024

	exporter := config.ReaderFunc[sdktrace.SpanExporter](func(ctx context.Context) (config.Value[sdktrace.SpanExporter], error) {
		// In real usage, create an actual exporter (e.g., OTLP, Jaeger)
		return config.ValueOf[sdktrace.SpanExporter](nil), nil
	})

	bsp := otel.NewBatchSpanProcessor(exporter,
		otel.ExportInterval(otel.ExportIntervalFromEnv()),
		otel.MaxExportBatchSize(otel.MaxExportBatchSizeFromEnv()),
	)

	ctx := context.Background()
	val, err := bsp.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	processor, ok := val.Value()
	if !ok {
		fmt.Println("no processor available")
		return
	}
	defer processor.Shutdown(ctx)

	fmt.Println("batch span processor created")
	// Output: batch span processor created
}

func ExampleBatchSpanProcessor_Read() {
	// Create batch span processor configuration directly
	exporter := config.ReaderFunc[sdktrace.SpanExporter](func(ctx context.Context) (config.Value[sdktrace.SpanExporter], error) {
		return config.ValueOf[sdktrace.SpanExporter](nil), nil
	})

	bsp := otel.BatchSpanProcessor{
		Exporter:           exporter,
		ExportInterval:     config.ReaderOf(5 * time.Second),
		MaxExportBatchSize: config.ReaderOf(512),
	}

	ctx := context.Background()
	val, err := bsp.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	processor, ok := val.Value()
	if !ok {
		fmt.Println("no processor available")
		return
	}
	defer processor.Shutdown(ctx)

	fmt.Println("batch span processor created with custom settings")
	// Output: batch span processor created with custom settings
}

func ExampleSdkTracerProvider_Read() {
	// Create tracer provider configuration
	resourceReader := config.ReaderFunc[*resource.Resource](func(ctx context.Context) (config.Value[*resource.Resource], error) {
		rsc, err := resource.New(ctx,
			resource.WithTelemetrySDK(),
			resource.WithAttributes(semconv.ServiceName("example-service")),
		)
		if err != nil {
			return config.Value[*resource.Resource]{}, err
		}
		return config.ValueOf(rsc), nil
	})

	samplerReader := config.ReaderFunc[sdktrace.Sampler](func(ctx context.Context) (config.Value[sdktrace.Sampler], error) {
		return config.ValueOf(sdktrace.AlwaysSample()), nil
	})

	processorReader := config.ReaderFunc[sdktrace.SpanProcessor](func(ctx context.Context) (config.Value[sdktrace.SpanProcessor], error) {
		return config.ValueOf[sdktrace.SpanProcessor](nil), nil
	})

	provider := otel.SdkTracerProvider{
		Resource:      resourceReader,
		Sampler:       samplerReader,
		SpanProcessor: processorReader,
	}

	ctx := context.Background()
	val, err := provider.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	tp, ok := val.Value()
	if !ok {
		fmt.Println("no tracer provider available")
		return
	}

	_ = tp
	fmt.Println("tracer provider created")
	// Output: tracer provider created
}

func ExampleNewPeriodicReader() {
	// Set environment variable before calling
	// OTEL_METRIC_EXPORT_INTERVAL=30s

	exporter := config.ReaderFunc[sdkmetric.Exporter](func(ctx context.Context) (config.Value[sdkmetric.Exporter], error) {
		// In real usage, create an actual metric exporter
		return config.ValueOf[sdkmetric.Exporter](nil), nil
	})

	pr := otel.NewPeriodicReader(exporter,
		otel.ExportIntervalMetric(config.ReaderOf(30*time.Second)),
	)

	ctx := context.Background()
	val, err := pr.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	reader, ok := val.Value()
	if !ok {
		fmt.Println("no reader available")
		return
	}
	_ = reader

	fmt.Println("periodic reader created")
	// Output: periodic reader created
}

func ExamplePeriodicReader_Read() {
	// Create periodic reader configuration directly
	exporter := config.ReaderFunc[sdkmetric.Exporter](func(ctx context.Context) (config.Value[sdkmetric.Exporter], error) {
		return config.ValueOf[sdkmetric.Exporter](nil), nil
	})

	pr := otel.PeriodicReader{
		Exporter:       exporter,
		ExportInterval: config.ReaderOf(60 * time.Second),
	}

	ctx := context.Background()
	val, err := pr.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	reader, ok := val.Value()
	if !ok {
		fmt.Println("no reader available")
		return
	}
	_ = reader

	fmt.Println("periodic reader created with 60s interval")
	// Output: periodic reader created with 60s interval
}

func ExampleSdkMeterProvider_Read() {
	// Create meter provider configuration
	resourceReader := config.ReaderFunc[*resource.Resource](func(ctx context.Context) (config.Value[*resource.Resource], error) {
		rsc, err := resource.New(ctx,
			resource.WithTelemetrySDK(),
			resource.WithAttributes(semconv.ServiceName("example-service")),
		)
		if err != nil {
			return config.Value[*resource.Resource]{}, err
		}
		return config.ValueOf(rsc), nil
	})

	readerReader := config.ReaderFunc[sdkmetric.Reader](func(ctx context.Context) (config.Value[sdkmetric.Reader], error) {
		return config.ValueOf[sdkmetric.Reader](nil), nil
	})

	provider := otel.SdkMeterProvider{
		Resource: resourceReader,
		Reader:   readerReader,
	}

	ctx := context.Background()
	val, err := provider.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	mp, ok := val.Value()
	if !ok {
		fmt.Println("no meter provider available")
		return
	}

	_ = mp
	fmt.Println("meter provider created")
	// Output: meter provider created
}

func ExampleNewBatchLogProcessor() {
	// Set environment variables before calling
	// OTEL_BLP_EXPORT_INTERVAL=5s
	// OTEL_BLP_MAX_EXPORT_BATCH_SIZE=256

	exporter := config.ReaderFunc[sdklog.Exporter](func(ctx context.Context) (config.Value[sdklog.Exporter], error) {
		// In real usage, create an actual log exporter
		return config.ValueOf[sdklog.Exporter](nil), nil
	})

	blp := otel.NewBatchLogProcessor(exporter,
		otel.ExportIntervalLog(config.ReaderOf(5*time.Second)),
		otel.MaxExportBatchSizeLog(config.ReaderOf(256)),
	)

	ctx := context.Background()
	val, err := blp.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	processor, ok := val.Value()
	if !ok {
		fmt.Println("no processor available")
		return
	}
	_ = processor

	fmt.Println("batch log processor created")
	// Output: batch log processor created
}

func ExampleBatchLogProcessor_Read() {
	// Create batch log processor configuration directly
	exporter := config.ReaderFunc[sdklog.Exporter](func(ctx context.Context) (config.Value[sdklog.Exporter], error) {
		return config.ValueOf[sdklog.Exporter](nil), nil
	})

	blp := otel.BatchLogProcessor{
		Exporter:           exporter,
		ExportInterval:     config.ReaderOf(10 * time.Second),
		MaxExportBatchSize: config.ReaderOf(512),
	}

	ctx := context.Background()
	val, err := blp.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	processor, ok := val.Value()
	if !ok {
		fmt.Println("no processor available")
		return
	}
	_ = processor

	fmt.Println("batch log processor created with custom settings")
	// Output: batch log processor created with custom settings
}

func ExampleSdkLoggerProvider_Read() {
	// Create logger provider configuration
	resourceReader := config.ReaderFunc[*resource.Resource](func(ctx context.Context) (config.Value[*resource.Resource], error) {
		rsc, err := resource.New(ctx,
			resource.WithTelemetrySDK(),
			resource.WithAttributes(semconv.ServiceName("example-service")),
		)
		if err != nil {
			return config.Value[*resource.Resource]{}, err
		}
		return config.ValueOf(rsc), nil
	})

	processorReader := config.ReaderFunc[sdklog.Processor](func(ctx context.Context) (config.Value[sdklog.Processor], error) {
		return config.ValueOf[sdklog.Processor](nil), nil
	})

	provider := otel.SdkLoggerProvider{
		Resource:     resourceReader,
		LogProcessor: processorReader,
	}

	ctx := context.Background()
	val, err := provider.Read(ctx)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	lp, ok := val.Value()
	if !ok {
		fmt.Println("no logger provider available")
		return
	}

	_ = lp
	fmt.Println("logger provider created")
	// Output: logger provider created
}
