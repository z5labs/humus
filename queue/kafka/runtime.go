// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/humus/queue"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

func consumeFetches(log *slog.Logger, consumer queue.Consumer[fetch], fetchCh chan<- fetch) func(context.Context) error {
	return func(ctx context.Context) error {
		defer close(fetchCh)

		for {
			f, err := consumer.Consume(ctx)
			if errors.Is(err, queue.ErrEndOfQueue) {
				log.InfoContext(ctx, "encountered end of queue")
				return nil
			}
			if err != nil {
				return err
			}

			select {
			case <-ctx.Done():
				log.WarnContext(ctx, "context cancelled while consuming fetch")
				return nil
			case fetchCh <- f:
			}
		}
	}
}

type recordProcessor struct {
	log               *slog.Logger
	tracer            trace.Tracer
	processor         queue.Processor[Message]
	messagesProcessed metric.Int64Counter
}

func (rp recordProcessor) process(ctx context.Context, record *kgo.Record) {
	topicAttr := semconv.MessagingDestinationName(record.Topic)
	partitionIDAttr := semconv.MessagingDestinationPartitionID(strconv.FormatInt(int64(record.Partition), 10))
	spanOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			semconv.MessagingSystemKafka,
			semconv.MessagingOperationTypeProcess,
			topicAttr,
			partitionIDAttr,
			semconv.MessagingKafkaOffset(int(record.Offset)),
		),
	}

	if s := trace.SpanContextFromContext(record.Context); s.IsValid() {
		spanOpts = append(spanOpts, trace.WithLinks(trace.Link{SpanContext: s}))
	}

	spanCtx, span := rp.tracer.Start(ctx, "process "+record.Topic, spanOpts...)
	defer span.End()

	headers := make([]Header, len(record.Headers))
	for i, hdr := range record.Headers {
		headers[i] = Header{
			Key:   hdr.Key,
			Value: hdr.Value,
		}
	}

	err := rp.processor.Process(spanCtx, Message{
		Headers:   headers,
		Key:       record.Key,
		Value:     record.Value,
		Timestamp: record.Timestamp,
		Topic:     record.Topic,
		Partition: record.Partition,
		Offset:    record.Offset,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		rp.log.ErrorContext(
			spanCtx,
			"failed to process kafka record",
			TopicAttr(record.Topic),
			PartitionAttr(record.Partition),
			OffsetAttr(record.Offset),
			slog.Any("error", err),
		)
	}

	rp.messagesProcessed.Add(spanCtx, 1, metric.WithAttributes(
		semconv.MessagingSystemKafka,
		topicAttr,
		partitionIDAttr,
		attribute.String("messaging.process.status", processStatus(err)),
	))
}

func processStatus(err error) string {
	if err != nil {
		return "failure"
	}
	return "success"
}
