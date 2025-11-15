---
title: Quick Start
description: Build your first Kafka message processor
weight: 10
type: docs
---

# Kafka Quick Start

This guide walks you through building a complete Kafka message processor with at-least-once delivery semantics.

## Prerequisites

- Go 1.21 or later
- Kafka cluster (local or remote)
- Humus installed (`go get github.com/z5labs/humus`)

## Running Kafka Locally

If you don't have Kafka running, start it with Docker:

```bash
docker run -d \
  --name kafka \
  -p 9092:9092 \
  -e KAFKA_ENABLE_KRAFT=yes \
  -e KAFKA_CFG_NODE_ID=1 \
  -e KAFKA_CFG_PROCESS_ROLES=broker,controller \
  -e KAFKA_CFG_CONTROLLER_QUORUM_VOTERS=1@127.0.0.1:9093 \
  -e KAFKA_CFG_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093 \
  -e KAFKA_CFG_ADVERTISED_LISTENERS=PLAINTEXT://127.0.0.1:9092 \
  -e KAFKA_CFG_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT \
  -e KAFKA_CFG_CONTROLLER_LISTENER_NAMES=CONTROLLER \
  bitnami/kafka:latest
```

Create a test topic:

```bash
docker exec kafka kafka-topics.sh \
  --create \
  --topic orders \
  --bootstrap-server localhost:9092 \
  --partitions 3 \
  --replication-factor 1
```

## Project Setup

```bash
mkdir order-processor
cd order-processor
go mod init order-processor
go get github.com/z5labs/humus
```

## Configuration

Create `config.yaml`:

```yaml
kafka:
  brokers:
    - "localhost:9092"
  group_id: "order-processors"
  topic: "orders"

otel:
  service:
    name: order-processor
  sdk:
    disabled: true  # Disable for this example
```

## Define Your Message

Create `main.go`:

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "sync"

    "github.com/z5labs/humus/queue"
    "github.com/z5labs/humus/queue/kafka"
)

// OrderMessage represents an order from Kafka
type OrderMessage struct {
    OrderID    string  `json:"order_id"`
    CustomerID string  `json:"customer_id"`
    Amount     float64 `json:"amount"`
}

// Config holds application configuration
type Config struct {
    queue.Config `config:",squash"`
    Kafka struct {
        Brokers []string `config:"brokers"`
        GroupID string   `config:"group_id"`
        Topic   string   `config:"topic"`
    } `config:"kafka"`
}
```

## Implement the Processor

An idempotent processor that tracks processed orders:

```go
// OrderProcessor processes order messages
type OrderProcessor struct {
    mu        sync.RWMutex
    processed map[string]bool
}

func NewOrderProcessor() *OrderProcessor {
    return &OrderProcessor{
        processed: make(map[string]bool),
    }
}

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Deserialize the message
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return fmt.Errorf("failed to unmarshal order: %w", err)
    }

    // Idempotency check
    p.mu.RLock()
    if p.processed[order.OrderID] {
        p.mu.RUnlock()
        fmt.Printf("Order %s already processed, skipping\n", order.OrderID)
        return nil
    }
    p.mu.RUnlock()

    // Process the order
    fmt.Printf("Processing order: ID=%s, Customer=%s, Amount=$%.2f\n",
        order.OrderID,
        order.CustomerID,
        order.Amount,
    )

    // Simulate order processing
    // In production: save to database, call payment service, etc.

    // Mark as processed
    p.mu.Lock()
    p.processed[order.OrderID] = true
    p.mu.Unlock()

    return nil
}
```

## Initialize the Runtime

Configure the Kafka runtime with at-least-once processing:

```go
func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    processor := NewOrderProcessor()

    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce(cfg.Kafka.Topic, processor),
    )

    return queue.NewApp(runtime), nil
}

func main() {
    queue.Run(queue.YamlSource("config.yaml"), Init)
}
```

## Run the Processor

```bash
go run main.go
```

You should see output indicating the processor is running:

```
INFO Starting order-processor
INFO Kafka consumer group initialized group_id=order-processors
```

## Test with Messages

In another terminal, produce test messages to Kafka:

```bash
# Message 1
echo '{"order_id":"ord-001","customer_id":"cust-123","amount":99.99}' | \
  docker exec -i kafka kafka-console-producer.sh \
    --broker-list localhost:9092 \
    --topic orders

# Message 2
echo '{"order_id":"ord-002","customer_id":"cust-456","amount":149.99}' | \
  docker exec -i kafka kafka-console-producer.sh \
    --broker-list localhost:9092 \
    --topic orders

# Duplicate of Message 1 (to test idempotency)
echo '{"order_id":"ord-001","customer_id":"cust-123","amount":99.99}' | \
  docker exec -i kafka kafka-console-producer.sh \
    --broker-list localhost:9092 \
    --topic orders
```

Your processor should output:

```
Processing order: ID=ord-001, Customer=cust-123, Amount=$99.99
Processing order: ID=ord-002, Customer=cust-456, Amount=$149.99
Order ord-001 already processed, skipping
```

Notice the duplicate message was detected and skipped.

## What's Happening

Let's break down the key components:

### 1. Configuration

The YAML config provides Kafka connection details and consumer group settings:

```yaml
kafka:
  brokers: ["localhost:9092"]  # Kafka broker addresses
  group_id: "order-processors" # Consumer group for offset tracking
  topic: "orders"              # Topic to consume from
```

### 2. Message Deserialization

The processor receives `kafka.Message` with raw bytes:

```go
func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order OrderMessage
    json.Unmarshal(msg.Value, &order)
    // ...
}
```

`msg.Value` contains the JSON bytes, which we deserialize into `OrderMessage`.

### 3. Idempotent Processing

The processor tracks processed order IDs to handle duplicates:

```go
if p.processed[order.OrderID] {
    return nil // Skip duplicate
}
// Process...
p.processed[order.OrderID] = true
```

This is critical for at-least-once processing where Kafka may redeliver messages.

### 4. At-Least-Once Semantics

```go
kafka.AtLeastOnce(cfg.Kafka.Topic, processor)
```

This ensures:
- Messages are processed before offsets are committed
- Failed processing results in message redelivery
- No messages are lost due to processing failures

### 5. Graceful Shutdown

Press Ctrl+C to stop the processor. You'll see:

```
INFO Shutting down gracefully
INFO Committing final offsets
INFO Kafka client closed
```

The framework ensures in-flight messages complete before shutdown.

## Production Considerations

This example uses in-memory state. For production:

### Database-Backed Idempotency

Replace the in-memory map with database storage:

```go
func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }

    // Check database for existing order
    var exists bool
    err := p.db.QueryRowContext(ctx,
        "SELECT EXISTS(SELECT 1 FROM orders WHERE order_id = $1)",
        order.OrderID,
    ).Scan(&exists)
    if err != nil {
        return err
    }
    if exists {
        return nil // Already processed
    }

    // Process in transaction
    tx, err := p.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx,
        "INSERT INTO orders (order_id, customer_id, amount) VALUES ($1, $2, $3)",
        order.OrderID, order.CustomerID, order.Amount,
    )
    if err != nil {
        return err
    }

    return tx.Commit()
}
```

See [Idempotency]({{< ref "idempotency" >}}) for detailed patterns.

### Enable OpenTelemetry

For production observability, enable OTel in `config.yaml`:

```yaml
otel:
  service:
    name: order-processor
  sdk:
    disabled: false
  exporter:
    otlp:
      endpoint: "localhost:4317"
      protocol: grpc
```

See [Observability]({{< ref "observability" >}}) for details.

### Tune Performance

Adjust fetch settings for your workload:

```go
runtime := kafka.NewRuntime(
    cfg.Kafka.Brokers,
    cfg.Kafka.GroupID,
    kafka.AtLeastOnce(cfg.Kafka.Topic, processor),
    kafka.FetchMaxBytes(100 * 1024 * 1024), // 100 MB
    kafka.MaxConcurrentFetches(10),
)
```

See [Configuration]({{< ref "configuration" >}}) for tuning guidance.

## Complete Code

<details>
<summary>Full main.go (click to expand)</summary>

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "sync"

    "github.com/z5labs/humus/queue"
    "github.com/z5labs/humus/queue/kafka"
)

type OrderMessage struct {
    OrderID    string  `json:"order_id"`
    CustomerID string  `json:"customer_id"`
    Amount     float64 `json:"amount"`
}

type Config struct {
    queue.Config `config:",squash"`
    Kafka struct {
        Brokers []string `config:"brokers"`
        GroupID string   `config:"group_id"`
        Topic   string   `config:"topic"`
    } `config:"kafka"`
}

type OrderProcessor struct {
    mu        sync.RWMutex
    processed map[string]bool
}

func NewOrderProcessor() *OrderProcessor {
    return &OrderProcessor{
        processed: make(map[string]bool),
    }
}

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return fmt.Errorf("failed to unmarshal order: %w", err)
    }

    p.mu.RLock()
    if p.processed[order.OrderID] {
        p.mu.RUnlock()
        fmt.Printf("Order %s already processed, skipping\n", order.OrderID)
        return nil
    }
    p.mu.RUnlock()

    fmt.Printf("Processing order: ID=%s, Customer=%s, Amount=$%.2f\n",
        order.OrderID,
        order.CustomerID,
        order.Amount,
    )

    p.mu.Lock()
    p.processed[order.OrderID] = true
    p.mu.Unlock()

    return nil
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    processor := NewOrderProcessor()

    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce(cfg.Kafka.Topic, processor),
    )

    return queue.NewApp(runtime), nil
}

func main() {
    queue.Run(queue.YamlSource("config.yaml"), Init)
}
```

</details>

## Next Steps

- Learn about [Message Structure]({{< ref "message" >}}) to work with headers and metadata
- Explore [Multi-Topic Processing]({{< ref "multi-topic" >}}) to handle multiple topics
- Understand [Concurrency Model]({{< ref "concurrency" >}}) for partition-level parallelism
- Implement robust [Idempotency]({{< ref "idempotency" >}}) patterns for production
