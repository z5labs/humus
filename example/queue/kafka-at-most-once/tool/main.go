// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// MetricMessage represents a metric to be published.
type MetricMessage struct {
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Timestamp int64             `json:"timestamp"`
	Tags      map[string]string `json:"tags"`
}

var metricNames = []string{
	"cpu_usage",
	"memory_usage",
	"disk_io_read",
	"disk_io_write",
	"network_in",
	"network_out",
	"request_count",
	"error_count",
	"latency_p50",
	"latency_p99",
}

var hosts = []string{
	"host-01", "host-02", "host-03", "host-04", "host-05",
}

var regions = []string{
	"us-east-1", "us-west-2", "eu-west-1",
}

func generateRandomMetric() *MetricMessage {
	return &MetricMessage{
		Name:      metricNames[rand.Intn(len(metricNames))],
		Value:     float64(rand.Intn(10000)) / 100.0, // 0.00 to 100.00
		Timestamp: time.Now().UnixMilli(),
		Tags: map[string]string{
			"host":   hosts[rand.Intn(len(hosts))],
			"region": regions[rand.Intn(len(regions))],
		},
	}
}

func createTopic(ctx context.Context, client *kgo.Client, topic string) error {
	adminClient := kadm.NewClient(client)
	// Don't close adminClient - it wraps the main client which we still need

	// Create topic with default settings: 1 partition, replication factor 1
	resp, err := adminClient.CreateTopics(ctx, 1, 1, nil, topic)
	if err != nil {
		return fmt.Errorf("failed to create topic: %w", err)
	}

	// Check if creation was successful
	for _, ctr := range resp {
		if ctr.Err != nil {
			// Topic already exists is not an error for our use case
			if strings.Contains(ctr.Err.Error(), "TOPIC_ALREADY_EXISTS") {
				log.Printf("Topic '%s' already exists", topic)
				return nil
			}
			return fmt.Errorf("failed to create topic '%s': %w", topic, ctr.Err)
		}
		log.Printf("Topic '%s' created successfully", topic)
	}

	return nil
}

func main() {
	brokers := flag.String("brokers", "localhost:9092", "comma-separated list of Kafka brokers")
	topic := flag.String("topic", "metrics", "Kafka topic to publish to")
	duration := flag.Duration("duration", 10*time.Second, "duration to publish messages")
	flag.Parse()

	// Create Kafka client
	client, err := kgo.NewClient(
		kgo.SeedBrokers(strings.Split(*brokers, ",")...),
	)
	if err != nil {
		log.Fatalf("failed to create Kafka client: %v", err)
	}
	defer client.Close()

	dCtx, dCancel := context.WithTimeout(context.Background(), *duration)
	defer dCancel()

	ctx, cancel := signal.NotifyContext(dCtx, os.Kill, os.Interrupt)
	defer cancel()

	// Create topic if it doesn't exist
	if err := createTopic(ctx, client, *topic); err != nil {
		log.Fatalf("failed to ensure topic exists: %v", err)
	}

	// Publish messages
	published := 0
	for {
		select {
		case <-ctx.Done():
			log.Printf("Shutdown signal received, stopping publisher. Published %d messages", published)
			return
		default:
		}

		// Generate random metric
		metric := generateRandomMetric()

		// Encode to JSON
		data, err := json.Marshal(metric)
		if err != nil {
			log.Printf("failed to encode message: %v", err)
			continue
		}

		// Create Kafka record
		record := &kgo.Record{
			Topic: *topic,
			Value: data,
		}

		// Publish
		result := client.ProduceSync(ctx, record)
		err = result.FirstErr()
		if errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		if err != nil {
			log.Printf("failed to publish message: %v", err)
			continue
		}

		published++
	}
}
