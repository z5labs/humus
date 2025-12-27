// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/z5labs/humus/queue"

	"github.com/sourcegraph/conc/pool"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
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

func (o atLeastOnceOrchestrator) Orchestrate(
	consumer queue.Consumer[fetch],
	acknowledger queue.Acknowledger[[]*kgo.Record],
) queue.QueueRuntime {
	log := logger().With(GroupIDAttr(o.groupId))
	metrics := initConsumerMetrics(log)

	return atLeastOncePartitionRuntime{
		log:      log,
		tracer:   tracer(),
		consumer: consumer,
		processor: recordProcessor{
			log:               log,
			tracer:            tracer(),
			processor:         o.processor,
			messagesProcessed: metrics.messagesProcessed,
		},
		acknowledger:      acknowledger,
		messagesCommitted: metrics.messagesCommitted,
	}
}

type atLeastOncePartitionRuntime struct {
	log               *slog.Logger
	tracer            trace.Tracer
	consumer          queue.Consumer[fetch]
	processor         recordProcessor
	acknowledger      queue.Acknowledger[[]*kgo.Record]
	messagesCommitted metric.Int64Counter
}

func (rt atLeastOncePartitionRuntime) ProcessQueue(ctx context.Context) error {
	p := pool.New().WithContext(ctx)

	fetchCh := make(chan fetch)
	p.Go(consumeFetches(rt.log, rt.consumer, fetchCh))

	recordCh := make(chan *kgo.Record)
	p.Go(rt.processFetches(recordCh, fetchCh))

	p.Go(rt.acknowledgeRecords(recordCh))

	return p.Wait()
}

func (rt atLeastOncePartitionRuntime) processFetches(recordCh chan<- *kgo.Record, fetchCh <-chan fetch) func(context.Context) error {
	return func(ctx context.Context) error {
		defer close(recordCh)

		for f := range fetchCh {
			for _, record := range f.records {
				rt.processor.process(ctx, record)

				select {
				case <-ctx.Done():
					rt.log.WarnContext(
						ctx,
						"context cancelled while processing records",
						slog.Any("error", ctx.Err()),
					)
					return nil
				case recordCh <- record:
				}
			}
		}

		return nil
	}
}

func (rt atLeastOncePartitionRuntime) acknowledgeRecords(recordCh <-chan *kgo.Record) func(context.Context) error {
	return func(ctx context.Context) error {
		for record := range recordCh {
			err := rt.acknowledger.Acknowledge(ctx, []*kgo.Record{record})
			if err != nil {
				rt.log.ErrorContext(
					ctx,
					"failed to commit kafka records",
					TopicAttr(record.Topic),
					PartitionAttr(record.Partition),
					OffsetAttr(record.Offset),
					slog.Any("error", err),
				)

				// TODO: how should this be handled?
				continue
			}

			rt.messagesCommitted.Add(ctx, 1, metric.WithAttributes(
				semconv.MessagingDestinationName(record.Topic),
				semconv.MessagingDestinationPartitionID(strconv.FormatInt(int64(record.Partition), 10)),
			))
		}
		return nil
	}
}
