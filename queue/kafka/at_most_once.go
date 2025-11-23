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

type atMostOnceOrchestrator struct {
	groupId   string
	processor queue.Processor[Message]
}

func newAtMostOnceOrchestrator(
	groupID string,
	processor queue.Processor[Message],
) partitionOrchestrator {
	return atMostOnceOrchestrator{
		groupId:   groupID,
		processor: processor,
	}
}

func (o atMostOnceOrchestrator) Orchestrate(
	consumer queue.Consumer[fetch],
	acknowledger queue.Acknowledger[[]*kgo.Record],
) queue.Runtime {
	log := logger().With(GroupIDAttr(o.groupId))
	metrics := initConsumerMetrics(log)

	return atMostOncePartitionRuntime{
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

type atMostOncePartitionRuntime struct {
	log               *slog.Logger
	tracer            trace.Tracer
	consumer          queue.Consumer[fetch]
	processor         recordProcessor
	acknowledger      queue.Acknowledger[[]*kgo.Record]
	messagesCommitted metric.Int64Counter
}

func (rt atMostOncePartitionRuntime) ProcessQueue(ctx context.Context) error {
	p := pool.New().WithContext(ctx)

	fetchCh := make(chan fetch)
	p.Go(consumeFetches(rt.log, rt.consumer, fetchCh))

	recordCh := make(chan *kgo.Record)
	p.Go(rt.acknowledgeFetchs(recordCh, fetchCh))

	p.Go(rt.processRecords(recordCh))

	return p.Wait()
}

func (rt atMostOncePartitionRuntime) acknowledgeFetchs(recordCh chan<- *kgo.Record, fetchCh <-chan fetch) func(context.Context) error {
	return func(ctx context.Context) error {
		defer close(recordCh)

		for f := range fetchCh {
			err := rt.acknowledger.Acknowledge(ctx, f.records)
			if err != nil {
				rt.log.ErrorContext(
					ctx,
					"failed to commit kafka records",
					slog.Any("error", err),
				)

				// TODO: how should this be handled?
				continue
			}

			rt.messagesCommitted.Add(ctx, int64(len(f.records)), metric.WithAttributes(
				semconv.MessagingSystemKafka,
				semconv.MessagingDestinationName(f.topic),
				semconv.MessagingDestinationPartitionID(strconv.FormatInt(int64(f.partition), 10)),
			))

			for _, record := range f.records {
				select {
				case <-ctx.Done():
					rt.log.WarnContext(
						ctx,
						"context cancelled after acknowledging records",
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

func (rt atMostOncePartitionRuntime) processRecords(recordCh <-chan *kgo.Record) func(context.Context) error {
	return func(ctx context.Context) error {
		p := pool.New().WithContext(ctx)

		for record := range recordCh {
			p.Go(func(ctx context.Context) error {
				rt.processor.process(ctx, record)
				return nil
			})
		}

		return p.Wait()
	}
}
