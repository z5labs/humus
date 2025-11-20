// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	meterName = "github.com/z5labs/humus/queue/kafka"
)

// metricsRecorder holds OTel metric instruments for tracking Kafka message processing.
type metricsRecorder struct {
	messagesProcessed metric.Int64Counter
	messagesCommitted metric.Int64Counter
	processingFailures metric.Int64Counter
}

// newMetricsRecorder creates a new metricsRecorder with initialized metric instruments.
func newMetricsRecorder() (*metricsRecorder, error) {
	meter := otel.GetMeterProvider().Meter(meterName)

	messagesProcessed, err := meter.Int64Counter(
		"kafka.consumer.messages.processed",
		metric.WithDescription("Total number of Kafka messages processed"),
		metric.WithUnit("{message}"),
	)
	if err != nil {
		return nil, err
	}

	messagesCommitted, err := meter.Int64Counter(
		"kafka.consumer.messages.committed",
		metric.WithDescription("Total number of Kafka messages committed"),
		metric.WithUnit("{message}"),
	)
	if err != nil {
		return nil, err
	}

	processingFailures, err := meter.Int64Counter(
		"kafka.consumer.processing.failures",
		metric.WithDescription("Total number of Kafka message processing failures"),
		metric.WithUnit("{failure}"),
	)
	if err != nil {
		return nil, err
	}

	return &metricsRecorder{
		messagesProcessed:  messagesProcessed,
		messagesCommitted:  messagesCommitted,
		processingFailures: processingFailures,
	}, nil
}

// recordMessageProcessed records a successfully processed message.
func (m *metricsRecorder) recordMessageProcessed(ctx context.Context, topic string, partition int32, deliverySemantics string) {
	m.messagesProcessed.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("topic", topic),
			attribute.Int("partition", int(partition)),
			attribute.String("delivery_semantics", deliverySemantics),
		),
	)
}

// recordMessagesCommitted records successfully committed messages.
func (m *metricsRecorder) recordMessagesCommitted(ctx context.Context, topic string, partition int32, count int) {
	m.messagesCommitted.Add(ctx, int64(count),
		metric.WithAttributes(
			attribute.String("topic", topic),
			attribute.Int("partition", int(partition)),
		),
	)
}

// recordProcessingFailure records a message processing failure.
func (m *metricsRecorder) recordProcessingFailure(ctx context.Context, topic string, partition int32, deliverySemantics string) {
	m.processingFailures.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("topic", topic),
			attribute.Int("partition", int(partition)),
			attribute.String("delivery_semantics", deliverySemantics),
		),
	)
}
