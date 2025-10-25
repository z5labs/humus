// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package config provides configuration schemas for OpenTelemetry instrumentation
// in Humus applications.
//
// The primary type is [OTel], which defines the complete configuration structure
// for traces, metrics, and logs. This package is designed to work with the Bedrock
// configuration system and supports YAML configuration files with Go templating.
//
// # Configuration Structure
//
// The configuration is organized into four main sections:
//
//   - Resource: Service identification (name and version)
//   - Trace: Distributed tracing configuration (sampling, processors, exporters)
//   - Metric: Metrics collection configuration (readers, exporters)
//   - Log: Logging configuration (processors, exporters)
//
// # Usage
//
// The types in this package are typically used by embedding [OTel] into your
// application's configuration struct:
//
//	type AppConfig struct {
//	    humus.Config `config:",squash"`  // Includes OTel config
//	    HTTP struct {
//	        Port uint `config:"port"`
//	    } `config:"http"`
//	}
//
// The configuration is automatically read from YAML and used to initialize the
// OpenTelemetry SDK when your application starts.
//
// # Example YAML Configuration
//
//	otel:
//	  resource:
//	    service_name: my-service
//	    service_version: 1.0.0
//	  trace:
//	    processor:
//	      type: batch
//	      batch:
//	        export_interval: 10s
//	        max_size: 25
//	    sampling:
//	      ratio: 0.1
//	    exporter:
//	      type: otlp
//	      otlp:
//	        type: grpc
//	        target: localhost:4317
//
// See the default_config.yaml file in the root of the Humus repository for a
// complete example with environment variable templating.
package config

import (
	_ "embed"
	"time"
)

// Resource defines the identification attributes for the OpenTelemetry resource.
//
// A resource represents the entity producing telemetry as attributes. These
// attributes are typically service-level metadata that apply to all telemetry
// signals (traces, metrics, logs).
//
// Configuration mapping:
//   - service_name: The logical name of the service
//   - service_version: The version of the service (e.g., semver, git SHA)
//
// Example:
//
//	resource:
//	  service_name: user-service
//	  service_version: v1.2.3
type Resource struct {
	ServiceName    string `config:"service_name"`
	ServiceVersion string `config:"service_version"`
}

// Batch defines configuration for batch processing of telemetry signals.
//
// Batch processing aggregates telemetry data before exporting to reduce
// network overhead and improve efficiency. This configuration applies to
// both trace span processors and log processors.
//
// Configuration mapping:
//   - export_interval: Time duration between batch exports (e.g., "10s", "1m")
//   - max_size: Maximum number of items in a batch before forcing an export
//
// Example:
//
//	batch:
//	  export_interval: 10s
//	  max_size: 512
type Batch struct {
	ExportInterval time.Duration `config:"export_interval"`
	MaxSize        int           `config:"max_size"`
}

// OTLPConnType specifies the protocol used for OTLP (OpenTelemetry Protocol) connections.
//
// OTLP supports both HTTP and gRPC transports. The choice between them depends
// on your infrastructure requirements:
//   - gRPC: Better performance, multiplexing, but requires gRPC-compatible infrastructure
//   - HTTP: Simpler deployment, works through standard proxies, may have higher latency
type OTLPConnType string

const (
	// OTLPHTTP specifies OTLP over HTTP/protobuf transport.
	// Default port: 4318
	OTLPHTTP OTLPConnType = "http"

	// OTLPGRPC specifies OTLP over gRPC transport.
	// Default port: 4317
	OTLPGRPC OTLPConnType = "grpc"
)

// OTLP defines configuration for OTLP (OpenTelemetry Protocol) exporters.
//
// OTLP is the vendor-neutral protocol for transmitting telemetry data to
// observability backends. This configuration specifies both the transport
// protocol and the destination endpoint.
//
// Configuration mapping:
//   - type: Transport protocol ("grpc" or "http")
//   - target: Destination endpoint (host:port for gRPC, URL for HTTP)
//
// Example:
//
//	otlp:
//	  type: grpc
//	  target: localhost:4317
type OTLP struct {
	Type   OTLPConnType `config:"type"`
	Target string       `config:"target"`
}

// SpanProcessorType specifies how trace spans are processed before export.
//
// Span processors control the lifecycle of spans from creation to export.
// Different processor types offer different tradeoffs between performance
// and resource usage.
type SpanProcessorType string

const (
	// BatchSpanProcessorType aggregates spans and exports them in batches.
	// This is the recommended processor for production use as it reduces
	// network overhead and improves performance.
	BatchSpanProcessorType SpanProcessorType = "batch"
)

// SpanProcessor defines configuration for trace span processing.
//
// Span processors sit between the tracer and exporter, controlling how and
// when spans are sent to the backend. Batch processing is recommended for
// production environments to optimize resource usage.
//
// Configuration mapping:
//   - type: Processor type (currently only "batch" is supported)
//   - batch: Batch processing configuration (when type is "batch")
//
// Example:
//
//	processor:
//	  type: batch
//	  batch:
//	    export_interval: 5s
//	    max_size: 512
type SpanProcessor struct {
	Type  SpanProcessorType `config:"type"`
	Batch Batch             `config:"batch"`
}

// SpanSampling defines trace sampling configuration.
//
// Sampling controls what percentage of traces are recorded and exported.
// This is crucial for managing costs and performance in high-throughput systems.
// A ratio of 1.0 samples all traces, while 0.1 samples 10% of traces.
//
// Configuration mapping:
//   - ratio: Sampling probability (0.0 to 1.0, where 1.0 = 100% sampling)
//
// Example:
//
//	sampling:
//	  ratio: 0.1  # Sample 10% of traces
type SpanSampling struct {
	Ratio float64 `config:"ratio"`
}

// SpanExporterType specifies the protocol used for exporting trace spans.
type SpanExporterType string

const (
	// OTLPSpanExporterType exports spans using the OTLP protocol.
	// This is the standard, vendor-neutral exporter compatible with most
	// observability backends (Jaeger, Zipkin, commercial vendors, etc.).
	OTLPSpanExporterType SpanExporterType = "otlp"
)

// SpanExporter defines configuration for trace span export.
//
// Exporters send trace spans to observability backends. OTLP is the
// recommended exporter as it's vendor-neutral and widely supported.
//
// Configuration mapping:
//   - type: Exporter type (currently only "otlp" is supported)
//   - otlp: OTLP exporter configuration (when type is "otlp")
//
// Example:
//
//	exporter:
//	  type: otlp
//	  otlp:
//	    type: grpc
//	    target: localhost:4317
type SpanExporter struct {
	Type SpanExporterType `config:"type"`
	OTLP OTLP             `config:"otlp"`
}

// Trace defines the complete configuration for distributed tracing.
//
// Tracing provides request-level observability by recording the flow of
// requests through distributed systems. This configuration controls sampling,
// processing, and export of trace spans.
//
// Configuration mapping:
//   - processor: How spans are processed before export
//   - sampling: What percentage of traces to record
//   - exporter: Where and how to send trace data
//
// Example:
//
//	trace:
//	  processor:
//	    type: batch
//	    batch:
//	      export_interval: 10s
//	      max_size: 512
//	  sampling:
//	    ratio: 0.1
//	  exporter:
//	    type: otlp
//	    otlp:
//	      type: grpc
//	      target: localhost:4317
type Trace struct {
	Processor SpanProcessor `config:"processor"`
	Sampling  SpanSampling  `config:"sampling"`
	Exporter  SpanExporter  `config:"exporter"`
}

// MetricReaderType specifies how metrics are read and exported.
//
// Metric readers control when and how metrics are collected from instruments
// and sent to exporters.
type MetricReaderType string

const (
	// PeriodicReaderType exports metrics at regular intervals.
	// This is the recommended reader for production use, providing predictable
	// export behavior and resource usage.
	PeriodicReaderType MetricReaderType = "periodic"
)

// PeriodicReader defines configuration for periodic metric export.
//
// The periodic reader collects metrics from all registered instruments and
// exports them at regular intervals.
//
// Configuration mapping:
//   - export_interval: Time duration between metric exports (e.g., "30s", "1m")
//
// Example:
//
//	periodic:
//	  export_interval: 60s
type PeriodicReader struct {
	ExportInterval time.Duration `config:"export_interval"`
}

// MetricReader defines configuration for metric reading and collection.
//
// Metric readers pull data from instruments and push it to exporters.
// The periodic reader is recommended for most use cases as it provides
// regular, predictable metric collection.
//
// Configuration mapping:
//   - type: Reader type (currently only "periodic" is supported)
//   - periodic: Periodic reader configuration (when type is "periodic")
//
// Example:
//
//	reader:
//	  type: periodic
//	  periodic:
//	    export_interval: 30s
type MetricReader struct {
	Type     MetricReaderType `config:"type"`
	Periodic PeriodicReader   `config:"periodic"`
}

// MetricExporterType specifies the protocol used for exporting metrics.
type MetricExporterType string

const (
	// OTLPMetricExporterType exports metrics using the OTLP protocol.
	// This is the standard, vendor-neutral exporter compatible with most
	// observability backends.
	OTLPMetricExporterType MetricExporterType = "otlp"
)

// MetricExporter defines configuration for metric export.
//
// Exporters send collected metrics to observability backends. OTLP is the
// recommended exporter as it's vendor-neutral and widely supported.
//
// Configuration mapping:
//   - type: Exporter type (currently only "otlp" is supported)
//   - otlp: OTLP exporter configuration (when type is "otlp")
//
// Example:
//
//	exporter:
//	  type: otlp
//	  otlp:
//	    type: grpc
//	    target: localhost:4317
type MetricExporter struct {
	Type MetricExporterType `config:"type"`
	OTLP OTLP               `config:"otlp"`
}

// Metric defines the complete configuration for metrics collection.
//
// Metrics provide aggregated measurements of system behavior over time,
// including counters, gauges, and histograms. This configuration controls
// how metrics are read, aggregated, and exported.
//
// Configuration mapping:
//   - reader: How metrics are collected from instruments
//   - exporter: Where and how to send metric data
//
// Example:
//
//	metric:
//	  reader:
//	    type: periodic
//	    periodic:
//	      export_interval: 60s
//	  exporter:
//	    type: otlp
//	    otlp:
//	      type: grpc
//	      target: localhost:4317
type Metric struct {
	Reader   MetricReader   `config:"reader"`
	Exporter MetricExporter `config:"exporter"`
}

// LogProcessorType specifies how log records are processed before export.
//
// Log processors control the lifecycle of log records from creation to export.
// Different processor types offer different tradeoffs between latency and
// resource efficiency.
type LogProcessorType string

const (
	// SimpleLogProcessorType exports each log record immediately without batching.
	// Useful for development or when low latency is critical, but has higher
	// overhead in production.
	SimpleLogProcessorType LogProcessorType = "simple"

	// BatchLogProcessorType aggregates log records and exports them in batches.
	// This is the recommended processor for production use as it reduces
	// network overhead and improves performance.
	BatchLogProcessorType LogProcessorType = "batch"
)

// LogProcessor defines configuration for log record processing.
//
// Log processors sit between the logger and exporter, controlling how and
// when log records are sent to the backend. Batch processing is recommended
// for production environments to optimize resource usage.
//
// Configuration mapping:
//   - type: Processor type ("simple" or "batch")
//   - batch: Batch processing configuration (when type is "batch")
//
// Example:
//
//	processor:
//	  type: batch
//	  batch:
//	    export_interval: 1s
//	    max_size: 512
type LogProcessor struct {
	Type  LogProcessorType `config:"type"`
	Batch Batch            `config:"batch"`
}

// LogExporterType specifies the protocol used for exporting log records.
type LogExporterType string

const (
	// OTLPLogExporterType exports logs using the OTLP protocol.
	// This is the standard, vendor-neutral exporter compatible with most
	// observability backends. If not configured, logs are exported to stdout
	// as JSON.
	OTLPLogExporterType LogExporterType = "otlp"
)

// LogExporter defines configuration for log record export.
//
// Exporters send log records to observability backends. OTLP is the
// recommended exporter for sending logs to centralized logging systems.
// If no exporter is configured, logs are written to stdout as JSON.
//
// Configuration mapping:
//   - type: Exporter type (currently only "otlp" is supported)
//   - otlp: OTLP exporter configuration (when type is "otlp")
//
// Example:
//
//	exporter:
//	  type: otlp
//	  otlp:
//	    type: grpc
//	    target: localhost:4317
type LogExporter struct {
	Type LogExporterType `config:"type"`
	OTLP OTLP            `config:"otlp"`
}

// Log defines the complete configuration for structured logging.
//
// Logging provides event-level observability with structured log records.
// This configuration controls how logs are processed and exported to
// observability backends. Humus integrates with Go's standard slog package.
//
// Configuration mapping:
//   - processor: How log records are processed before export
//   - exporter: Where and how to send log data
//
// Example:
//
//	log:
//	  processor:
//	    type: batch
//	    batch:
//	      export_interval: 1s
//	      max_size: 512
//	  exporter:
//	    type: otlp
//	    otlp:
//	      type: grpc
//	      target: localhost:4317
type Log struct {
	Processor LogProcessor `config:"processor"`
	Exporter  LogExporter  `config:"exporter"`
}

// OTel defines the complete OpenTelemetry configuration for Humus applications.
//
// This is the root configuration type that encompasses all observability signals:
// traces, metrics, and logs. It should be embedded in your application's config
// structure via [humus.Config].
//
// The configuration follows OpenTelemetry best practices and supports OTLP
// export to any compatible observability backend (Jaeger, Prometheus, Grafana,
// commercial vendors, etc.).
//
// Configuration mapping:
//   - resource: Service identification attributes
//   - trace: Distributed tracing configuration
//   - metric: Metrics collection configuration
//   - log: Structured logging configuration
//
// Example:
//
//	otel:
//	  resource:
//	    service_name: my-service
//	    service_version: v1.0.0
//	  trace:
//	    processor:
//	      type: batch
//	      batch:
//	        export_interval: 10s
//	        max_size: 512
//	    sampling:
//	      ratio: 0.1
//	    exporter:
//	      type: otlp
//	      otlp:
//	        type: grpc
//	        target: localhost:4317
//	  metric:
//	    reader:
//	      type: periodic
//	      periodic:
//	        export_interval: 60s
//	    exporter:
//	      type: otlp
//	      otlp:
//	        type: grpc
//	        target: localhost:4317
//	  log:
//	    processor:
//	      type: batch
//	      batch:
//	        export_interval: 1s
//	        max_size: 512
//	    exporter:
//	      type: otlp
//	      otlp:
//	        type: grpc
//	        target: localhost:4317
type OTel struct {
	Resource Resource `config:"resource"`
	Trace    Trace    `config:"trace"`
	Metric   Metric   `config:"metric"`
	Log      Log      `config:"log"`
}
