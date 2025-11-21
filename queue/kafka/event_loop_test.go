package kafka

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/z5labs/humus/queue"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
)

type partitionOrchestratorFunc func(queue.Consumer[fetch], queue.Acknowledger[[]*kgo.Record]) queue.Runtime

func (f partitionOrchestratorFunc) Orchestrate(
	consumer queue.Consumer[fetch],
	acknowledger queue.Acknowledger[[]*kgo.Record],
) queue.Runtime {
	return f(consumer, acknowledger)
}

type recordsCommitterFunc func(ctx context.Context, records ...*kgo.Record) error

func (f recordsCommitterFunc) CommitRecords(ctx context.Context, records ...*kgo.Record) error {
	return f(ctx, records...)
}

type pollFetcherFunc func(context.Context) kgo.Fetches

func (f pollFetcherFunc) PollFetches(ctx context.Context) kgo.Fetches {
	return f(ctx)
}

func TestEventLoop(t *testing.T) {
	t.Parallel()

	type callbacks struct {
		onLostPartition    onPartitionCallback[*kgo.Client]
		onRevokedPartition onPartitionCallback[*kgo.Client]
	}

	testCases := []struct {
		name                  string
		topicPartitions       []topicPartition
		partitionOrchestrator func(cancel func(), callbacks *callbacks) partitionOrchestratorFunc
	}{
		{
			name: "should close partition record channel on context cancellation",
			topicPartitions: []topicPartition{
				{topic: "test-topic", partition: 0},
			},
			partitionOrchestrator: func(cancel func(), _ *callbacks) partitionOrchestratorFunc {
				return func(
					consumer queue.Consumer[fetch],
					acknowledger queue.Acknowledger[[]*kgo.Record],
				) queue.Runtime {
					return queue.RuntimeFunc(func(ctx context.Context) error {
						for {
							_, err := consumer.Consume(ctx)
							if errors.Is(err, context.Canceled) {
								continue
							}
							if errors.Is(err, queue.ErrEndOfQueue) {
								break
							}

							cancel()
						}

						return nil
					})
				}
			},
		},
		{
			name: "should handle fetches for all assigned partitions",
			topicPartitions: []topicPartition{
				{topic: "test-topic", partition: 0},
				{topic: "test-topic", partition: 1},
			},
			partitionOrchestrator: func(cancel func(), _ *callbacks) partitionOrchestratorFunc {
				var mu sync.Mutex
				processedPartitions := make(map[topicPartition]struct{})

				return func(
					consumer queue.Consumer[fetch],
					acknowledger queue.Acknowledger[[]*kgo.Record],
				) queue.Runtime {
					return queue.RuntimeFunc(func(ctx context.Context) error {
						for {
							fetch, err := consumer.Consume(ctx)
							if errors.Is(err, context.Canceled) {
								continue
							}
							if errors.Is(err, queue.ErrEndOfQueue) {
								break
							}

							mu.Lock()
							processedPartitions[fetch.topicPartition] = struct{}{}
							if len(processedPartitions) == 2 {
								cancel()
							}
							mu.Unlock()
						}

						return nil
					})
				}
			},
		},
		{
			name: "should shutdown lost partition runtimes",
			topicPartitions: []topicPartition{
				{topic: "test-topic", partition: 0},
				{topic: "test-topic", partition: 1},
			},
			partitionOrchestrator: func(cancel func(), cbs *callbacks) partitionOrchestratorFunc {
				return func(
					consumer queue.Consumer[fetch],
					acknowledger queue.Acknowledger[[]*kgo.Record],
				) queue.Runtime {
					lost := false

					return queue.RuntimeFunc(func(ctx context.Context) error {
						for {
							fetch, err := consumer.Consume(ctx)
							if errors.Is(err, context.Canceled) {
								continue
							}
							if errors.Is(err, queue.ErrEndOfQueue) {
								cancel()
								break
							}

							if fetch.partition == 1 && !lost {
								go cbs.onLostPartition(ctx, nil, map[string][]int32{
									fetch.topic: {fetch.partition},
								})

								lost = true
							}
						}

						return nil
					})
				}
			},
		},
		{
			name: "should shutdown revoked partition runtimes",
			topicPartitions: []topicPartition{
				{topic: "test-topic", partition: 0},
				{topic: "test-topic", partition: 1},
			},
			partitionOrchestrator: func(cancel func(), cbs *callbacks) partitionOrchestratorFunc {
				return func(
					consumer queue.Consumer[fetch],
					acknowledger queue.Acknowledger[[]*kgo.Record],
				) queue.Runtime {
					revoked := false

					return queue.RuntimeFunc(func(ctx context.Context) error {
						for {
							fetch, err := consumer.Consume(ctx)
							if errors.Is(err, context.Canceled) {
								continue
							}
							if errors.Is(err, queue.ErrEndOfQueue) {
								cancel()
								break
							}

							if fetch.partition == 1 && !revoked {
								go cbs.onRevokedPartition(ctx, nil, map[string][]int32{
									fetch.topic: {fetch.partition},
								})

								revoked = true
							}
						}

						return nil
					})
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			log := slog.Default().With(slog.String("test", tc.name))

			cbs := &callbacks{}

			orchestrator := tc.partitionOrchestrator(cancel, cbs)

			var wg sync.WaitGroup
			topicOrchestrators := make(map[string]partitionOrchestrator)
			for _, tp := range tc.topicPartitions {
				wg.Add(1)

				topicOrchestrators[tp.topic] = partitionOrchestratorFunc(func(c queue.Consumer[fetch], a queue.Acknowledger[[]*kgo.Record]) queue.Runtime {
					runtime := orchestrator(c, a)

					return queue.RuntimeFunc(func(ctx context.Context) error {
						defer wg.Done()

						return runtime.ProcessQueue(ctx)
					})
				})
			}

			loop := newEventLoop(ctx, log, topicOrchestrators)
			cbs.onLostPartition = loop.onPartitionsLost(ctx)
			cbs.onRevokedPartition = loop.onPartitionsRevoked(ctx)

			errCh := make(chan error, 1)
			go func() {
				defer close(errCh)
				errCh <- loop.run(ctx)
			}()

			committer := recordsCommitterFunc(func(ctx context.Context, records ...*kgo.Record) error {
				return nil
			})

			assignedTopicPartitions := make(map[string][]int32)
			for _, tp := range tc.topicPartitions {
				assignedTopicPartitions[tp.topic] = append(assignedTopicPartitions[tp.topic], tp.partition)
			}

			assignPartitions := loop.onPartitionsAssigned(ctx)
			assignPartitions(ctx, committer, assignedTopicPartitions)

			var fetches kgo.Fetches
			for _, tp := range tc.topicPartitions {
				fetches = append(fetches, kgo.Fetch{
					Topics: []kgo.FetchTopic{
						{
							Topic: tp.topic,
							Partitions: []kgo.FetchPartition{
								{
									Partition: tp.partition,
									Records: []*kgo.Record{
										{
											Topic:     tp.topic,
											Partition: tp.partition,
											Offset:    1,
											Key:       []byte("key1"),
											Value:     []byte("value1"),
										},
									},
								},
							},
						},
					},
				})
			}

			pollFetcher := pollFetcherFunc(func(ctx context.Context) kgo.Fetches {
				return fetches
			})

			fetch := loop.fetchRecords(pollFetcher)
			err := fetch(ctx)
			require.Nil(t, err)

			// ensure all runtimes have completed
			wg.Wait()

			err = <-errCh
			require.Nil(t, err)
		})
	}
}
