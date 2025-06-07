// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package config

import (
	_ "embed"
	"time"
)

// Resource
type Resource struct {
	ServiceName    string `config:"service_name"`
	ServiceVersion string `config:"service_version"`
}

// Batch
type Batch struct {
	ExportInterval time.Duration `config:"export_interval"`
	MaxSize        int           `config:"max_size"`
}

// OTLPConnType
type OTLPConnType string

const (
	OTLPHTTP OTLPConnType = "http"
	OTLPGRPC OTLPConnType = "grpc"
)

// OTLP
type OTLP struct {
	Type   OTLPConnType `config:"type"`
	Target string       `config:"target"`
}

// SpanProcessorType
type SpanProcessorType string

const (
	BatchSpanProcessorType SpanProcessorType = "batch"
)

// SpanProcessor
type SpanProcessor struct {
	Type  SpanProcessorType `config:"type"`
	Batch Batch             `config:"batch"`
}

// SpanSampling
type SpanSampling struct {
	Ratio float64 `config:"ratio"`
}

// SpanExporterType
type SpanExporterType string

const (
	OTLPSpanExporterType SpanExporterType = "otlp"
)

// SpanExporter
type SpanExporter struct {
	Type SpanExporterType `config:"type"`
	OTLP OTLP             `config:"otlp"`
}

// Trace
type Trace struct {
	Processor SpanProcessor `config:"processor"`
	Sampling  SpanSampling  `config:"sampling"`
	Exporter  SpanExporter  `config:"exporter"`
}

// MetricReaderType
type MetricReaderType string

const (
	PeriodicReaderType MetricReaderType = "periodic"
)

type PeriodicReader struct {
	ExportInterval time.Duration `config:"export_interval"`
}

// MetricReader
type MetricReader struct {
	Type     MetricReaderType `config:"type"`
	Periodic PeriodicReader   `config:"periodic"`
}

// MetricExporterType
type MetricExporterType string

const (
	OTLPMetricExporterType MetricExporterType = "otlp"
)

// MetricExporter
type MetricExporter struct {
	Type MetricExporterType `config:"type"`
	OTLP OTLP               `config:"otlp"`
}

// Metric
type Metric struct {
	Reader   MetricReader   `config:"reader"`
	Exporter MetricExporter `config:"exporter"`
}

// LogProcessorType
type LogProcessorType string

const (
	SimpleLogProcessorType LogProcessorType = "simple"
	BatchLogProcessorType  LogProcessorType = "batch"
)

// LogProcessor
type LogProcessor struct {
	Type  LogProcessorType `config:"type"`
	Batch Batch            `config:"batch"`
}

// LogExporterType
type LogExporterType string

const (
	OTLPLogExporterType LogExporterType = "otlp"
)

// LogExporter
type LogExporter struct {
	Type LogExporterType `config:"type"`
	OTLP OTLP            `config:"otlp"`
}

// Log
type Log struct {
	Processor LogProcessor `config:"processor"`
	Exporter  LogExporter  `config:"exporter"`
}

// OTel
type OTel struct {
	Resource Resource `config:"resource"`
	Trace    Trace    `config:"trace"`
	Metric   Metric   `config:"metric"`
	Log      Log      `config:"log"`
}
