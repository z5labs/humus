// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"maps"
	"os"
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

type recordsHandler interface {
	Handle(context.Context, []*kgo.Record) error
}

type recordsCommitter interface {
	CommitRecords(context.Context, ...*kgo.Record) error
}

// TLSConfig holds TLS/mTLS configuration for secure Kafka connections.
type TLSConfig struct {
	// Client certificate (PEM-encoded) - supports both file path and raw data
	// If CertFile is set, it will be loaded; otherwise CertData is used
	CertFile string
	CertData []byte

	// Client private key (PEM-encoded) - supports both file path and raw data
	// If KeyFile is set, it will be loaded; otherwise KeyData is used
	KeyFile string
	KeyData []byte

	// CA certificate (PEM-encoded) for verifying broker certificates
	// If CAFile is set, it will be loaded; otherwise CAData is used
	CAFile string
	CAData []byte

	// ServerName for SNI (Server Name Indication)
	// If empty, the broker hostname will be used
	ServerName string

	// MinVersion specifies the minimum TLS version (e.g., tls.VersionTLS12)
	// If 0, a default minimum version will be used
	MinVersion uint16

	// MaxVersion specifies the maximum TLS version
	// If 0, the highest version supported by the implementation is used
	MaxVersion uint16
}

// Options represents configuration options for the Kafka runtime.
type Options struct {
	groupId              string
	topics               map[string]func(recordsCommitter) recordsHandler
	sessionTimeout       time.Duration
	rebalanceTimeout     time.Duration
	fetchMaxBytes        int32
	maxConcurrentFetches int
	tlsConfig            *TLSConfig
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
// The TLSConfig supports both file paths and in-memory certificate data,
// making it suitable for both traditional file-based deployments and
// Kubernetes secret-based deployments.
//
// Example with file paths:
//
//	tlsCfg := kafka.TLSConfig{
//	    CertFile: "/path/to/client-cert.pem",
//	    KeyFile:  "/path/to/client-key.pem",
//	    CAFile:   "/path/to/ca-cert.pem",
//	}
//	runtime := kafka.NewRuntime(brokers, groupID,
//	    kafka.WithTLS(tlsCfg),
//	    kafka.AtLeastOnce(topic, processor),
//	)
//
// Example with in-memory data (e.g., from Kubernetes secrets):
//
//	tlsCfg := kafka.TLSConfig{
//	    CertData: certBytes,
//	    KeyData:  keyBytes,
//	    CAData:   caBytes,
//	}
//	runtime := kafka.NewRuntime(brokers, groupID,
//	    kafka.WithTLS(tlsCfg),
//	    kafka.AtLeastOnce(topic, processor),
//	)
func WithTLS(cfg TLSConfig) Option {
	return func(o *Options) {
		o.tlsConfig = &cfg
	}
}

// Runtime represents the Kafka runtime for processing messages.
type Runtime struct {
	log                  *slog.Logger
	brokers              []string
	groupID              string
	topics               map[string]func(recordsCommitter) recordsHandler
	sessionTimeout       time.Duration
	rebalanceTimeout     time.Duration
	fetchMaxBytes        int32
	maxConcurrentFetches int
	tlsConfig            *TLSConfig
}

// NewRuntime creates a new Kafka runtime with the provided brokers, group ID, and options.
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
		log:                  humus.Logger("github.com/z5labs/humus/queue/kafka").With(GroupIDAttr(groupID)),
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

// buildTLSConfig constructs a *tls.Config from TLSConfig.
// It supports loading certificates from files or using in-memory data.
func buildTLSConfig(cfg *TLSConfig) (*tls.Config, error) {
	if cfg == nil {
		return nil, nil
	}

	tlsConfig := &tls.Config{
		MinVersion: cfg.MinVersion,
		MaxVersion: cfg.MaxVersion,
		ServerName: cfg.ServerName,
	}

	// Load client certificate and key
	var certData, keyData []byte
	var err error

	// Load certificate
	if cfg.CertFile != "" {
		certData, err = os.ReadFile(cfg.CertFile)
		if err != nil {
			return nil, fmt.Errorf("kafka: failed to read client certificate file %q: %w", cfg.CertFile, err)
		}
	} else if len(cfg.CertData) > 0 {
		certData = cfg.CertData
	}

	// Load private key
	if cfg.KeyFile != "" {
		keyData, err = os.ReadFile(cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("kafka: failed to read client key file %q: %w", cfg.KeyFile, err)
		}
	} else if len(cfg.KeyData) > 0 {
		keyData = cfg.KeyData
	}

	// If both cert and key are provided, load them
	if len(certData) > 0 && len(keyData) > 0 {
		cert, err := tls.X509KeyPair(certData, keyData)
		if err != nil {
			return nil, fmt.Errorf("kafka: failed to load client certificate and key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate for broker verification
	var caData []byte
	if cfg.CAFile != "" {
		caData, err = os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("kafka: failed to read CA certificate file %q: %w", cfg.CAFile, err)
		}
	} else if len(cfg.CAData) > 0 {
		caData = cfg.CAData
	}

	if len(caData) > 0 {
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caData) {
			return nil, fmt.Errorf("kafka: failed to parse CA certificate")
		}
		tlsConfig.RootCAs = caCertPool
	}

	return tlsConfig, nil
}

// ProcessQueue starts processing the Kafka queue.
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
		kgo.OnPartitionsAssigned(loop.onPartitionsAssigned(ctx)),
		kgo.OnPartitionsRevoked(loop.onPartitionsRevoked(ctx)),
		kgo.OnPartitionsLost(loop.onPartitionsLost(ctx)),
	}

	// Configure TLS if provided
	if r.tlsConfig != nil {
		tlsCfg, err := buildTLSConfig(r.tlsConfig)
		if err != nil {
			return err
		}
		clientOpts = append(clientOpts, kgo.DialTLSConfig(tlsCfg))
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
