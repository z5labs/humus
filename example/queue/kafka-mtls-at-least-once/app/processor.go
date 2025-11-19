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

// OrderMessage represents an order to be processed.
type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	Amount    float64 `json:"amount"`
	ProductID string  `json:"product_id"`
	Quantity  int     `json:"quantity"`
}

// OrderProcessor processes order messages.
//
// This is a simple example processor that demonstrates message processing
// in an at-least-once delivery scenario.
type OrderProcessor struct {
	log *slog.Logger
}

// NewOrderProcessor creates a new order processor.
func NewOrderProcessor() *OrderProcessor {
	return &OrderProcessor{
		log: humus.Logger("github.com/z5labs/humus/example/queue/kafka-mtls-at-least-once/app"),
	}
}

// Process implements queue.Processor interface.
func (p *OrderProcessor) Process(ctx context.Context, msg *OrderMessage) error {
	// Validate order
	if msg.Amount <= 0 {
		return fmt.Errorf("invalid order amount: %f", msg.Amount)
	}
	if msg.Quantity <= 0 {
		return fmt.Errorf("invalid order quantity: %d", msg.Quantity)
	}

	// Process the order
	p.log.InfoContext(ctx, "processing order",
		slog.String("order_id", msg.OrderID),
		slog.Float64("amount", msg.Amount),
		slog.String("product_id", msg.ProductID),
		slog.Int("quantity", msg.Quantity),
	)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(rand.IntN(351)+50) * time.Millisecond):
	}

	// In a real application, you would:
	// 1. Check if order already exists in database (idempotency)
	// 2. Process payment
	// 3. Update inventory
	// 4. Create shipping record
	// 5. Send confirmation email
	//
	// All in a transactional manner to ensure consistency

	p.log.InfoContext(ctx, "order processed successfully",
		slog.String("order_id", msg.OrderID),
	)

	return nil
}

// DecodingProcessor is a middleware processor that decodes Kafka records
// into typed messages and delegates to a business logic processor.
//
// This demonstrates the middleware pattern for message decoding in the
// refactored Kafka runtime that works with raw *kgo.Record objects.
type DecodingProcessor struct {
	decoder func([]byte) (*OrderMessage, error)
	handler *OrderProcessor
}

// Process implements queue.Processor[*kgo.Record].
//
// It decodes the Kafka record's value into an OrderMessage and then
// delegates to the OrderProcessor for business logic.
func (d *DecodingProcessor) Process(ctx context.Context, msg kafka.Message) error {
	// Decode the Kafka message bytes
	decodedMsg, err := d.decoder(msg.Value)
	if err != nil {
		return fmt.Errorf("failed to decode message: %w", err)
	}

	// Delegate to business logic processor
	return d.handler.Process(ctx, decodedMsg)
}

// decodeOrder deserializes JSON bytes into OrderMessage.
func decodeOrder(data []byte) (*OrderMessage, error) {
	var msg OrderMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("failed to decode order message: %w", err)
	}
	return &msg, nil
}
