// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// BuildApp creates the Kafka queue application builder.
func BuildApp(ctx context.Context) app.Builder[queue.Runtime] {
	// Create business logic processor for metrics aggregation
	// Note: No idempotency needed for at-most-once semantics
	handler := NewMetricsProcessor()

	// Wrap with decoding middleware
	processor := &DecodingProcessor{
		decoder: decodeMetric,
		handler: handler,
	}

	// Configure Kafka infrastructure
	cfg := kafka.Config{
		Brokers: config.Or(
			kafka.BrokersFromEnv(),
			config.ReaderOf([]string{"localhost:9092"}),
		),
		GroupID: config.Or(
			kafka.GroupIDFromEnv(),
			config.ReaderOf("metrics-processor"),
		),
	}

	// Configure topic and processor with at-most-once delivery
	// This commits offsets BEFORE processing, which means:
	// - Lower latency and higher throughput
	// - Messages may be lost if processing fails
	// - Suitable for non-critical data like metrics, logs, cache updates
	topic := config.MustOr(ctx, "metrics", config.Env("KAFKA_TOPIC"))
	topics := []kafka.TopicProcessor{
		{
			Topic:        topic,
			Processor:    processor,
			DeliveryMode: kafka.AtMostOnce,
		},
	}

	// Build Kafka queue runtime
	kafkaBuilder := kafka.Build(cfg, topics)

	// Wrap with queue.Runtime
	return app.Bind(kafkaBuilder, func(qr queue.QueueRuntime) app.Builder[queue.Runtime] {
		return queue.Build(qr)
	})
}
