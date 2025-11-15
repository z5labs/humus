// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/z5labs/humus"

	"github.com/sourcegraph/conc/pool"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel"
)

// Header represents a Kafka message header.
type Header struct {
	Key   string
	Value []byte
}

// Message represents a Kafka message.
type Message struct {
	Key       []byte
	Value     []byte
	Headers   []Header
	Timestamp time.Time
	Topic     string
	Partition int32
	Offset    int64
	Attrs     uint8
}

type recordsHandler interface {
	Handle(context.Context, []*kgo.Record) error
}

type recordsCommitter interface {
	CommitRecords(context.Context, ...*kgo.Record) error
}

// Options represents configuration options for the Kafka runtime.
type Options struct {
	groupId              string
	topics               map[string]func(recordsCommitter) recordsHandler
	sessionTimeout       time.Duration
	rebalanceTimeout     time.Duration
	fetchMaxBytes        int32
	maxConcurrentFetches int
}

// Option defines a function type for configuring Kafka runtime options.
type Option func(*Options)

// SessionTimeout sets the session timeout for the Kafka consumer group.
func SessionTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.sessionTimeout = d
	}
}

// RebalanceTimeout sets the rebalance timeout for the Kafka consumer group.
func RebalanceTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.rebalanceTimeout = d
	}
}

// FetchMaxBytes sets the maximum total bytes to buffer from fetch responses across all partitions.
// Default is 50 MB if not set.
func FetchMaxBytes(bytes int32) Option {
	return func(o *Options) {
		o.fetchMaxBytes = bytes
	}
}

// MaxConcurrentFetches sets the maximum number of concurrent fetch requests.
// Default is unlimited if not set.
func MaxConcurrentFetches(fetches int) Option {
	return func(o *Options) {
		o.maxConcurrentFetches = fetches
	}
}

type Runtime struct {
	log                  *slog.Logger
	brokers              []string
	groupID              string
	topics               map[string]func(recordsCommitter) recordsHandler
	sessionTimeout       time.Duration
	rebalanceTimeout     time.Duration
	fetchMaxBytes        int32
	maxConcurrentFetches int
}

func NewRuntime(
	brokers []string,
	groupID string,
	opts ...Option,
) Runtime {
	cfg := &Options{
		groupId:              groupID,
		topics:               make(map[string]func(recordsCommitter) recordsHandler),
		sessionTimeout:       45 * time.Second,
		rebalanceTimeout:     30 * time.Second,
		fetchMaxBytes:        50 * 1024 * 1024, // 50 MB
		maxConcurrentFetches: 0,                // unlimited by default
	}
	for _, opt := range opts {
		opt(cfg)
	}

	if len(cfg.topics) == 0 {
		panic("kafka: at least one topic must be configured to consume from")
	}

	return Runtime{
		log:                  humus.Logger("kafka").With(GroupIDAttr(groupID)),
		brokers:              brokers,
		groupID:              groupID,
		topics:               cfg.topics,
		sessionTimeout:       cfg.sessionTimeout,
		rebalanceTimeout:     cfg.rebalanceTimeout,
		fetchMaxBytes:        cfg.fetchMaxBytes,
		maxConcurrentFetches: cfg.maxConcurrentFetches,
	}
}

type topicPartition struct {
	topic     string
	partition int32
}

type assignedPartition struct {
	topicPartition

	handler recordsHandler
}

type eventLoop struct {
	log *slog.Logger

	fetches            chan kgo.FetchTopic
	assignedPartitions chan assignedPartition
	lostPartitions     chan topicPartition
	revokedPartitions  chan topicPartition

	topicHandlers   map[string]func(recordsCommitter) recordsHandler
	topicPartitions map[topicPartition]chan []*kgo.Record
	partitionPool   *pool.ContextPool
}

func (r Runtime) ProcessQueue(ctx context.Context) error {
	loop := eventLoop{
		log:                r.log,
		fetches:            make(chan kgo.FetchTopic),
		assignedPartitions: make(chan assignedPartition),
		lostPartitions:     make(chan topicPartition),
		revokedPartitions:  make(chan topicPartition),
		topicHandlers:      r.topics,
		topicPartitions:    make(map[topicPartition]chan []*kgo.Record),
		partitionPool:      pool.New().WithContext(ctx),
	}

	clientOpts := []kgo.Opt{
		// kgo.WithLogger(kslog.New(r.log)), TODO: should only log at warning or above
		kgo.WithHooks(
			kotel.NewTracer(
				kotel.TracerProvider(otel.GetTracerProvider()),
				kotel.TracerPropagator(otel.GetTextMapPropagator()),
				kotel.LinkSpans(),
				kotel.ConsumerGroup(r.groupID),
			),
			kotel.NewMeter(
				kotel.MeterProvider(otel.GetMeterProvider()),
				kotel.WithMergedConnectsMeter(),
			),
		),
		kgo.SeedBrokers(r.brokers...),
		kgo.ConsumerGroup(r.groupID),
		kgo.ConsumeTopics(slices.Collect(maps.Keys(r.topics))...),
		kgo.Balancers(kgo.CooperativeStickyBalancer()),
		kgo.SessionTimeout(r.sessionTimeout),
		kgo.RebalanceTimeout(r.rebalanceTimeout),
		kgo.FetchMaxBytes(r.fetchMaxBytes),
		kgo.MaxConcurrentFetches(r.maxConcurrentFetches),
		kgo.DisableAutoCommit(),
		kgo.OnPartitionsAssigned(loop.onPartitionsAssigned(ctx)),
		kgo.OnPartitionsRevoked(loop.onPartitionsRevoked(ctx)),
		kgo.OnPartitionsLost(loop.onPartitionsLost(ctx)),
	}

	client, err := kgo.NewClient(clientOpts...)
	if err != nil {
		return fmt.Errorf("kafka: failed to create client: %w", err)
	}

	topicHandlers := make(map[string]recordsHandler)
	for topic, f := range r.topics {
		topicHandlers[topic] = f(client)
	}

	p := pool.New().WithContext(ctx)
	p.Go(loop.fetchRecords(client))
	p.Go(loop.run)

	return p.Wait()
}

type onPartitionCallback func(ctx context.Context, client *kgo.Client, partitions map[string][]int32)

func (loop eventLoop) onPartitionsAssigned(ctx context.Context) onPartitionCallback {
	return func(_ context.Context, client *kgo.Client, assigned map[string][]int32) {
		for topic, partitions := range assigned {
			for _, partition := range partitions {
				handler := loop.topicHandlers[topic](client)

				ap := assignedPartition{
					topicPartition: topicPartition{topic: topic, partition: partition},
					handler:        handler,
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

func (loop eventLoop) onPartitionsLost(ctx context.Context) onPartitionCallback {
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

func (loop eventLoop) onPartitionsRevoked(ctx context.Context) onPartitionCallback {
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
	Close()
	PollFetches(context.Context) kgo.Fetches
}

func (loop eventLoop) fetchRecords(client pollFetcher) func(context.Context) error {
	return func(ctx context.Context) error {
		defer client.Close()

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			fetches := client.PollFetches(ctx)
			for _, fetch := range fetches {
				for _, topic := range fetch.Topics {
					select {
					case <-ctx.Done():
						return ctx.Err()
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
			return loop.shutdown()
		}
	}
}

func (loop eventLoop) tick(ctx context.Context) error {
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

func (loop eventLoop) handleAssignedPartition(ctx context.Context, ap assignedPartition) error {
	loop.log.InfoContext(
		ctx,
		"topic partition assigned",
		TopicAttr(ap.topic),
		PartitionAttr(ap.partition),
	)

	records := make(chan []*kgo.Record)
	loop.topicPartitions[ap.topicPartition] = records

	loop.partitionPool.Go(processRecords(records, ap.handler))

	return nil
}

func processRecords(recordsCh <-chan []*kgo.Record, handler recordsHandler) func(context.Context) error {
	return func(ctx context.Context) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case records, ok := <-recordsCh:
				if !ok {
					return nil
				}

				err := handler.Handle(ctx, records)
				if err != nil {
					return err
				}
			}
		}
	}
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

func (loop eventLoop) handleFetch(ctx context.Context, fetch kgo.FetchTopic) error {
	for _, partition := range fetch.Partitions {
		tp := topicPartition{topic: fetch.Topic, partition: partition.Partition}
		recordCh, exists := loop.topicPartitions[tp]
		if !exists {
			loop.log.WarnContext(
				ctx,
				"topic partition not found for fetched records",
				TopicAttr(tp.topic),
				PartitionAttr(tp.partition),
			)
			continue
		}

		records := partition.Records
		select {
		case <-ctx.Done():
			return ctx.Err()
		case recordCh <- records:
		}
	}

	return nil
}
