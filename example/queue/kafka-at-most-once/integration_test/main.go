// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

const (
	kafkaBroker       = "localhost:9092"
	kafkaTopic        = "metrics"
	kafkaGroupID      = "integration-test-group"
	messageCount      = 10
	processingTimeout = 5 * time.Second
)

// MetricMessage represents the message structure processed by the application
type MetricMessage struct {
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Timestamp int64             `json:"timestamp"`
	Tags      map[string]string `json:"tags"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Step 1: Start Kafka using podman-compose
	fmt.Println("Starting Kafka container...")
	err := startKafka(ctx)
	if err != nil {
		log.Fatalf("Failed to start Kafka: %v", err)
	}

	// Register cleanup to stop Kafka
	defer func() {
		fmt.Println("Stopping Kafka container...")
		if err := stopKafka(); err != nil {
			log.Printf("Warning: Failed to stop Kafka: %v", err)
		}
	}()

	// Wait for Kafka to be ready
	fmt.Println("Waiting for Kafka to be ready...")
	err = waitForKafkaReady(ctx, kafkaBroker, 15*time.Second)
	if err != nil {
		log.Fatalf("Kafka failed to become ready: %v", err)
	}

	// Create Kafka admin client
	kafkaClient, err := kgo.NewClient(
		kgo.SeedBrokers(kafkaBroker),
	)
	if err != nil {
		log.Fatalf("Failed to create Kafka client: %v", err)
	}
	defer kafkaClient.Close()

	adminClient := kadm.NewClient(kafkaClient)

	// Step 2: Create topic
	fmt.Printf("Creating topic '%s' with 3 partitions...\n", kafkaTopic)
	err = createTopic(ctx, adminClient, kafkaTopic, 3, 1)
	if err != nil {
		log.Fatalf("Failed to create topic: %v", err)
	}

	// Step 3: Publish messages to topic
	fmt.Printf("Publishing %d test messages...\n", messageCount)
	messages, err := publishTestMessages(ctx, kafkaBroker, kafkaTopic, messageCount)
	if err != nil {
		log.Fatalf("Failed to publish test messages: %v", err)
	}
	if len(messages) != messageCount {
		log.Fatalf("Expected %d messages to be published, got %d", messageCount, len(messages))
	}

	// Step 4: Validate messages are in topic (sanity check)
	fmt.Println("Validating messages in topic...")
	err = validateMessagesInTopic(ctx, adminClient, kafkaTopic, int64(messageCount))
	if err != nil {
		log.Fatalf("Failed to validate messages in topic: %v", err)
	}

	// Step 5: Start the application using go run
	fmt.Println("Starting kafka-at-most-once application...")
	appCmd, err := startApplication(ctx)
	if err != nil {
		log.Fatalf("Failed to start application: %v", err)
	}

	// Ensure application is cleaned up
	defer func() {
		if appCmd != nil && appCmd.Process != nil {
			fmt.Println("Stopping application...")
			_ = appCmd.Process.Signal(syscall.SIGTERM)

			// Wait for graceful shutdown with timeout
			done := make(chan error, 1)
			go func() {
				done <- appCmd.Wait()
			}()

			select {
			case <-time.After(5 * time.Second):
				fmt.Println("Application did not stop gracefully, killing...")
				_ = appCmd.Process.Kill()
			case err := <-done:
				if err != nil {
					fmt.Printf("Application exited with error: %v\n", err)
				} else {
					fmt.Println("Application stopped gracefully")
				}
			}
		}
	}()

	// Step 6: Validate that all messages have been committed
	// In at-most-once semantics, messages are committed BEFORE processing
	fmt.Println("Waiting for all messages to be committed (at-most-once semantics)...")
	processCtx, processCancel := context.WithTimeout(ctx, processingTimeout)
	defer processCancel()

	err = waitForCommittedOffsets(processCtx, adminClient, kafkaTopic, kafkaGroupID, int64(messageCount))
	if err != nil {
		log.Fatalf("Failed to commit all messages within timeout: %v", err)
	}

	fmt.Println("Integration test completed successfully!")
	fmt.Println("Note: At-most-once semantics means messages were committed before processing.")
	fmt.Println("If any processing errors occurred, those messages would be lost (which is expected).")
}

// startKafka starts Kafka using podman-compose
func startKafka(ctx context.Context) error {
	composeFile, err := getComposeFilePath()
	if err != nil {
		return fmt.Errorf("failed to get compose file path: %w", err)
	}

	cmd := exec.CommandContext(ctx, "podman-compose", "-f", composeFile, "up", "-d")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// stopKafka stops and removes Kafka containers and volumes
func stopKafka() error {
	composeFile, err := getComposeFilePath()
	if err != nil {
		return fmt.Errorf("failed to get compose file path: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "podman-compose", "-f", composeFile, "down", "-v")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// getComposeFilePath returns the absolute path to podman-compose.yaml
func getComposeFilePath() (string, error) {
	// integration_test/../podman-compose.yaml
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get working directory: %w", err)
	}

	composeFile := filepath.Join(wd, "..", "podman-compose.yaml")
	absPath, err := filepath.Abs(composeFile)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path to compose file: %w", err)
	}

	return absPath, nil
}

// waitForKafkaReady polls Kafka until it's responsive or timeout occurs
func waitForKafkaReady(ctx context.Context, broker string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for Kafka to be ready: %w", ctx.Err())
		case <-ticker.C:
			client, err := kgo.NewClient(
				kgo.SeedBrokers(broker),
				kgo.RequestTimeoutOverhead(2*time.Second),
			)
			if err != nil {
				continue
			}

			// Try to fetch metadata
			err = client.Ping(ctx)
			client.Close()

			if err == nil {
				return nil
			}
		}
	}
}

// createTopic creates a Kafka topic with specified partitions and replication factor
func createTopic(ctx context.Context, admin *kadm.Client, topic string, partitions int32, replicationFactor int16) error {
	resp, err := admin.CreateTopics(ctx, partitions, replicationFactor, nil, topic)
	if err != nil {
		return fmt.Errorf("failed to create topic: %w", err)
	}

	for _, topicResp := range resp {
		if topicResp.Err != nil {
			return fmt.Errorf("failed to create topic %s: %w", topicResp.Topic, topicResp.Err)
		}
	}

	return nil
}

// publishTestMessages publishes test MetricMessage records to Kafka
func publishTestMessages(ctx context.Context, broker, topic string, count int) ([]MetricMessage, error) {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(broker),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create producer client: %w", err)
	}
	defer client.Close()

	messages := make([]MetricMessage, 0, count)
	records := make([]*kgo.Record, 0, count)

	metricNames := []string{"cpu_usage", "memory_usage", "disk_io"}

	for i := 0; i < count; i++ {
		msg := MetricMessage{
			Name:      metricNames[i%len(metricNames)],
			Value:     float64((i + 1) * 10),
			Timestamp: time.Now().UnixMilli(),
			Tags: map[string]string{
				"host":   fmt.Sprintf("host-%d", i%3),
				"region": "us-east-1",
			},
		}
		messages = append(messages, msg)

		msgBytes, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal message: %w", err)
		}

		record := &kgo.Record{
			Topic: topic,
			Value: msgBytes,
			Key:   []byte(fmt.Sprintf("metric-%d", i)),
		}
		records = append(records, record)
	}

	// Produce all messages synchronously
	results := client.ProduceSync(ctx, records...)
	if err := results.FirstErr(); err != nil {
		return nil, fmt.Errorf("failed to produce messages: %w", err)
	}

	return messages, nil
}

// validateMessagesInTopic verifies that the expected number of messages exist in the topic
func validateMessagesInTopic(ctx context.Context, admin *kadm.Client, topic string, expectedCount int64) error {
	// Fetch high watermarks for all partitions
	offsets, err := admin.ListEndOffsets(ctx, topic)
	if err != nil {
		return fmt.Errorf("failed to list end offsets: %w", err)
	}

	var totalMessages int64
	offsets.Each(func(o kadm.ListedOffset) {
		totalMessages += o.Offset
	})

	if totalMessages != expectedCount {
		return fmt.Errorf("expected %d messages in topic, found %d", expectedCount, totalMessages)
	}

	return nil
}

// startApplication starts the kafka-at-most-once application using go run
func startApplication(ctx context.Context) (*exec.Cmd, error) {
	// Get the parent directory (example/queue/kafka-at-most-once)
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}

	appDir := filepath.Join(wd, "..")
	absAppDir, err := filepath.Abs(appDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute app directory: %w", err)
	}

	// Create command: go run .
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = absAppDir

	// Set environment variables
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("KAFKA_BROKERS=%s", kafkaBroker),
		fmt.Sprintf("KAFKA_TOPIC=%s", kafkaTopic),
		fmt.Sprintf("KAFKA_GROUP_ID=%s", kafkaGroupID),
	)

	// Capture output for debugging
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start the application
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start application: %w", err)
	}

	return cmd, nil
}

// waitForCommittedOffsets polls consumer group offsets until expected count is reached.
// In at-most-once semantics, offsets are committed BEFORE processing, so this verifies
// that messages were received and acknowledged (even if processing might have failed).
func waitForCommittedOffsets(ctx context.Context, admin *kadm.Client, topic, groupID string, expectedCount int64) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Try one last time to get diagnostic info
			offsets, _ := admin.FetchOffsets(ctx, groupID)
			endOffsets, _ := admin.ListEndOffsets(ctx, topic)

			var currentCount int64
			offsets.Each(func(o kadm.OffsetResponse) {
				if o.Topic == topic {
					currentCount += o.At
				}
			})

			fmt.Printf("[DEBUG] Timeout diagnostic:\n")
			fmt.Printf("[DEBUG] Committed offsets: %d (expected %d)\n", currentCount, expectedCount)
			endOffsets.Each(func(o kadm.ListedOffset) {
				fmt.Printf("[DEBUG] End offset for partition %d: %d\n", o.Partition, o.Offset)
			})

			return fmt.Errorf("timeout waiting for committed offsets: expected %d, got %d: %w",
				expectedCount, currentCount, ctx.Err())

		case <-ticker.C:
			// Fetch committed offsets for the consumer group
			offsets, err := admin.FetchOffsets(ctx, groupID)
			if err != nil {
				// Consumer group might not exist yet, continue polling
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				fmt.Printf("[DEBUG] Error fetching offsets: %v, continuing...\n", err)
				continue
			}

			// Fetch end offsets for the topic
			endOffsets, err := admin.ListEndOffsets(ctx, topic)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				fmt.Printf("[DEBUG] Error listing end offsets: %v, continuing...\n", err)
				continue
			}

			// Build a map of partition -> end offset for comparison
			endOffsetMap := make(map[int32]int64)
			endOffsets.Each(func(o kadm.ListedOffset) {
				endOffsetMap[o.Partition] = o.Offset
			})

			// Check committed offsets against end offsets for each partition
			var committedCount int64
			allPartitionsProcessed := true
			partitionCount := 0

			offsets.Each(func(o kadm.OffsetResponse) {
				if o.Topic != topic {
					return
				}

				partitionCount++
				endOffset, hasEnd := endOffsetMap[o.Partition]

				fmt.Printf("[DEBUG] Partition %d: committed=%d, end=%d, matched=%t\n",
					o.Partition, o.At, endOffset, o.At == endOffset)

				committedCount += o.At

				// Check if this partition has caught up to its end offset
				if hasEnd && o.At < endOffset {
					allPartitionsProcessed = false
				}
			})

			fmt.Printf("[DEBUG] Total partitions: %d, Total committed: %d (expected %d), All caught up: %t\n",
				partitionCount, committedCount, expectedCount, allPartitionsProcessed)

			// Success if we've committed the expected count and all partitions are caught up
			if committedCount >= expectedCount && allPartitionsProcessed {
				return nil
			}
		}
	}
}
