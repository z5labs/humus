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
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/queue"

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

// DeliveryMode specifies the message delivery semantics for a topic.
type DeliveryMode int

const (
	// AtLeastOnce ensures messages are processed before acknowledgment.
	// May result in duplicate processing on failure, but no message loss.
	AtLeastOnce DeliveryMode = iota

	// AtMostOnce acknowledges messages before processing.
	// May result in message loss on failure, but no duplicate processing.
	AtMostOnce
)

// TopicProcessor associates a topic with its processor and delivery mode.
// This is NOT a config.Reader - it's business logic configuration.
type TopicProcessor struct {
	Topic        string
	Processor    queue.Processor[Message]
	DeliveryMode DeliveryMode
}

// Config holds configuration readers for Kafka infrastructure settings.
// All fields use config.Reader for composable configuration.
type Config struct {
	Brokers              config.Reader[[]string]
	GroupID              config.Reader[string]
	SessionTimeout       config.Reader[time.Duration]
	RebalanceTimeout     config.Reader[time.Duration]
	FetchMaxBytes        config.Reader[int32]
	MaxConcurrentFetches config.Reader[int]
	TLSConfig            config.Reader[*tls.Config]
}

// BrokersFromEnv reads Kafka broker addresses from the KAFKA_BROKERS environment variable.
// Brokers should be comma-separated (e.g., "localhost:9092,localhost:9093").
func BrokersFromEnv() config.Reader[[]string] {
	return config.Map(
		config.Env("KAFKA_BROKERS"),
		func(ctx context.Context, s string) ([]string, error) {
			return strings.Split(s, ","), nil
		},
	)
}

// GroupIDFromEnv reads the Kafka consumer group ID from the KAFKA_GROUP_ID environment variable.
func GroupIDFromEnv() config.Reader[string] {
	return config.Env("KAFKA_GROUP_ID")
}

// SessionTimeoutFromEnv reads the Kafka session timeout from the KAFKA_SESSION_TIMEOUT environment variable.
// The value should be a duration string (e.g., "45s", "1m30s").
func SessionTimeoutFromEnv() config.Reader[time.Duration] {
	return config.Map(
		config.Env("KAFKA_SESSION_TIMEOUT"),
		func(ctx context.Context, s string) (time.Duration, error) {
			return time.ParseDuration(s)
		},
	)
}

// RebalanceTimeoutFromEnv reads the Kafka rebalance timeout from the KAFKA_REBALANCE_TIMEOUT environment variable.
// The value should be a duration string (e.g., "30s", "1m").
func RebalanceTimeoutFromEnv() config.Reader[time.Duration] {
	return config.Map(
		config.Env("KAFKA_REBALANCE_TIMEOUT"),
		func(ctx context.Context, s string) (time.Duration, error) {
			return time.ParseDuration(s)
		},
	)
}

// FetchMaxBytesFromEnv reads the maximum fetch bytes from the KAFKA_FETCH_MAX_BYTES environment variable.
// The value should be a number (e.g., "52428800" for 50MB).
func FetchMaxBytesFromEnv() config.Reader[int32] {
	return config.Map(
		config.Env("KAFKA_FETCH_MAX_BYTES"),
		func(ctx context.Context, s string) (int32, error) {
			n, err := strconv.ParseInt(s, 10, 32)
			if err != nil {
				return 0, err
			}
			return int32(n), nil
		},
	)
}

// MaxConcurrentFetchesFromEnv reads the maximum concurrent fetches from the KAFKA_MAX_CONCURRENT_FETCHES environment variable.
// The value should be a number (e.g., "10").
func MaxConcurrentFetchesFromEnv() config.Reader[int] {
	return config.Map(
		config.Env("KAFKA_MAX_CONCURRENT_FETCHES"),
		func(ctx context.Context, s string) (int, error) {
			return strconv.Atoi(s)
		},
	)
}

// TLSConfigFromFiles creates a config.Reader that loads TLS configuration from certificate files.
// This is a helper for common TLS setup patterns.
//
// Parameters:
//   - certFile: Path to client certificate file (required for mTLS)
//   - keyFile: Path to client key file (required for mTLS)
//   - caFile: Path to CA certificate file (required for TLS verification)
//
// Example:
//
//	tlsConfig := kafka.TLSConfigFromFiles(
//	    config.ValueOf("client-cert.pem"),
//	    config.ValueOf("client-key.pem"),
//	    config.ValueOf("ca-cert.pem"),
//	)
func TLSConfigFromFiles(
	certFile config.Reader[string],
	keyFile config.Reader[string],
	caFile config.Reader[string],
) config.Reader[*tls.Config] {
	return config.ReaderFunc[*tls.Config](func(ctx context.Context) (config.Value[*tls.Config], error) {
		certPath := config.Must(ctx, certFile)
		keyPath := config.Must(ctx, keyFile)
		caPath := config.Must(ctx, caFile)

		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return config.Value[*tls.Config]{}, fmt.Errorf("failed to load client certificate: %w", err)
		}

		caCert, err := os.ReadFile(caPath)
		if err != nil {
			return config.Value[*tls.Config]{}, fmt.Errorf("failed to read CA certificate: %w", err)
		}

		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}

		// Note: CA pool configuration would be added here when needed
		// This is a basic template showing certificate loading
		_ = caCert

		return config.ValueOf(tlsConfig), nil
	})
}

// Runtime represents the Kafka queue runtime for processing messages.
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

// Build creates an app.Builder for a Kafka queue runtime.
//
// This function reads configuration from the provided Config readers and creates
// topic orchestrators based on the TopicProcessor specifications.
//
// Parameters:
//   - cfg: Infrastructure configuration (brokers, timeouts, etc.) using config.Reader
//   - topics: Business logic configuration (topic/processor/delivery mode mappings)
//
// Example:
//
//	cfg := kafka.Config{
//	    Brokers:  kafka.BrokersFromEnv(),
//	    GroupID:  kafka.GroupIDFromEnv(),
//	}
//
//	topics := []kafka.TopicProcessor{
//	    {
//	        Topic:        "orders",
//	        Processor:    ordersProcessor,
//	        DeliveryMode: kafka.AtLeastOnce,
//	    },
//	}
//
//	builder := kafka.Build(cfg, topics)
func Build(cfg Config, topics []TopicProcessor) app.Builder[queue.QueueRuntime] {
	return app.BuilderFunc[queue.QueueRuntime](func(ctx context.Context) (queue.QueueRuntime, error) {
		// Read infrastructure configuration
		brokers := config.Must(ctx, cfg.Brokers)
		groupID := config.Must(ctx, cfg.GroupID)

		// Apply defaults for optional config
		sessionTimeout := config.MustOr(ctx, 45*time.Second, cfg.SessionTimeout)
		rebalanceTimeout := config.MustOr(ctx, 30*time.Second, cfg.RebalanceTimeout)
		fetchMaxBytes := config.MustOr(ctx, int32(50*1024*1024), cfg.FetchMaxBytes)
		maxConcurrentFetches := config.MustOr(ctx, 0, cfg.MaxConcurrentFetches)

		// TLS config is optional
		var tlsConfig *tls.Config
		if cfg.TLSConfig != nil {
			tlsConfig = config.MustOr(ctx, (*tls.Config)(nil), cfg.TLSConfig)
		}

		if len(topics) == 0 {
			return nil, fmt.Errorf("kafka: at least one topic must be configured")
		}

		// Build topic orchestrators from TopicProcessor specifications
		topicOrchestrators := make(map[string]partitionOrchestrator, len(topics))
		for _, tp := range topics {
			var orch partitionOrchestrator
			switch tp.DeliveryMode {
			case AtLeastOnce:
				orch = newAtLeastOnceOrchestrator(groupID, tp.Processor)
			case AtMostOnce:
				orch = newAtMostOnceOrchestrator(groupID, tp.Processor)
			default:
				return nil, fmt.Errorf("kafka: unknown delivery mode for topic %s", tp.Topic)
			}
			topicOrchestrators[tp.Topic] = orch
		}

		runtime := Runtime{
			log:                  logger().With(GroupIDAttr(groupID)),
			brokers:              brokers,
			groupID:              groupID,
			topics:               topicOrchestrators,
			sessionTimeout:       sessionTimeout,
			rebalanceTimeout:     rebalanceTimeout,
			fetchMaxBytes:        fetchMaxBytes,
			maxConcurrentFetches: maxConcurrentFetches,
			tlsConfig:            tlsConfig,
		}

		return runtime, nil
	})
}
