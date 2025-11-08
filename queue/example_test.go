// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package queue_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/z5labs/humus/queue"
)

// MetricsMessage represents a metrics data point
type MetricsMessage struct {
	Name  string
	Value float64
}

// MetricsConsumer simulates consuming metrics from a queue
type MetricsConsumer struct {
	messages []MetricsMessage
	index    int
}

func (c *MetricsConsumer) Consume(ctx context.Context) (MetricsMessage, error) {
	if c.index >= len(c.messages) {
		return MetricsMessage{}, queue.EOQ
	}
	msg := c.messages[c.index]
	c.index++
	return msg, nil
}

// MetricsProcessor processes metrics (may fail occasionally)
type MetricsProcessor struct{}

func (p *MetricsProcessor) Process(ctx context.Context, msg MetricsMessage) error {
	// Simulate occasional processing failure
	if msg.Value < 0 {
		return fmt.Errorf("invalid metric value: %f", msg.Value)
	}
	// Process the metric (e.g., send to monitoring system)
	return nil
}

// MetricsAcknowledger acknowledges metrics
type MetricsAcknowledger struct{}

func (a *MetricsAcknowledger) Acknowledge(ctx context.Context, msg MetricsMessage) error {
	// Acknowledge receipt (e.g., delete from SQS)
	return nil
}

// Example_processAtMostOnce demonstrates using ProcessAtMostOnce for metrics collection.
//
// At-most-once processing is suitable for metrics where occasional data loss is acceptable
// and performance is more important than reliability. Messages are acknowledged before
// processing, so processing failures result in lost messages.
func Example_processAtMostOnce() {
	ctx := context.Background()

	// Setup consumer, processor, and acknowledger
	consumer := &MetricsConsumer{
		messages: []MetricsMessage{
			{Name: "cpu.usage", Value: 45.2},
			{Name: "memory.usage", Value: 78.5},
			{Name: "disk.invalid", Value: -1.0}, // This will fail processing
			{Name: "network.bytes", Value: 1024.0},
		},
	}
	processor := &MetricsProcessor{}
	acknowledger := &MetricsAcknowledger{}

	// Create at-most-once processor
	itemProcessor := queue.ProcessAtMostOnce(consumer, processor, acknowledger)

	// Process messages until queue is exhausted
	for {
		err := itemProcessor.ProcessItem(ctx)
		if errors.Is(err, queue.EOQ) {
			fmt.Println("Queue exhausted, shutting down")
			break
		}
		if err != nil {
			// Message was already acknowledged and lost - continue processing
			fmt.Printf("Processing error (message lost): %v\n", err)
			continue
		}
	}

	// Output:
	// Processing error (message lost): invalid metric value: -1.000000
	// Queue exhausted, shutting down
}

// OrderMessage represents an order to be processed
type OrderMessage struct {
	OrderID string
	Amount  float64
}

// OrderConsumer simulates consuming orders from a queue
type OrderConsumer struct {
	messages []OrderMessage
	index    int
}

func (c *OrderConsumer) Consume(ctx context.Context) (OrderMessage, error) {
	if c.index >= len(c.messages) {
		return OrderMessage{}, queue.EOQ
	}
	msg := c.messages[c.index]
	c.index++
	return msg, nil
}

// OrderProcessor processes orders idempotently
type OrderProcessor struct {
	processed map[string]bool
}

func (p *OrderProcessor) Process(ctx context.Context, msg OrderMessage) error {
	// Idempotent check - skip if already processed
	if p.processed[msg.OrderID] {
		return nil
	}

	// Process the order (e.g., charge payment, update inventory)
	if msg.Amount <= 0 {
		return fmt.Errorf("invalid order amount: %f", msg.Amount)
	}

	p.processed[msg.OrderID] = true
	return nil
}

// OrderAcknowledger acknowledges orders
type OrderAcknowledger struct{}

func (a *OrderAcknowledger) Acknowledge(ctx context.Context, msg OrderMessage) error {
	// Acknowledge successful processing (e.g., commit Kafka offset)
	return nil
}

// Example_processAtLeastOnce demonstrates using ProcessAtLeastOnce for order processing.
//
// At-least-once processing is suitable for critical operations where reliability is more
// important than avoiding duplicate processing. Messages are acknowledged only after
// successful processing, so processing failures result in retry. The processor must be
// idempotent to handle duplicate messages.
func Example_processAtLeastOnce() {
	ctx := context.Background()

	// Setup consumer, processor, and acknowledger
	consumer := &OrderConsumer{
		messages: []OrderMessage{
			{OrderID: "order-1", Amount: 99.99},
			{OrderID: "order-2", Amount: 149.50},
			{OrderID: "order-3", Amount: -10.00}, // This will fail and be retried
			{OrderID: "order-4", Amount: 75.25},
		},
	}
	processor := &OrderProcessor{
		processed: make(map[string]bool),
	}
	acknowledger := &OrderAcknowledger{}

	// Create at-least-once processor
	itemProcessor := queue.ProcessAtLeastOnce(consumer, processor, acknowledger)

	// Process messages until queue is exhausted
	for {
		err := itemProcessor.ProcessItem(ctx)
		if errors.Is(err, queue.EOQ) {
			fmt.Println("Queue exhausted, shutting down")
			break
		}
		if err != nil {
			// Message not acknowledged - will be retried
			fmt.Printf("Processing error (will retry): %v\n", err)
			// In production, you might want to implement backoff here
			break
		}
	}

	// Output:
	// Processing error (will retry): invalid order amount: -10.000000
}
