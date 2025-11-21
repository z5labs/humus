// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"log/slog"

	"github.com/z5labs/humus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

func logger() *slog.Logger {
	return humus.Logger("github.com/z5labs/humus/queue/kafka")
}

func tracer() trace.Tracer {
	return otel.Tracer("github.com/z5labs/humus/queue/kafka")
}

// meter returns the OpenTelemetry meter for the Kafka package.
// It uses the global meter provider configured via otel.SetMeterProvider.
func meter() metric.Meter {
	return otel.Meter("github.com/z5labs/humus/queue/kafka")
}

// errorType returns a safe, non-sensitive classification of an error for metrics.
// This prevents sensitive information from being exposed in metric labels while
// still providing meaningful categorization for monitoring.
func errorType(err error) string {
	if err == nil {
		return ""
	}
	// Return "processing_error" as a generic classification
	// This provides useful error tracking without exposing sensitive details
	// that might be contained in error messages
	return "processing_error"
}

// consumerMetrics holds the OpenTelemetry metric instruments for Kafka consumer operations.
type consumerMetrics struct {
	messagesProcessed  metric.Int64Counter
	messagesCommitted  metric.Int64Counter
	processingFailures metric.Int64Counter
}

// initConsumerMetrics initializes OpenTelemetry counter metrics for Kafka consumer operations.
// It creates counters for messages processed, messages committed, and processing failures.
// Any errors during metric creation are logged as warnings.
func initConsumerMetrics(log *slog.Logger) consumerMetrics {
	m := meter()

	messagesProcessed, err := m.Int64Counter(
		"messaging.client.messages.processed",
		metric.WithDescription("Total number of Kafka messages processed"),
		metric.WithUnit("{message}"),
	)
	if err != nil {
		log.Warn("failed to create messages processed metric", slog.Any("error", err))
	}

	messagesCommitted, err := m.Int64Counter(
		"messaging.client.messages.committed",
		metric.WithDescription("Total number of Kafka messages successfully committed"),
		metric.WithUnit("{message}"),
	)
	if err != nil {
		log.Warn("failed to create messages committed metric", slog.Any("error", err))
	}

	processingFailures, err := m.Int64Counter(
		"messaging.client.processing.failures",
		metric.WithDescription("Total number of Kafka message processing failures"),
		metric.WithUnit("{failure}"),
	)
	if err != nil {
		log.Warn("failed to create processing failures metric", slog.Any("error", err))
	}

	return consumerMetrics{
		messagesProcessed:  messagesProcessed,
		messagesCommitted:  messagesCommitted,
		processingFailures: processingFailures,
	}
}
