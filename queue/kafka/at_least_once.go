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

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type atLeastOnceOrchestrator struct {
	groupId   string
	processor queue.Processor[Message]
}

func newAtLeastOnceOrchestrator(
	groupID string,
	processor queue.Processor[Message],
) partitionOrchestrator {
	return atLeastOnceOrchestrator{
		groupId:   groupID,
		processor: processor,
	}
}

// AtLeastOnce configures the Kafka runtime to process messages from the specified topic
// with at-least-once delivery semantics (process before acknowledge).
func AtLeastOnce(topic string, processor queue.Processor[Message]) Option {
	return func(o *Options) {
		o.topics[topic] = newAtLeastOnceOrchestrator(o.groupId, processor)
	}
}

func (o atLeastOnceOrchestrator) Orchestrate(
	consumer queue.Consumer[fetch],
	acknowledger queue.Acknowledger[[]*kgo.Record],
) queue.Runtime {
	log := logger().With(GroupIDAttr(o.groupId))
	metrics := initConsumerMetrics(log)

	return atLeastOncePartitionRuntime{
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

type atLeastOncePartitionRuntime struct {
	log                *slog.Logger
	tracer             trace.Tracer
	consumer           queue.Consumer[fetch]
	processor          queue.Processor[Message]
	acknowledger       queue.Acknowledger[[]*kgo.Record]
	messagesProcessed  metric.Int64Counter
	messagesCommitted  metric.Int64Counter
	processingFailures metric.Int64Counter
}

func (rt atLeastOncePartitionRuntime) ProcessQueue(ctx context.Context) error {
	for {
		// Consume a fetch
		f, err := rt.consumer.Consume(ctx)
		if errors.Is(err, queue.ErrEndOfQueue) {
			return nil
		}
		if err != nil {
			return err
		}

		// Process all records in the fetch
		for _, record := range f.records {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			if record.Context == nil {
				record.Context = ctx
			}

			rt.processRecord(record)
		}

		// Acknowledge all records after processing
		err = rt.acknowledger.Acknowledge(ctx, f.records)
		if err != nil {
			return err
		}

		// Increment committed messages counter
		if rt.messagesCommitted != nil && len(f.records) > 0 {
			attrs := []attribute.KeyValue{
				attribute.String("messaging.destination.name", f.topic),
				attribute.Int("messaging.destination.partition.id", int(f.partition)),
				attribute.String("delivery.semantics", "at_least_once"),
			}
			rt.messagesCommitted.Add(ctx, int64(len(f.records)), metric.WithAttributes(attrs...))
		}
	}
}

func (rt atLeastOncePartitionRuntime) processRecord(record *kgo.Record) {
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
		attribute.String("delivery.semantics", "at_least_once"),
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
