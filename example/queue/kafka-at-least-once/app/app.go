// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// Config holds the application configuration.
type Config struct {
	queue.Config `config:",squash"`

	Kafka struct {
		Brokers []string `config:"brokers"`
		GroupID string   `config:"group_id"`
		Topic   string   `config:"topic"`
	} `config:"kafka"`
}

// Init initializes the application.
func Init(ctx context.Context, cfg Config) (*queue.App, error) {
	// Create business logic processor with idempotent handling
	handler := NewOrderProcessor()

	// Wrap with decoding middleware
	processor := &DecodingProcessor{
		decoder: decodeOrder,
		handler: handler,
	}

	// Create Kafka runtime with at-least-once semantics
	runtime := kafka.NewRuntime(
		cfg.Kafka.Brokers,
		cfg.Kafka.GroupID,
		kafka.AtLeastOnce(cfg.Kafka.Topic, processor),
	)

	return queue.NewApp(runtime), nil
}
