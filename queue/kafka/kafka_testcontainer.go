//go:build testcontainers

// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"crypto/tls"
	"fmt"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/humus/config"
)

// setupKafkaContainer starts a Kafka container and returns the broker address and cleanup function.
func setupKafkaContainer(t *testing.T) (brokers []string, cleanup func()) {
	t.Helper()

	ctx := context.Background()

	// Configure Kafka container with KRaft mode settings
	// Using host network mode for simplicity with advertised listeners
	req := testcontainers.ContainerRequest{
		Image: "docker.io/apache/kafka-native:latest",
		HostConfigModifier: func(hc *container.HostConfig) {
			// Use host network mode to avoid port mapping issues
			// This makes the container accessible on localhost at the actual Kafka port
			hc.NetworkMode = "host"
		},
		User: "root", // Run as root to avoid permission issues with /var/lib/kafka/data
		Env: map[string]string{
			// KRaft mode settings
			"KAFKA_NODE_ID":                   "1",
			"KAFKA_PROCESS_ROLES":             "broker,controller",
			"KAFKA_CONTROLLER_QUORUM_VOTERS":  "1@localhost:9093",
			"KAFKA_CONTROLLER_LISTENER_NAMES": "CONTROLLER",

			// Listener configuration
			"KAFKA_LISTENERS":                      "PLAINTEXT://0.0.0.0:9092,CONTROLLER://0.0.0.0:9093",
			"KAFKA_ADVERTISED_LISTENERS":           "PLAINTEXT://localhost:9092",
			"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP": "PLAINTEXT:PLAINTEXT,CONTROLLER:PLAINTEXT",
			"KAFKA_INTER_BROKER_LISTENER_NAME":     "PLAINTEXT",

			// Log settings
			"KAFKA_LOG_DIRS": "/var/lib/kafka/data",

			// Kafka cluster ID
			"KAFKA_CLUSTER_ID": "WmV3pZkQR0O6n5j3x8j6bg==",

			// Cluster settings
			"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR":         "1",
			"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR": "1",
			"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR":            "1",
			"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS":         "0",
			"KAFKA_AUTO_CREATE_TOPICS_ENABLE":                "false",
		},
		WaitingFor: wait.ForLog("Kafka Server started").WithStartupTimeout(60 * time.Second),
	}

	// Start Kafka container
	kafkaContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "failed to start Kafka container")

	// With host networking, Kafka is accessible on localhost:9092
	brokerAddr := "localhost:9092"

	// Give Kafka a moment to fully start up
	time.Sleep(2 * time.Second)

	cleanup = func() {
		ctx := context.Background()
		if err := kafkaContainer.Terminate(ctx); err != nil {
			t.Logf("failed to terminate Kafka container: %v", err)
		}
	}

	return []string{brokerAddr}, cleanup
}

// createTopic creates a Kafka topic with the specified number of partitions.
func createTopic(t *testing.T, brokers []string, topic string, partitions int32) {
	t.Helper()

	ctx := context.Background()

	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
	)
	require.NoError(t, err, "failed to create Kafka client")
	defer client.Close()

	admin := kadm.NewClient(client)

	resp, err := admin.CreateTopics(ctx, partitions, 1, nil, topic)
	require.NoError(t, err, "failed to create topic")

	for _, topicResp := range resp {
		require.NoError(t, topicResp.Err, "failed to create topic %s", topic)
	}

	// Wait for topic to be ready
	time.Sleep(1 * time.Second)
}

// produceTestMessages produces messages to a Kafka topic.
func produceTestMessages(t *testing.T, brokers []string, topic string, messages []Message) {
	t.Helper()

	ctx := context.Background()

	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
	)
	require.NoError(t, err, "failed to create Kafka client")
	defer client.Close()

	for i, msg := range messages {
		record := &kgo.Record{
			Topic: topic,
			Key:   msg.Key,
			Value: msg.Value,
		}

		// Convert headers
		if len(msg.Headers) > 0 {
			record.Headers = make([]kgo.RecordHeader, len(msg.Headers))
			for j, h := range msg.Headers {
				record.Headers[j] = kgo.RecordHeader{
					Key:   h.Key,
					Value: h.Value,
				}
			}
		}

		result := client.ProduceSync(ctx, record)
		require.NoError(t, result.FirstErr(), "failed to produce message %d", i)
	}

	// Flush to ensure all messages are sent
	require.NoError(t, client.Flush(ctx), "failed to flush messages")
}

// newTestRuntime creates a new Runtime instance for testing.
func newTestRuntime(t *testing.T, brokers []string, groupID string, topics []TopicProcessor) (Runtime, error) {
	t.Helper()

	cfg := Config{
		Brokers: config.ReaderFunc[[]string](func(ctx context.Context) (config.Value[[]string], error) {
			return config.ValueOf(brokers), nil
		}),
		GroupID: config.ReaderFunc[string](func(ctx context.Context) (config.Value[string], error) {
			return config.ValueOf(groupID), nil
		}),
		// Provide empty readers for optional fields so config.MustOr can apply defaults
		SessionTimeout: config.ReaderFunc[time.Duration](func(ctx context.Context) (config.Value[time.Duration], error) {
			return config.Value[time.Duration]{}, nil
		}),
		RebalanceTimeout: config.ReaderFunc[time.Duration](func(ctx context.Context) (config.Value[time.Duration], error) {
			return config.Value[time.Duration]{}, nil
		}),
		FetchMaxBytes: config.ReaderFunc[int32](func(ctx context.Context) (config.Value[int32], error) {
			return config.Value[int32]{}, nil
		}),
		MaxConcurrentFetches: config.ReaderFunc[int](func(ctx context.Context) (config.Value[int], error) {
			return config.Value[int]{}, nil
		}),
		TLSConfig: config.ReaderFunc[*tls.Config](func(ctx context.Context) (config.Value[*tls.Config], error) {
			return config.Value[*tls.Config]{}, nil
		}),
	}

	builder := Build(cfg, topics)
	runtime, err := builder.Build(context.Background())
	if err != nil {
		return Runtime{}, err
	}

	return runtime.(Runtime), nil
}

// testMessage creates a test Message with the given value.
func testMessage(value string) Message {
	return Message{
		Key:   []byte(fmt.Sprintf("key-%s", value)),
		Value: []byte(value),
	}
}

// testMessageWithHeaders creates a test Message with headers.
func testMessageWithHeaders(value string, headers []Header) Message {
	return Message{
		Key:     []byte(fmt.Sprintf("key-%s", value)),
		Value:   []byte(value),
		Headers: headers,
	}
}
