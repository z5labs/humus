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

// OrderMessage represents an order to be published.
type OrderMessage struct {
	OrderID   string  `json:"order_id"`
	Amount    float64 `json:"amount"`
	ProductID string  `json:"product_id"`
	Quantity  int     `json:"quantity"`
}

// Config holds the publisher configuration.
type Config struct {
	Kafka struct {
		Brokers []string `yaml:"brokers"`
		Topic   string   `yaml:"topic"`
	} `yaml:"kafka"`

	Publisher struct {
		Count    int `yaml:"count"`    // Number of messages to publish
		Interval int `yaml:"interval"` // Interval between messages in milliseconds
	} `yaml:"publisher"`
}

var productIDs = []string{
	"PROD-001", "PROD-002", "PROD-003", "PROD-004", "PROD-005",
	"PROD-006", "PROD-007", "PROD-008", "PROD-009", "PROD-010",
}

func generateRandomOrder() *OrderMessage {
	return &OrderMessage{
		OrderID:   fmt.Sprintf("ORDER-%d", time.Now().UnixNano()),
		Amount:    float64(rand.Intn(99000)+1000) / 100.0, // $10.00 to $1000.00
		ProductID: productIDs[rand.Intn(len(productIDs))],
		Quantity:  rand.Intn(10) + 1, // 1 to 10 items
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
	topic := flag.String("topic", "orders", "Kafka topic to publish to")
	duration := flag.Duration("duration", 10*time.Second, "number of messages to publish (overrides config)")
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
			log.Println("Shutdown signal received, stopping publisher", published)
			return
		default:
		}

		// Generate random order
		order := generateRandomOrder()

		// Encode to JSON
		data, err := json.Marshal(order)
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
