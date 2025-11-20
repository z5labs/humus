// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"log/slog"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/queue"

	"github.com/sourcegraph/conc/pool"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel"
)

// atMostOnceMessagesHandler implements at-most-once delivery semantics for Kafka messages.
// Messages are committed to Kafka before processing, which means:
//   - If processing fails, the message is lost (already committed)
//   - Lower latency since commits happen immediately
//   - Suitable for non-critical data where message loss is acceptable
//
// Use cases:
//   - Metrics and monitoring data
//   - Logging and audit trails (non-critical)
//   - Cache updates
//   - Analytics events where exact-once is not required
type atMostOnceMessagesHandler struct {
	log       *slog.Logger
	tracer    *kotel.Tracer
	committer recordsCommitter
	processor queue.Processor[Message]
	metrics   *metricsRecorder
}

func newAtMostOnceMessagesHandler(
	groupId string,
	processor queue.Processor[Message],
) func(recordsCommitter) recordsHandler {
	return func(committer recordsCommitter) recordsHandler {
		metrics, err := newMetricsRecorder()
		if err != nil {
			panic(err) // Metrics initialization failure is a fatal error
		}

		return atMostOnceMessagesHandler{
			log: humus.Logger("github.com/z5labs/humus/queue/kafka").With(GroupIDAttr(groupId)),
			tracer: kotel.NewTracer(
				kotel.TracerProvider(otel.GetTracerProvider()),
				kotel.TracerPropagator(otel.GetTextMapPropagator()),
				kotel.LinkSpans(),
				kotel.ConsumerGroup(groupId),
			),
			committer: committer,
			processor: processor,
			metrics:   metrics,
		}
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
		o.topics[topic] = newAtMostOnceMessagesHandler(o.groupId, processor)
	}
}

// Handle processes a batch of Kafka records using at-most-once semantics.
// Records are committed first, then processed concurrently. Processing errors are logged but
// do not prevent the commit, meaning messages may be lost on processing failure.
// Since records are already committed, they can be processed in parallel for better throughput.
func (h atMostOnceMessagesHandler) Handle(ctx context.Context, records []*kgo.Record) error {
	// Commit records first (at-most-once: commit before processing)
	err := h.committer.CommitRecords(ctx, records...)
	if err != nil {
		h.log.ErrorContext(
			ctx,
			"failed to commit kafka records",
			slog.Any("error", err),
		)
		return err
	}

	// Record commit metrics (group by topic and partition)
	if len(records) > 0 {
		commitCounts := make(map[topicPartition]int)
		for _, record := range records {
			tp := topicPartition{topic: record.Topic, partition: record.Partition}
			commitCounts[tp]++
		}
		for tp, count := range commitCounts {
			h.metrics.recordMessagesCommitted(ctx, tp.topic, tp.partition, count)
		}
	}

	// Process all records concurrently after commit
	// Since records are already committed, we can process them in parallel
	p := pool.New().WithContext(ctx)

	for _, record := range records {
		p.Go(func(ctx context.Context) error {
			if record.Context == nil {
				record.Context = ctx
			}

			h.processRecord(record)

			return nil // Don't propagate errors - messages are already committed
		})
	}

	return p.Wait()
}

func (h atMostOnceMessagesHandler) processRecord(record *kgo.Record) {
	spanCtx, span := h.tracer.WithProcessSpan(record)
	defer span.End()

	headers := make([]Header, len(record.Headers))
	for i, hdr := range record.Headers {
		headers[i] = Header{
			Key:   hdr.Key,
			Value: hdr.Value,
		}
	}

	err := h.processor.Process(spanCtx, Message{
		Headers:   headers,
		Key:       record.Key,
		Value:     record.Value,
		Timestamp: record.Timestamp,
		Topic:     record.Topic,
		Partition: record.Partition,
		Offset:    record.Offset,
	})
	if err != nil {
		h.log.ErrorContext(
			spanCtx,
			"failed to process kafka message",
			TopicAttr(record.Topic),
			PartitionAttr(record.Partition),
			OffsetAttr(record.Offset),
			slog.Any("error", err),
		)
		h.metrics.recordProcessingFailure(spanCtx, record.Topic, record.Partition)
	} else {
		h.metrics.recordMessageProcessed(spanCtx, record.Topic, record.Partition)
	}
}
