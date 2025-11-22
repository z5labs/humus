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

func meter() metric.Meter {
	return otel.Meter("github.com/z5labs/humus/queue/kafka")
}

type consumerMetrics struct {
	messagesProcessed metric.Int64Counter
	messagesCommitted metric.Int64Counter
}

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

	return consumerMetrics{
		messagesProcessed: messagesProcessed,
		messagesCommitted: messagesCommitted,
	}
}
