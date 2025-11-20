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

	// Configure TLS if certificates are provided
	var tlsOpts []kafka.Option
	if cfg.Kafka.TLS.CertFile != "" || cfg.Kafka.TLS.CAFile != "" {
		tlsConfig, err := buildTLSConfig(cfg.Kafka.TLS)
		if err != nil {
			return nil, fmt.Errorf("failed to build TLS config: %w", err)
		}
		tlsOpts = append(tlsOpts, kafka.WithTLS(tlsConfig))
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

// buildTLSConfig constructs a *tls.Config from the application configuration.
func buildTLSConfig(cfg struct {
	CertFile   string `config:"cert_file"`
	KeyFile    string `config:"key_file"`
	CAFile     string `config:"ca_file"`
	ServerName string `config:"server_name"`
	MinVersion uint16 `config:"min_version"`
}) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		ServerName: cfg.ServerName,
	}

	// Set TLS version
	if cfg.MinVersion > 0 {
		tlsConfig.MinVersion = cfg.MinVersion
	} else {
		// Default to TLS 1.2 for security
		tlsConfig.MinVersion = tls.VersionTLS12
	}

	// Load client certificate and key if provided
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate and key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	// Load CA certificate if provided
	if cfg.CAFile != "" {
		caCert, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.RootCAs = caCertPool
	}

	return tlsConfig, nil
}
