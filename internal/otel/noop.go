// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import (
	"context"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace"
)

type noopSpanExporter struct{}

// ExportSpans implements trace.SpanExporter.
func (n noopSpanExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	return nil
}

// Shutdown implements trace.SpanExporter.
func (n noopSpanExporter) Shutdown(ctx context.Context) error {
	return nil
}

type noopMetricExporter struct{}

// Aggregation implements metric.Exporter.
func (n noopMetricExporter) Aggregation(metric.InstrumentKind) metric.Aggregation {
	return nil
}

// Export implements metric.Exporter.
func (n noopMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return nil
}

// ForceFlush implements metric.Exporter.
func (n noopMetricExporter) ForceFlush(context.Context) error {
	return nil
}

// Shutdown implements metric.Exporter.
func (n noopMetricExporter) Shutdown(context.Context) error {
	return nil
}

// Temporality implements metric.Exporter.
func (n noopMetricExporter) Temporality(metric.InstrumentKind) metricdata.Temporality {
	return 0
}
