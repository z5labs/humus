// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sourcegraph/conc/pool"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/humus/queue"
)

type topicPartition struct {
	topic     string
	partition int32
}

type fetch struct {
	topicPartition

	records []*kgo.Record
}

type partitionOrchestrator interface {
	Orchestrate(queue.Consumer[fetch], queue.Acknowledger[[]*kgo.Record]) queue.Runtime
}

type assignedPartition struct {
	topicPartition

	orchestrator partitionOrchestrator
	committer    recordsCommitter
}

type eventLoop struct {
	log *slog.Logger

	fetches            chan kgo.FetchTopic
	assignedPartitions chan assignedPartition
	lostPartitions     chan topicPartition
	revokedPartitions  chan topicPartition

	topicOrchestrators map[string]partitionOrchestrator
	topicPartitions    map[topicPartition]chan fetch
	partitionPool      *pool.ContextPool
}

func newEventLoop(ctx context.Context, log *slog.Logger, topics map[string]partitionOrchestrator) eventLoop {
	return eventLoop{
		log:                log,
		fetches:            make(chan kgo.FetchTopic),
		assignedPartitions: make(chan assignedPartition),
		lostPartitions:     make(chan topicPartition),
		revokedPartitions:  make(chan topicPartition),
		topicOrchestrators: topics,
		topicPartitions:    make(map[topicPartition]chan fetch),
		partitionPool:      pool.New().WithContext(ctx),
	}
}

type onPartitionCallback[C any] func(ctx context.Context, client C, partitions map[string][]int32)

type recordsCommitter interface {
	CommitRecords(context.Context, ...*kgo.Record) error
}

func (loop eventLoop) onPartitionsAssigned(ctx context.Context) onPartitionCallback[recordsCommitter] {
	return func(_ context.Context, client recordsCommitter, assigned map[string][]int32) {
		for topic, partitions := range assigned {
			for _, partition := range partitions {
				orchestrator := loop.topicOrchestrators[topic]

				ap := assignedPartition{
					topicPartition: topicPartition{topic: topic, partition: partition},
					orchestrator:   orchestrator,
					committer:      client,
				}

				select {
				case <-ctx.Done():
					return
				case loop.assignedPartitions <- ap:
				}
			}
		}
	}
}

func (loop eventLoop) onPartitionsLost(ctx context.Context) onPartitionCallback[*kgo.Client] {
	return func(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
		for topic, partitions := range lost {
			for _, partition := range partitions {
				select {
				case <-ctx.Done():
					return
				case loop.lostPartitions <- topicPartition{topic: topic, partition: partition}:
				}
			}
		}
	}
}

func (loop eventLoop) onPartitionsRevoked(ctx context.Context) onPartitionCallback[*kgo.Client] {
	return func(_ context.Context, _ *kgo.Client, revoked map[string][]int32) {
		for topic, partitions := range revoked {
			for _, partition := range partitions {
				select {
				case <-ctx.Done():
					return
				case loop.revokedPartitions <- topicPartition{topic: topic, partition: partition}:
				}
			}
		}
	}
}

type pollFetcher interface {
	PollFetches(context.Context) kgo.Fetches
}

func (loop eventLoop) fetchRecords(client pollFetcher) func(context.Context) error {
	return func(ctx context.Context) error {
		for {
			select {
			case <-ctx.Done():
				loop.log.InfoContext(
					ctx,
					"stopped fetching",
					slog.Any("error", ctx.Err()),
				)
				return nil
			default:
			}

			fetches := client.PollFetches(ctx)
			for _, fetch := range fetches {
				for _, topic := range fetch.Topics {
					select {
					case <-ctx.Done():
						loop.log.InfoContext(
							ctx,
							"stopped fetching",
							slog.Any("error", ctx.Err()),
						)
						return nil
					case loop.fetches <- topic:
					}
				}
			}
		}
	}
}

func (loop eventLoop) shutdown() error {
	for _, ch := range loop.topicPartitions {
		close(ch)
	}

	return loop.partitionPool.Wait()
}

func (loop eventLoop) run(ctx context.Context) error {
	for {
		err := loop.tick(ctx)
		if err != nil {
			loop.log.InfoContext(ctx, "shutting down event loop", slog.Any("error", err))
			return loop.shutdown()
		}
	}
}

func (loop eventLoop) tick(ctx context.Context) error {
	fmt.Println("tick")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case tp := <-loop.assignedPartitions:
		return loop.handleAssignedPartition(ctx, tp)
	case tp := <-loop.lostPartitions:
		return loop.handleLostPartition(ctx, tp)
	case tp := <-loop.revokedPartitions:
		return loop.handleRevokedPartition(ctx, tp)
	case fetch := <-loop.fetches:
		return loop.handleFetch(ctx, fetch)
	}
}

type channelConsumer struct {
	fetches <-chan fetch
}

func (c *channelConsumer) Consume(ctx context.Context) (fetch, error) {
	select {
	case <-ctx.Done():
		return fetch{}, ctx.Err()
	case f, ok := <-c.fetches:
		if !ok {
			return fetch{}, queue.ErrEndOfQueue
		}

		return f, nil
	}
}

type committerAcknowledger struct {
	committer recordsCommitter
}

func (a *committerAcknowledger) Acknowledge(ctx context.Context, records []*kgo.Record) error {
	return a.committer.CommitRecords(ctx, records...)
}

func (loop eventLoop) handleAssignedPartition(ctx context.Context, ap assignedPartition) error {
	loop.log.InfoContext(
		ctx,
		"topic partition assigned",
		TopicAttr(ap.topic),
		PartitionAttr(ap.partition),
	)

	records := make(chan fetch)
	loop.topicPartitions[ap.topicPartition] = records

	// Create adapters for the orchestrator
	consumer := &channelConsumer{fetches: records}
	acknowledger := &committerAcknowledger{committer: ap.committer}

	// Create runtime from orchestrator
	runtime := ap.orchestrator.Orchestrate(consumer, acknowledger)

	// Run the runtime
	loop.partitionPool.Go(runtime.ProcessQueue)

	return nil
}

func (loop eventLoop) handleLostPartition(ctx context.Context, tp topicPartition) error {
	loop.log.InfoContext(
		ctx,
		"topic partition lost",
		TopicAttr(tp.topic),
		PartitionAttr(tp.partition),
	)

	recordCh, exists := loop.topicPartitions[tp]
	if !exists {
		loop.log.WarnContext(
			ctx,
			"topic partition not found for lost partition",
			TopicAttr(tp.topic),
			PartitionAttr(tp.partition),
		)
		return nil
	}

	close(recordCh)
	delete(loop.topicPartitions, tp)

	return nil
}

func (loop eventLoop) handleRevokedPartition(ctx context.Context, tp topicPartition) error {
	loop.log.InfoContext(
		ctx,
		"topic partition revoked",
		TopicAttr(tp.topic),
		PartitionAttr(tp.partition),
	)

	recordCh, exists := loop.topicPartitions[tp]
	if !exists {
		loop.log.WarnContext(
			ctx,
			"topic partition not found for revoked partition",
			TopicAttr(tp.topic),
			PartitionAttr(tp.partition),
		)
		return nil
	}

	close(recordCh)
	delete(loop.topicPartitions, tp)

	return nil
}

func (loop eventLoop) handleFetch(ctx context.Context, fetchTopic kgo.FetchTopic) error {
	for _, partition := range fetchTopic.Partitions {
		tp := topicPartition{topic: fetchTopic.Topic, partition: partition.Partition}
		fetchCh, exists := loop.topicPartitions[tp]
		if !exists {
			loop.log.WarnContext(
				ctx,
				"topic partition not found for fetched records",
				TopicAttr(tp.topic),
				PartitionAttr(tp.partition),
			)
			continue
		}

		f := fetch{topicPartition: tp, records: partition.Records}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case fetchCh <- f:
		}
	}

	return nil
}
