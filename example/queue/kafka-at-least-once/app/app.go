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
	// Create business logic processor with idempotent handling
	handler := NewOrderProcessor()

	// Wrap with decoding middleware
	processor := &DecodingProcessor{
		decoder: decodeOrder,
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
			config.ReaderOf("order-processor"),
		),
	}

	// Configure topic and processor with at-least-once delivery
	topic := config.MustOr(ctx, "orders", config.Env("KAFKA_TOPIC"))
	topics := []kafka.TopicProcessor{
		{
			Topic:        topic,
			Processor:    processor,
			DeliveryMode: kafka.AtLeastOnce,
		},
	}

	// Build Kafka queue runtime
	kafkaBuilder := kafka.Build(cfg, topics)

	// Wrap with queue.Runtime
	return app.Bind(kafkaBuilder, func(qr queue.QueueRuntime) app.Builder[queue.Runtime] {
		return queue.Build(qr)
	})
}
