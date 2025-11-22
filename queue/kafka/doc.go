// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package kafka provides Kafka runtime implementations for the Humus queue framework.
//
// This package integrates Apache Kafka with Humus's queue processing abstractions,
// offering at-most-once and at-least-once delivery semantics with concurrent
// per-partition processing using the franz-go client library.
//
// # Architecture
//
// The package provides two runtime implementations:
//   - [AtMostOnceRuntime] - Acknowledges messages before processing (fast, may lose messages)
//   - [AtLeastOnceRuntime] - Acknowledges messages after processing (reliable, may duplicate)
//
// Both runtimes use a goroutine-per-partition pattern for concurrent processing,
// leveraging franz-go's partition assignment callbacks. Each partition runs an
// independent processing loop using the core queue package's [queue.ProcessAtMostOnce]
// or [queue.ProcessAtLeastOnce] processors.
//
// # At-Most-Once Processing
//
// At-most-once processing provides fast throughput by acknowledging (committing offsets)
// immediately after consuming messages, before processing. If processing fails, the
// message is already committed and will be lost.
//
// Use cases:
//   - Metrics collection and monitoring
//   - Log aggregation
//   - Cache updates
//   - Any scenario where occasional data loss is acceptable
//
// Example:
//
//	type MetricsMessage struct {
//	    Name  string
//	    Value float64
//	}
//
//	// Decode Kafka message bytes into your message type
//	func decodeMetrics(data []byte) (*MetricsMessage, error) {
//	    var msg MetricsMessage
//	    err := json.Unmarshal(data, &msg)
//	    return &msg, err
//	}
//
//	// Implement business logic
//	type MetricsProcessor struct{}
//
//	func (p *MetricsProcessor) Process(ctx context.Context, msg *MetricsMessage) error {
//	    // Send to monitoring system
//	    return nil
//	}
//
//	// Create runtime and run
//	func Init(ctx context.Context, cfg Config) (*queue.App, error) {
//	    processor := &MetricsProcessor{}
//	    runtime, err := kafka.NewAtMostOnceRuntime(
//	        cfg.Kafka.Brokers,
//	        cfg.Kafka.Topic,
//	        cfg.Kafka.GroupID,
//	        processor,
//	        decodeMetrics,
//	    )
//	    if err != nil {
//	        return nil, err
//	    }
//	    return queue.NewApp(runtime), nil
//	}
//
// # At-Least-Once Processing
//
// At-least-once processing provides reliable delivery by acknowledging (committing offsets)
// only after successful processing. If processing fails, the message is not committed and
// will be redelivered for retry. Your processor MUST be idempotent to handle duplicates.
//
// Use cases:
//   - Financial transactions
//   - Database updates
//   - Event sourcing
//   - Any scenario where data loss is unacceptable
//
// Example:
//
//	type OrderMessage struct {
//	    OrderID string
//	    Amount  float64
//	}
//
//	// Implement idempotent business logic
//	type OrderProcessor struct {
//	    db *sql.DB
//	}
//
//	func (p *OrderProcessor) Process(ctx context.Context, msg *OrderMessage) error {
//	    // Check if already processed (idempotency)
//	    var exists bool
//	    err := p.db.QueryRowContext(ctx,
//	        "SELECT EXISTS(SELECT 1 FROM orders WHERE order_id = $1)",
//	        msg.OrderID,
//	    ).Scan(&exists)
//	    if err != nil {
//	        return err
//	    }
//	    if exists {
//	        return nil // Already processed
//	    }
//
//	    // Process order
//	    _, err = p.db.ExecContext(ctx,
//	        "INSERT INTO orders (order_id, amount) VALUES ($1, $2)",
//	        msg.OrderID, msg.Amount,
//	    )
//	    return err
//	}
//
//	// Create runtime and run
//	func Init(ctx context.Context, cfg Config) (*queue.App, error) {
//	    processor := &OrderProcessor{db: cfg.DB}
//	    runtime, err := kafka.NewAtLeastOnceRuntime(
//	        cfg.Kafka.Brokers,
//	        cfg.Kafka.Topic,
//	        cfg.Kafka.GroupID,
//	        processor,
//	        decodeOrder,
//	    )
//	    if err != nil {
//	        return nil, err
//	    }
//	    return queue.NewApp(runtime), nil
//	}
//
// # Message Decoding
//
// Both runtimes accept a decoder function that converts Kafka message bytes into
// your application's message type. This keeps the runtime generic and allows you
// to use any serialization format (JSON, Protobuf, Avro, etc.).
//
//	func decodeMyMessage(data []byte) (*MyMessage, error) {
//	    // JSON example
//	    var msg MyMessage
//	    err := json.Unmarshal(data, &msg)
//	    return &msg, err
//
//	    // Protobuf example
//	    // var msg MyMessage
//	    // err := proto.Unmarshal(data, &msg)
//	    // return &msg, err
//	}
//
// # Concurrency Model
//
// Each Kafka partition is processed concurrently in its own goroutine. When a consumer
// group rebalance occurs:
//   - Assigned partitions spawn new processing goroutines
//   - Revoked partitions gracefully shut down their goroutines
//   - All goroutines coordinate through context cancellation
//
// This provides natural parallelism and isolation, with processing throughput scaling
// with the number of partitions.
//
// # Graceful Shutdown
//
// When the application context is cancelled (e.g., on SIGTERM), the runtime:
//  1. Stops fetching new messages
//  2. Returns [queue.ErrEndOfQueue] from consumers
//  3. Allows in-flight messages to complete processing
//  4. Commits final offsets for at-least-once semantics
//  5. Closes the Kafka client
//
// The Humus framework handles signal handling automatically.
//
// # OpenTelemetry Instrumentation
//
// All message processing is automatically instrumented with OpenTelemetry tracing,
// logging, and metrics through the core queue package processors.
//
// # Tracing
//
// Traces include:
//   - Span per message with processing order visible
//   - Context propagation through consume/process/acknowledge phases
//   - Error recording in spans
//
// No additional tracing configuration is needed in your processor implementation.
//
// # Metrics
//
// The following metrics are automatically collected:
//
//	messaging.client.messages.processed - Total number of Kafka messages processed
//	  Labels: messaging.destination.name (topic), messaging.destination.partition.id
//	  Unit: {message}
//
//	messaging.client.messages.committed - Total number of Kafka messages successfully committed
//	  Labels: messaging.destination.name (topic), messaging.destination.partition.id
//	  Unit: {message}
//
//	messaging.client.processing.failures - Total number of Kafka message processing failures
//	  Labels: messaging.destination.name (topic), messaging.destination.partition.id, error.type
//	  Unit: {failure}
//	  Note: error.type is a generic classification ("processing_error") to avoid exposing sensitive information
//
// These metrics help monitor:
//   - Message throughput (messages processed per second)
//   - Consumer lag (by comparing processed vs committed)
//   - Error rates (failures per message)
//   - Partition-level performance
//
// All metrics use the OpenTelemetry meter provider configured in your application via
// otel.GetMeterProvider().
//
// # Configuration
//
// The runtimes accept franz-go client options for advanced configuration:
//
//	runtime, err := kafka.NewAtLeastOnceRuntime(
//	    brokers,
//	    topic,
//	    groupID,
//	    processor,
//	    decoder,
//	    kafka.WithKafkaOptions(
//	        kgo.FetchMaxWait(500*time.Millisecond),
//	        kgo.SessionTimeout(10*time.Second),
//	    ),
//	)
//
// See franz-go documentation for available options:
// https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo
package kafka
