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
