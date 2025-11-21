// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/z5labs/humus"

	"github.com/sourcegraph/conc/pool"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"github.com/twmb/franz-go/plugin/kslog"
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

// Options represents configuration options for the Kafka runtime.
type Options struct {
	groupId              string
	topics               map[string]partitionOrchestrator
	sessionTimeout       time.Duration
	rebalanceTimeout     time.Duration
	fetchMaxBytes        int32
	maxConcurrentFetches int
	tlsConfig            *tls.Config
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

// WithTLS configures TLS/mTLS for secure connections to Kafka brokers.
// Pass a fully configured *tls.Config with certificates, CA pool, and other TLS settings.
//
// Example:
//
//	// Load certificates
//	cert, err := tls.LoadX509KeyPair("client-cert.pem", "client-key.pem")
//	if err != nil {
//	    return err
//	}
//
//	caCert, err := os.ReadFile("ca-cert.pem")
//	if err != nil {
//	    return err
//	}
//	caCertPool := x509.NewCertPool()
//	caCertPool.AppendCertsFromPEM(caCert)
//
//	tlsConfig := &tls.Config{
//	    Certificates: []tls.Certificate{cert},
//	    RootCAs:      caCertPool,
//	    MinVersion:   tls.VersionTLS12,
//	}
//
//	runtime := kafka.NewRuntime(brokers, groupID,
//	    kafka.WithTLS(tlsConfig),
//	    kafka.AtLeastOnce(topic, processor),
//	)
func WithTLS(cfg *tls.Config) Option {
	return func(o *Options) {
		o.tlsConfig = cfg
	}
}

// Runtime represents the Kafka runtime for processing messages.
type Runtime struct {
	log                  *slog.Logger
	brokers              []string
	groupID              string
	topics               map[string]partitionOrchestrator
	sessionTimeout       time.Duration
	rebalanceTimeout     time.Duration
	fetchMaxBytes        int32
	maxConcurrentFetches int
	tlsConfig            *tls.Config
}

// NewRuntime creates a new Kafka runtime with the provided brokers, group ID, and options.
func NewRuntime(
	brokers []string,
	groupID string,
	opts ...Option,
) Runtime {
	cfg := &Options{
		groupId:              groupID,
		topics:               make(map[string]partitionOrchestrator),
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
		log:                  logger().With(GroupIDAttr(groupID)),
		brokers:              brokers,
		groupID:              groupID,
		topics:               cfg.topics,
		sessionTimeout:       cfg.sessionTimeout,
		rebalanceTimeout:     cfg.rebalanceTimeout,
		fetchMaxBytes:        cfg.fetchMaxBytes,
		maxConcurrentFetches: cfg.maxConcurrentFetches,
		tlsConfig:            cfg.tlsConfig,
	}
}

// ProcessQueue starts processing the Kafka queue.
func (r Runtime) ProcessQueue(ctx context.Context) error {
	loop := newEventLoop(ctx, r.log, r.topics)

	onPartitionAssigned := loop.onPartitionsAssigned(ctx)
	onPartitionRevoked := loop.onPartitionsRevoked(ctx)
	onPartitionLost := loop.onPartitionsLost(ctx)

	clientOpts := []kgo.Opt{
		kgo.WithLogger(kslog.New(humus.Logger("github.com/twmb/franz-go/pkg/kgo"))),
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
		kgo.OnPartitionsAssigned(func(ctx context.Context, c *kgo.Client, m map[string][]int32) {
			onPartitionAssigned(ctx, c, m)
		}),
		kgo.OnPartitionsRevoked(onPartitionRevoked),
		kgo.OnPartitionsLost(onPartitionLost),
	}

	// Configure TLS if provided
	if r.tlsConfig != nil {
		clientOpts = append(clientOpts, kgo.DialTLSConfig(r.tlsConfig))
	}

	client, err := kgo.NewClient(clientOpts...)
	if err != nil {
		return fmt.Errorf("kafka: failed to create client: %w", err)
	}
	defer client.Close()

	p := pool.New().WithContext(ctx)
	p.Go(loop.fetchRecords(client))
	p.Go(loop.run)

	return p.Wait()
}
