// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// BuildApp creates the Kafka queue application builder with mTLS.
func BuildApp(ctx context.Context) app.Builder[queue.Runtime] {
	// Create business logic processor with idempotent handling
	handler := NewOrderProcessor()

	// Wrap with decoding middleware
	processor := &DecodingProcessor{
		decoder: decodeOrder,
		handler: handler,
	}

	// Configure TLS using config readers
	tlsConfig := config.ReaderFunc[*tls.Config](func(ctx context.Context) (config.Value[*tls.Config], error) {
		// Read certificate paths from environment
		certFile := config.MustOr(ctx, "", config.Env("KAFKA_TLS_CERT_FILE"))
		keyFile := config.MustOr(ctx, "", config.Env("KAFKA_TLS_KEY_FILE"))
		caFile := config.MustOr(ctx, "", config.Env("KAFKA_TLS_CA_FILE"))
		serverName := config.MustOr(ctx, "", config.Env("KAFKA_TLS_SERVER_NAME"))

		// If no TLS config provided, return nil
		if certFile == "" && caFile == "" {
			return config.Value[*tls.Config]{}, nil
		}

		tlsCfg := &tls.Config{
			ServerName: serverName,
			MinVersion: tls.VersionTLS12,
		}

		// Load client certificate and key if provided
		if certFile != "" && keyFile != "" {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return config.Value[*tls.Config]{}, fmt.Errorf("failed to load client certificate and key: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}

		// Load CA certificate if provided
		if caFile != "" {
			caCert, err := os.ReadFile(caFile)
			if err != nil {
				return config.Value[*tls.Config]{}, fmt.Errorf("failed to read CA certificate: %w", err)
			}

			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caCert) {
				return config.Value[*tls.Config]{}, fmt.Errorf("failed to parse CA certificate")
			}
			tlsCfg.RootCAs = caCertPool
		}

		return config.ValueOf(tlsCfg), nil
	})

	// Configure Kafka infrastructure with mTLS
	cfg := kafka.Config{
		Brokers: config.Or(
			kafka.BrokersFromEnv(),
			config.ReaderOf([]string{"localhost:9092"}),
		),
		GroupID: config.Or(
			kafka.GroupIDFromEnv(),
			config.ReaderOf("order-processor-mtls"),
		),
		TLSConfig: tlsConfig,
	}

	// Configure topic and processor with at-least-once delivery
	topic := config.MustOr(ctx, "orders", config.Env("KAFKA_TOPIC"))
	topics := []kafka.TopicProcessor{
		{
			Topic:        topic,
			Processor:    processor,
			DeliveryMode: kafka.AtLeastOnce,
		},
	}

	// Build Kafka queue runtime
	kafkaBuilder := kafka.Build(cfg, topics)

	// Wrap with queue.Runtime
	return app.Bind(kafkaBuilder, func(qr queue.QueueRuntime) app.Builder[queue.Runtime] {
		return queue.Build(qr)
	})
}
