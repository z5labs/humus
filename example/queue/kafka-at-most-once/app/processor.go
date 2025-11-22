// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/queue/kafka"
)

// MetricMessage represents a metric data point to be processed.
type MetricMessage struct {
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Timestamp int64             `json:"timestamp"`
	Tags      map[string]string `json:"tags"`
}

// MetricsProcessor processes metric messages with at-most-once semantics.
//
// This demonstrates at-most-once processing where messages are acknowledged
// before processing. If processing fails, the message is lost and will not be
// redelivered. This is suitable for non-critical data like metrics, logs, or
// cache updates where occasional message loss is acceptable.
type MetricsProcessor struct {
	log *slog.Logger
}

// NewMetricsProcessor creates a new metrics processor.
func NewMetricsProcessor() *MetricsProcessor {
	return &MetricsProcessor{
		log: humus.Logger("github.com/z5labs/humus/example/queue/kafka-at-most-once/app"),
	}
}

// Process implements queue.Processor interface for at-most-once processing.
//
// Note: In at-most-once semantics, messages are acknowledged BEFORE processing.
// If this method fails, the message has already been acknowledged and will be lost.
// This is acceptable for non-critical data like metrics.
func (p *MetricsProcessor) Process(ctx context.Context, msg *MetricMessage) error {
	// Validate metric fields
	if msg.Name == "" {
		return fmt.Errorf("invalid metric: empty name")
	}
	if msg.Timestamp <= 0 {
		return fmt.Errorf("invalid metric: timestamp must be positive")
	}

	// Log the metric being processed
	p.log.InfoContext(ctx, "processing metric",
		slog.String("name", msg.Name),
		slog.Float64("value", msg.Value),
		slog.Int64("timestamp", msg.Timestamp),
		slog.Any("tags", msg.Tags),
	)

	// Simulate processing time (e.g., aggregation, storage, etc.)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(rand.IntN(351)+50) * time.Millisecond):
	}

	// In a real application, you might:
	// 1. Aggregate metrics in memory
	// 2. Update a time-series database
	// 3. Update a cache
	// 4. Send to a monitoring system
	//
	// Note: No duplicate detection needed - at-most-once delivery
	// means each message is processed at most once, and message loss
	// on failure is acceptable for metrics/logs.

	p.log.InfoContext(ctx, "metric processed successfully",
		slog.String("name", msg.Name),
	)

	return nil
}

// DecodingProcessor is a middleware processor that decodes Kafka records
// into typed messages and delegates to a business logic processor.
//
// This demonstrates the middleware pattern for message decoding in the
// Kafka runtime that works with raw kafka.Message objects.
type DecodingProcessor struct {
	decoder func([]byte) (*MetricMessage, error)
	handler *MetricsProcessor
}

// Process implements queue.Processor[kafka.Message].
//
// It decodes the Kafka record's value into a MetricMessage and then
// delegates to the MetricsProcessor for business logic.
func (d *DecodingProcessor) Process(ctx context.Context, msg kafka.Message) error {
	// Decode the Kafka message bytes
	decodedMsg, err := d.decoder(msg.Value)
	if err != nil {
		return fmt.Errorf("failed to decode message: %w", err)
	}

	// Delegate to business logic processor
	// Note: In at-most-once processing, the message has already been acknowledged
	// before this method is called, so any error here results in message loss.
	return d.handler.Process(ctx, decodedMsg)
}

// decodeMetric deserializes JSON bytes into MetricMessage.
func decodeMetric(data []byte) (*MetricMessage, error) {
	var msg MetricMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("failed to decode metric message: %w", err)
	}
	return &msg, nil
}
