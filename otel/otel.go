// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package otel provides OpenTelemetry SDK configuration types and builders.
//
// This package offers config.Reader implementations for constructing OpenTelemetry
// resources, trace providers, metric providers, and log providers. Each type can
// be configured via environment variables or programmatically with overrides.
//
// The package follows the config.Reader pattern, allowing declarative configuration
// that integrates seamlessly with Humus applications. All types support:
//   - Environment-based configuration via *FromEnv constructors
//   - Programmatic overrides via functional options
//   - Explicit configuration via struct initialization
//
// Environment Variables:
//   - OTEL_SERVICE_NAME: Service name for resource attributes
//   - OTEL_SERVICE_VERSION: Service version for resource attributes
//   - OTEL_TRACES_SAMPLER_RATIO: Sampling ratio for traces (0.0 to 1.0)
//   - OTEL_BSP_EXPORT_INTERVAL: Batch span processor export interval
//   - OTEL_BSP_MAX_EXPORT_BATCH_SIZE: Maximum batch size for span exports
//   - OTEL_METRIC_EXPORT_INTERVAL: Metric export interval
//   - OTEL_BLP_EXPORT_INTERVAL: Batch log processor export interval
//   - OTEL_BLP_MAX_EXPORT_BATCH_SIZE: Maximum batch size for log exports
package otel

import (
	"context"
	"time"

	"github.com/z5labs/humus/config"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"
	"go.opentelemetry.io/otel/trace"
)

// Resource configures an OpenTelemetry resource with service metadata.
// Resources represent the entity producing telemetry data.
type Resource struct {
	SchemaURL      config.Reader[string] // OpenTelemetry schema URL
	ServiceName    config.Reader[string] // Service name attribute
	ServiceVersion config.Reader[string] // Service version attribute
}

// ResourceOption is a functional option for configuring a Resource.
type ResourceOption func(*Resource)

// ServiceName sets the service name reader for the resource.
func ServiceName(name config.Reader[string]) ResourceOption {
	return func(r *Resource) {
		r.ServiceName = name
	}
}

// ServiceNameFromEnv returns a config reader that reads the service name
// from the OTEL_SERVICE_NAME environment variable.
func ServiceNameFromEnv() config.Reader[string] {
	return config.Env("OTEL_SERVICE_NAME")
}

// ServiceVersion sets the service version reader for the resource.
func ServiceVersion(version config.Reader[string]) ResourceOption {
	return func(r *Resource) {
		r.ServiceVersion = version
	}
}

// ServiceVersionFromEnv returns a config reader that reads the service version
// from the OTEL_SERVICE_VERSION environment variable.
func ServiceVersionFromEnv() config.Reader[string] {
	return config.Env("OTEL_SERVICE_VERSION")
}

// NewResource creates a new Resource with the given options.
// Use this constructor to configure resource attributes for OpenTelemetry.
func NewResource(opts ...ResourceOption) Resource {
	res := Resource{
		SchemaURL:      config.EmptyReader[string](),
		ServiceName:    config.EmptyReader[string](),
		ServiceVersion: config.EmptyReader[string](),
	}
	for _, o := range opts {
		o(&res)
	}
	return res
}

// Read constructs an OpenTelemetry resource from the configuration.
//
// The resource includes:
//   - Telemetry SDK attributes (runtime, language, version)
//   - Service name and version from configuration
//   - Optional schema URL for semantic convention version
//
// Defaults:
//   - SchemaURL: "" (uses default from WithTelemetrySDK if not specified)
//   - ServiceName: "" (empty if not configured)
//   - ServiceVersion: "" (empty if not configured)
func (cfg Resource) Read(ctx context.Context) (config.Value[*resource.Resource], error) {
	serviceName := config.MustOr(ctx, "", cfg.ServiceName)
	serviceVersion := config.MustOr(ctx, "", cfg.ServiceVersion)

	opts := []resource.Option{
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
		),
	}

	schemaUrl := config.MustOr(ctx, "", cfg.SchemaURL)
	if schemaUrl != "" {
		opts = append(opts, resource.WithSchemaURL(schemaUrl))
	}

	rsc, err := resource.New(context.Background(), opts...)
	if err != nil {
		return config.Value[*resource.Resource]{}, err
	}

	return config.ValueOf(rsc), nil
}

// TraceIDRatioBasedSampler configures a trace sampler based on trace ID ratio.
// Samples a percentage of traces based on the trace ID.
type TraceIDRatioBasedSampler struct {
	Ratio config.Reader[float64] // Sampling ratio between 0.0 and 1.0
}

// TraceIDRatioBasedSamplerOption is a functional option for configuring a TraceIDRatioBasedSampler.
type TraceIDRatioBasedSamplerOption func(*TraceIDRatioBasedSampler)

// TraceIDSampleRatio sets the sampling ratio reader for the sampler.
func TraceIDSampleRatio(ratio config.Reader[float64]) TraceIDRatioBasedSamplerOption {
	return func(s *TraceIDRatioBasedSampler) {
		s.Ratio = ratio
	}
}

// TraceIDSampleRatioFromEnv returns a config reader that reads the sampling ratio
// from the OTEL_TRACES_SAMPLER_RATIO environment variable.
func TraceIDSampleRatioFromEnv() config.Reader[float64] {
	return config.Float64FromString(config.Env("OTEL_TRACES_SAMPLER_RATIO"))
}

// NewTraceIDRatioBasedSampler creates a new TraceIDRatioBasedSampler with the given options.
// Use this constructor to configure trace sampling based on trace ID ratio.
func NewTraceIDRatioBasedSampler(opts ...TraceIDRatioBasedSamplerOption) TraceIDRatioBasedSampler {
	sampler := TraceIDRatioBasedSampler{
		Ratio: config.EmptyReader[float64](),
	}
	for _, o := range opts {
		o(&sampler)
	}
	return sampler
}

// Read constructs a trace ID ratio-based sampler.
//
// The sampler samples traces based on the trace ID, ensuring consistent
// sampling decisions across distributed services for the same trace.
//
// Default ratio: 1.0 (sample 100% of traces)
func (cfg TraceIDRatioBasedSampler) Read(ctx context.Context) (config.Value[sdktrace.Sampler], error) {
	ratio := config.MustOr(ctx, 1.0, cfg.Ratio)
	sampler := sdktrace.TraceIDRatioBased(ratio)

	return config.ValueOf(sampler), nil
}

// BatchSpanProcessor configures a batch span processor for trace export.
// Batches completed spans and exports them in periodic batches.
type BatchSpanProcessor struct {
	Exporter           config.Reader[sdktrace.SpanExporter] // Span exporter for sending telemetry
	ExportInterval     config.Reader[time.Duration]         // Interval between batch exports
	MaxExportBatchSize config.Reader[int]                   // Maximum spans per batch
}

// BatchSpanProcessorOption is a functional option for configuring a BatchSpanProcessor.
type BatchSpanProcessorOption func(*BatchSpanProcessor)

// ExportInterval sets the export interval reader for the batch span processor.
func ExportInterval(interval config.Reader[time.Duration]) BatchSpanProcessorOption {
	return func(bsp *BatchSpanProcessor) {
		bsp.ExportInterval = interval
	}
}

// ExportIntervalFromEnv returns a config reader that reads the batch span processor
// export interval from the OTEL_BSP_EXPORT_INTERVAL environment variable.
func ExportIntervalFromEnv() config.Reader[time.Duration] {
	return config.DurationFromString(config.Env("OTEL_BSP_EXPORT_INTERVAL"))
}

// MaxExportBatchSize sets the maximum export batch size reader for the batch span processor.
func MaxExportBatchSize(size config.Reader[int]) BatchSpanProcessorOption {
	return func(bsp *BatchSpanProcessor) {
		bsp.MaxExportBatchSize = size
	}
}

// MaxExportBatchSizeFromEnv returns a config reader that reads the maximum export batch size
// from the OTEL_BSP_MAX_EXPORT_BATCH_SIZE environment variable.
func MaxExportBatchSizeFromEnv() config.Reader[int] {
	return config.IntFromString(config.Env("OTEL_BSP_MAX_EXPORT_BATCH_SIZE"))
}

// NewBatchSpanProcessor creates a new BatchSpanProcessor with the given exporter and options.
// Use this constructor to configure batch processing of trace spans.
func NewBatchSpanProcessor(exporter config.Reader[sdktrace.SpanExporter], opts ...BatchSpanProcessorOption) BatchSpanProcessor {
	bsp := BatchSpanProcessor{
		Exporter:           exporter,
		ExportInterval:     config.EmptyReader[time.Duration](),
		MaxExportBatchSize: config.EmptyReader[int](),
	}
	for _, o := range opts {
		o(&bsp)
	}
	return bsp
}

// Read constructs a batch span processor.
//
// The processor batches completed spans and exports them at regular intervals.
// This is more efficient than exporting each span immediately.
//
// Defaults:
//   - ExportInterval: 5 seconds
//   - MaxExportBatchSize: 512 spans
func (cfg BatchSpanProcessor) Read(ctx context.Context) (config.Value[sdktrace.SpanProcessor], error) {
	exporter := config.Must(ctx, cfg.Exporter)
	exportInterval := config.MustOr(ctx, 5*time.Second, cfg.ExportInterval)
	maxExportBatchSize := config.MustOr(ctx, 512, cfg.MaxExportBatchSize)

	bsp := sdktrace.NewBatchSpanProcessor(
		exporter,
		sdktrace.WithBatchTimeout(exportInterval),
		sdktrace.WithMaxExportBatchSize(maxExportBatchSize),
	)

	return config.ValueOf(bsp), nil
}

// SdkTracerProvider configures an OpenTelemetry tracer provider.
// The tracer provider is the entry point for creating tracers.
type SdkTracerProvider struct {
	Resource      config.Reader[*resource.Resource]     // Resource attributes for all spans
	Sampler       config.Reader[sdktrace.Sampler]       // Sampling strategy
	SpanProcessor config.Reader[sdktrace.SpanProcessor] // Processor for completed spans
}

// Read constructs an OpenTelemetry tracer provider.
//
// The tracer provider creates tracers for instrumenting code. It applies
// the configured resource, sampler, and span processor to all traces.
//
// The provider should be registered globally via otel.SetTracerProvider
// and shut down gracefully when the application terminates.
func (cfg SdkTracerProvider) Read(ctx context.Context) (config.Value[trace.TracerProvider], error) {
	rsc := config.Must(ctx, cfg.Resource)
	sampler := config.Must(ctx, cfg.Sampler)
	spanProcessor := config.Must(ctx, cfg.SpanProcessor)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(rsc),
		sdktrace.WithSampler(sampler),
		sdktrace.WithSpanProcessor(spanProcessor),
	)
	return config.ValueOf[trace.TracerProvider](tp), nil
}

// PeriodicReader configures a periodic metric reader for metric export.
// Collects and exports metrics at regular intervals.
type PeriodicReader struct {
	Exporter       config.Reader[sdkmetric.Exporter] // Metric exporter for sending telemetry
	ExportInterval config.Reader[time.Duration]      // Interval between metric collections
}

// PeriodicReaderOption is a functional option for configuring a PeriodicReader.
type PeriodicReaderOption func(*PeriodicReader)

// ExportIntervalMetric sets the export interval reader for the periodic metric reader.
func ExportIntervalMetric(interval config.Reader[time.Duration]) PeriodicReaderOption {
	return func(pr *PeriodicReader) {
		pr.ExportInterval = interval
	}
}

// ExportIntervalMetricFromEnv returns a config reader that reads the metric export interval
// from the OTEL_METRIC_EXPORT_INTERVAL environment variable.
func ExportIntervalMetricFromEnv() config.Reader[time.Duration] {
	return config.DurationFromString(config.Env("OTEL_METRIC_EXPORT_INTERVAL"))
}

// NewPeriodicReader creates a new PeriodicReader with the given exporter and options.
// Use this constructor to configure periodic collection and export of metrics.
func NewPeriodicReader(exporter config.Reader[sdkmetric.Exporter], opts ...PeriodicReaderOption) PeriodicReader {
	pr := PeriodicReader{
		Exporter:       exporter,
		ExportInterval: config.EmptyReader[time.Duration](),
	}
	for _, o := range opts {
		o(&pr)
	}
	return pr
}

// Read constructs a periodic metric reader.
//
// The reader collects metrics from all registered instruments at regular
// intervals and exports them via the configured exporter.
func (cfg PeriodicReader) Read(ctx context.Context) (config.Value[sdkmetric.Reader], error) {
	exporter := config.Must(ctx, cfg.Exporter)
	exportInterval := config.MustOr(ctx, 1*time.Second, cfg.ExportInterval)

	pr := sdkmetric.NewPeriodicReader(
		exporter,
		sdkmetric.WithInterval(exportInterval),
	)

	return config.ValueOf[sdkmetric.Reader](pr), nil
}

// SdkMeterProvider configures an OpenTelemetry meter provider.
// The meter provider is the entry point for creating meters.
type SdkMeterProvider struct {
	Resource config.Reader[*resource.Resource] // Resource attributes for all metrics
	Reader   config.Reader[sdkmetric.Reader]   // Reader for collecting and exporting metrics
}

// Read constructs an OpenTelemetry meter provider.
//
// The meter provider creates meters for recording metrics. It applies
// the configured resource and reader to all metrics.
//
// The provider should be registered globally via otel.SetMeterProvider
// and shut down gracefully when the application terminates.
func (cfg SdkMeterProvider) Read(ctx context.Context) (config.Value[metric.MeterProvider], error) {
	rsc := config.Must(ctx, cfg.Resource)
	reader := config.Must(ctx, cfg.Reader)

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(rsc),
		sdkmetric.WithReader(reader),
	)

	return config.ValueOf[metric.MeterProvider](mp), nil
}

// BatchLogProcessor configures a batch log processor for log export.
// Batches log records and exports them in periodic batches.
type BatchLogProcessor struct {
	Exporter           config.Reader[sdklog.Exporter] // Log exporter for sending telemetry
	ExportInterval     config.Reader[time.Duration]   // Interval between batch exports
	MaxExportBatchSize config.Reader[int]             // Maximum log records per batch
}

// BatchLogProcessorOption is a functional option for configuring a BatchLogProcessor.
type BatchLogProcessorOption func(*BatchLogProcessor)

// ExportIntervalLog sets the export interval reader for the batch log processor.
func ExportIntervalLog(interval config.Reader[time.Duration]) BatchLogProcessorOption {
	return func(blp *BatchLogProcessor) {
		blp.ExportInterval = interval
	}
}

// ExportIntervalLogFromEnv returns a config reader that reads the batch log processor
// export interval from the OTEL_BLP_EXPORT_INTERVAL environment variable.
func ExportIntervalLogFromEnv() config.Reader[time.Duration] {
	return config.DurationFromString(config.Env("OTEL_BLP_EXPORT_INTERVAL"))
}

// MaxExportBatchSizeLog sets the maximum export batch size reader for the batch log processor.
func MaxExportBatchSizeLog(size config.Reader[int]) BatchLogProcessorOption {
	return func(blp *BatchLogProcessor) {
		blp.MaxExportBatchSize = size
	}
}

// MaxExportBatchSizeLogFromEnv returns a config reader that reads the maximum export batch size
// from the OTEL_BLP_MAX_EXPORT_BATCH_SIZE environment variable.
func MaxExportBatchSizeLogFromEnv() config.Reader[int] {
	return config.IntFromString(config.Env("OTEL_BLP_MAX_EXPORT_BATCH_SIZE"))
}

// NewBatchLogProcessor creates a new BatchLogProcessor with the given exporter and options.
// Use this constructor to configure batch processing of log records.
func NewBatchLogProcessor(exporter config.Reader[sdklog.Exporter], opts ...BatchLogProcessorOption) BatchLogProcessor {
	blp := BatchLogProcessor{
		Exporter:           exporter,
		ExportInterval:     config.EmptyReader[time.Duration](),
		MaxExportBatchSize: config.EmptyReader[int](),
	}
	for _, o := range opts {
		o(&blp)
	}
	return blp
}

// Read constructs a batch log processor.
//
// The processor batches log records and exports them at regular intervals.
// This is more efficient than exporting each log record immediately.
func (cfg BatchLogProcessor) Read(ctx context.Context) (config.Value[sdklog.Processor], error) {
	exporter := config.Must(ctx, cfg.Exporter)
	exportInterval := config.MustOr(ctx, 1*time.Second, cfg.ExportInterval)
	maxExportBatchSize := config.MustOr(ctx, 512, cfg.MaxExportBatchSize)

	blp := sdklog.NewBatchProcessor(
		exporter,
		sdklog.WithExportInterval(exportInterval),
		sdklog.WithExportMaxBatchSize(maxExportBatchSize),
	)
	return config.ValueOf[sdklog.Processor](blp), nil
}

// SdkLoggerProvider configures an OpenTelemetry logger provider.
// The logger provider is the entry point for creating loggers.
type SdkLoggerProvider struct {
	Resource     config.Reader[*resource.Resource] // Resource attributes for all logs
	LogProcessor config.Reader[sdklog.Processor]   // Processor for log records
}

// Read constructs an OpenTelemetry logger provider.
//
// The logger provider creates loggers for recording structured logs. It applies
// the configured resource and processor to all log records.
//
// The provider should be registered globally via global.SetLoggerProvider
// and shut down gracefully when the application terminates.
func (cfg SdkLoggerProvider) Read(ctx context.Context) (config.Value[log.LoggerProvider], error) {
	rsc := config.Must(ctx, cfg.Resource)
	logProcessor := config.Must(ctx, cfg.LogProcessor)

	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(rsc),
		sdklog.WithProcessor(logProcessor),
	)

	return config.ValueOf[log.LoggerProvider](lp), nil
}
