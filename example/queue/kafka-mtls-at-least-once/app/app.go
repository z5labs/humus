// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/tls"

	"github.com/z5labs/humus/queue"
	"github.com/z5labs/humus/queue/kafka"
)

// Config holds the application configuration.
type Config struct {
	queue.Config `config:",squash"`

	Kafka struct {
		Brokers []string `config:"brokers"`
		GroupID string   `config:"group_id"`
		Topic   string   `config:"topic"`
		TLS     struct {
			CertFile   string `config:"cert_file"`
			KeyFile    string `config:"key_file"`
			CAFile     string `config:"ca_file"`
			ServerName string `config:"server_name"`
			MinVersion uint16 `config:"min_version"`
		} `config:"tls"`
	} `config:"kafka"`
}

// Init initializes the application with mTLS-enabled Kafka runtime.
func Init(ctx context.Context, cfg Config) (*queue.App, error) {
	// Create business logic processor with idempotent handling
	handler := NewOrderProcessor()

	// Wrap with decoding middleware
	processor := &DecodingProcessor{
		decoder: decodeOrder,
		handler: handler,
	}

	// Configure TLS
	var tlsOpts []kafka.Option
	if cfg.Kafka.TLS.CertFile != "" || cfg.Kafka.TLS.CAFile != "" {
		tlsCfg := kafka.TLSConfig{
			CertFile:   cfg.Kafka.TLS.CertFile,
			KeyFile:    cfg.Kafka.TLS.KeyFile,
			CAFile:     cfg.Kafka.TLS.CAFile,
			ServerName: cfg.Kafka.TLS.ServerName,
		}
		
		// Set TLS version if specified (0 means use default)
		if cfg.Kafka.TLS.MinVersion > 0 {
			tlsCfg.MinVersion = cfg.Kafka.TLS.MinVersion
		} else {
			// Default to TLS 1.2 for security
			tlsCfg.MinVersion = tls.VersionTLS12
		}
		
		tlsOpts = append(tlsOpts, kafka.WithTLS(tlsCfg))
	}

	// Create Kafka runtime with at-least-once semantics and mTLS
	opts := append(tlsOpts, kafka.AtLeastOnce(cfg.Kafka.Topic, processor))
	runtime := kafka.NewRuntime(
		cfg.Kafka.Brokers,
		cfg.Kafka.GroupID,
		opts...,
	)

	return queue.NewApp(runtime), nil
}
