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

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel"
)

type atLeastOnceMessagesHandler struct {
	log       *slog.Logger
	tracer    *kotel.Tracer
	committer recordsCommitter
	processor queue.Processor[Message]
	metrics   *metricsRecorder
}

func newAtLeastOnceMessagesHandler(
	groupId string,
	processor queue.Processor[Message],
) func(recordsCommitter) recordsHandler {
	return func(committer recordsCommitter) recordsHandler {
		metrics, err := newMetricsRecorder()
		if err != nil {
			panic(err) // Metrics initialization failure is a fatal error
		}

		return atLeastOnceMessagesHandler{
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

// AtLeastOnce configures the Kafka runtime to process messages from the specified topic
func AtLeastOnce(topic string, processor queue.Processor[Message]) Option {
	return func(o *Options) {
		o.topics[topic] = newAtLeastOnceMessagesHandler(o.groupId, processor)
	}
}

func (h atLeastOnceMessagesHandler) Handle(ctx context.Context, records []*kgo.Record) error {
	for _, record := range records {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if record.Context == nil {
			record.Context = ctx
		}

		h.processRecord(record)
	}

	err := h.committer.CommitRecords(ctx, records...)
	if err == nil && len(records) > 0 {
		// Record commit metrics (group by partition)
		commitCounts := make(map[int32]int)
		var topic string
		for _, record := range records {
			topic = record.Topic
			commitCounts[record.Partition]++
		}
		for partition, count := range commitCounts {
			h.metrics.recordMessagesCommitted(ctx, topic, partition, count)
		}
	}

	return err
}

func (h atLeastOnceMessagesHandler) processRecord(record *kgo.Record) {
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
