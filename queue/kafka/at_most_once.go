// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"log/slog"

	"github.com/z5labs/humus/queue"

	"github.com/sourcegraph/conc/pool"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// atMostOnceOrchestrator implements the partitionOrchestrator interface.
type atMostOnceOrchestrator struct {
	groupId   string
	processor queue.Processor[Message]
}

// newAtMostOnceOrchestrator creates a new at-most-once orchestrator.
func newAtMostOnceOrchestrator(
	groupID string,
	processor queue.Processor[Message],
) partitionOrchestrator {
	return atMostOnceOrchestrator{
		groupId:   groupID,
		processor: processor,
	}
}

// Orchestrate creates a Runtime that processes messages with at-most-once semantics.
func (o atMostOnceOrchestrator) Orchestrate(
	consumer queue.Consumer[fetch],
	acknowledger queue.Acknowledger[[]*kgo.Record],
) queue.Runtime {
	log := logger().With(GroupIDAttr(o.groupId))
	metrics := initConsumerMetrics(log)

	return atMostOncePartitionRuntime{
		log:                log,
		tracer:             tracer(),
		consumer:           consumer,
		processor:          o.processor,
		acknowledger:       acknowledger,
		messagesProcessed:  metrics.messagesProcessed,
		messagesCommitted:  metrics.messagesCommitted,
		processingFailures: metrics.processingFailures,
	}
}

// atMostOncePartitionRuntime implements queue.Runtime with at-most-once delivery semantics.
// It acknowledges records before processing them, which means messages may be lost on processing failures.
//
// With at-most-once semantics:
//   - Messages are committed to Kafka before processing begins
//   - If processing fails, the message is lost (already committed)
//   - Lower latency since commits happen immediately
//   - Suitable for non-critical data where message loss is acceptable
//
// Use cases:
//   - Metrics and monitoring data
//   - Logging and audit trails (non-critical)
//   - Cache updates
//   - Analytics events where exact-once is not required
type atMostOncePartitionRuntime struct {
	log                *slog.Logger
	tracer             trace.Tracer
	consumer           queue.Consumer[fetch]
	processor          queue.Processor[Message]
	acknowledger       queue.Acknowledger[[]*kgo.Record]
	messagesProcessed  metric.Int64Counter
	messagesCommitted  metric.Int64Counter
	processingFailures metric.Int64Counter
}

// ProcessQueue implements queue.Runtime interface.
// It consumes fetches, acknowledges them immediately, then processes all records concurrently (at-most-once semantics).
func (rt atMostOncePartitionRuntime) ProcessQueue(ctx context.Context) error {
	// Create pool once and use across all batches for better performance
	p := pool.New().WithContext(ctx)

	for {
		// Consume a fetch
		f, err := rt.consumer.Consume(ctx)
		if errors.Is(err, queue.ErrEndOfQueue) {
			// Wait for all pending tasks before returning
			return p.Wait()
		}
		if err != nil {
			return err
		}

		// Acknowledge all records before processing (at-most-once: commit first)
		err = rt.acknowledger.Acknowledge(ctx, f.records)
		if err != nil {
			rt.log.ErrorContext(
				ctx,
				"failed to commit kafka records",
				slog.Any("error", err),
			)
			return err
		}

		// Increment committed messages counter
		if rt.messagesCommitted != nil && len(f.records) > 0 {
			attrs := []attribute.KeyValue{
				attribute.String("messaging.destination.name", f.topic),
				attribute.Int("messaging.destination.partition.id", int(f.partition)),
			}
			rt.messagesCommitted.Add(ctx, int64(len(f.records)), metric.WithAttributes(attrs...))
		}

		// Process all records concurrently after commit
		// Since records are already committed, we can process them in parallel
		for _, record := range f.records {
			// Capture record in closure
			record := record
			p.Go(func(ctx context.Context) error {
				// Use the context from the pool instead of modifying the record
				// to avoid data races when processing multiple batches concurrently
				if record.Context == nil {
					// Create a new record with the context set
					recordWithContext := *record
					recordWithContext.Context = ctx
					rt.processRecord(&recordWithContext)
				} else {
					rt.processRecord(record)
				}

				return nil // Don't propagate errors - messages are already committed
			})
		}
	}
}

// processRecord processes a single Kafka record with tracing.
func (rt *atMostOncePartitionRuntime) processRecord(record *kgo.Record) {
	spanCtx, span := rt.tracer.Start(
		record.Context,
		"kafka.process",
		trace.WithSpanKind(trace.SpanKindConsumer),
	)
	defer span.End()

	headers := make([]Header, len(record.Headers))
	for i, hdr := range record.Headers {
		headers[i] = Header{
			Key:   hdr.Key,
			Value: hdr.Value,
		}
	}

	err := rt.processor.Process(spanCtx, Message{
		Headers:   headers,
		Key:       record.Key,
		Value:     record.Value,
		Timestamp: record.Timestamp,
		Topic:     record.Topic,
		Partition: record.Partition,
		Offset:    record.Offset,
	})

	// Increment processed messages counter
	attrs := []attribute.KeyValue{
		attribute.String("messaging.destination.name", record.Topic),
		attribute.Int("messaging.destination.partition.id", int(record.Partition)),
	}
	if rt.messagesProcessed != nil {
		rt.messagesProcessed.Add(spanCtx, 1, metric.WithAttributes(attrs...))
	}

	if err != nil {
		// Increment failure counter
		if rt.processingFailures != nil {
			failureAttrs := append(attrs, attribute.String("error.type", errorType(err)))
			rt.processingFailures.Add(spanCtx, 1, metric.WithAttributes(failureAttrs...))
		}

		rt.log.ErrorContext(
			spanCtx,
			"failed to process kafka message",
			TopicAttr(record.Topic),
			PartitionAttr(record.Partition),
			OffsetAttr(record.Offset),
			slog.Any("error", err),
		)
	}
}

// AtMostOnce configures the Kafka runtime to process messages from the specified topic
// using at-most-once delivery semantics.
//
// With at-most-once semantics, messages are committed to Kafka before processing begins.
// This means that if processing fails, the message is lost since it has already been
// committed and will not be redelivered.
//
// This approach provides:
//   - Lower latency (commits happen immediately)
//   - Higher throughput (no waiting for processing to complete)
//   - Risk of message loss on processing failures
//
// Use at-most-once when:
//   - Message loss is acceptable
//   - Performance is critical
//   - Data is non-critical (metrics, logs, cache updates)
//
// The processor will receive messages even if commit fails, but the consumer group
// offset will not advance, potentially causing duplicate processing on restart.
func AtMostOnce(topic string, processor queue.Processor[Message]) Option {
	return func(o *Options) {
		o.topics[topic] = newAtMostOnceOrchestrator(o.groupId, processor)
	}
}
