---
description: 'Patterns and best practices for Kafka queue processors using Humus (queue/kafka package)'
applyTo: '**/*.go'
---

# Humus Framework - Kafka Queue Instructions

This file provides patterns and best practices specific to Kafka queue processors using the `github.com/z5labs/humus/queue/kafka` package. Use this file alongside `humus-common.instructions.md` for complete guidance.

Note: The `queue` package provides abstractions but no runtime implementations. All runtime functionality is provided by `queue/kafka`.

## Project Structure

```
my-queue-service/
├── main.go
├── config.yaml
├── app/
│   └── app.go          # Init function
├── processor/
│   └── processor.go    # Message processing logic
├── go.mod
└── go.sum
```

## Queue Processing Patterns

### Entry Point

**main.go:**
```go
package main

import (
    "bytes"
    _ "embed"
    "github.com/z5labs/humus/queue"
    "my-queue-service/app"
)

//go:embed config.yaml
var configBytes []byte

func main() {
    queue.Run(bytes.NewReader(configBytes), app.Init)
}
```

### Init Function

**app/app.go:**
```go
package app

import (
    "context"
    "my-queue-service/processor"
    "github.com/z5labs/humus/queue"
    "github.com/z5labs/humus/queue/kafka"
)

type Config struct {
    queue.Config `config:",squash"`
    
    Kafka struct {
        Brokers []string `config:"brokers"`
        Topic   string   `config:"topic"`
        GroupID string   `config:"group_id"`
    } `config:"kafka"`
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    proc := processor.New()
    
    // Create Kafka runtime with at-least-once semantics
    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce(cfg.Kafka.Topic, proc),
    )
    
    return queue.NewApp(runtime), nil
}
```

## Processing Semantics

The `queue/kafka` package provides two processing semantics via options to `kafka.NewRuntime`:

### At-Most-Once Processing (fast, may lose messages)

Process order: **Consume → Acknowledge → Process**

```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtMostOnce(topic, processor),
)
```

**Use when:**
- Message loss is acceptable
- Speed is critical
- Messages are not critical (e.g., metrics, logs)

### At-Least-Once Processing (reliable, may duplicate)

Process order: **Consume → Process → Acknowledge**

```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce(topic, processor),
)

// IMPORTANT: Your processor MUST be idempotent!
```

**Use when:**
- Message loss is unacceptable
- Messages are critical (e.g., payments, orders)
- You can handle duplicates (idempotent processing)

## Kafka Runtime Options

### Basic Runtime

```go
func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    proc := processor.New()
    
    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtMostOnce(cfg.Kafka.Topic, proc),
    )
    
    return queue.NewApp(runtime), nil
}
```

### Runtime with Multiple Options

```go
func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    proc := processor.New()
    
    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce(cfg.Kafka.Topic, proc),
        kafka.SessionTimeout(45 * time.Second),
        kafka.RebalanceTimeout(30 * time.Second),
        kafka.FetchMaxBytes(50 * 1024 * 1024),
    )
    
    return queue.NewApp(runtime), nil
}
```

### Runtime with TLS/mTLS

```go
func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    proc := processor.New()
    
    tlsConfig := &tls.Config{
        Certificates: []tls.Certificate{cert},
        RootCAs:      caCertPool,
        MinVersion:   tls.VersionTLS12,
    }
    
    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce(cfg.Kafka.Topic, proc),
        kafka.WithTLS(tlsConfig),
    )
    
    return queue.NewApp(runtime), nil
}
```

## Processor Implementation

Processors implement the `queue.Processor[kafka.Message]` interface to process Kafka messages:

### Basic Processor

```go
package processor

import (
    "context"
    "encoding/json"
    "github.com/z5labs/humus/queue/kafka"
)

type OrderMessage struct {
    ID      string `json:"id"`
    UserID  string `json:"user_id"`
    Action  string `json:"action"`
}

type Processor struct {
    // Dependencies
}

func New() *Processor {
    return &Processor{}
}

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var m OrderMessage
    if err := json.Unmarshal(msg.Value, &m); err != nil {
        return err
    }
    
    // Process the message
    // msg.Topic, msg.Partition, msg.Offset are also available
    
    return nil
}
```

### Idempotent Processor (for At-Least-Once)

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var m OrderMessage
    if err := json.Unmarshal(msg.Value, &m); err != nil {
        return err
    }
    
    // Check if already processed (idempotency)
    if p.isProcessed(m.ID) {
        return nil  // Skip duplicate
    }
    
    // Process the message
    if err := p.processMessage(m); err != nil {
        return err
    }
    
    // Mark as processed
    return p.markProcessed(m.ID)
}
```

## Kafka-Specific Best Practices

### DO ✅

1. **Make at-least-once processors idempotent** - they may process messages multiple times
2. **Use at-least-once for critical messages** - payments, orders, important events
3. **Use at-most-once for non-critical messages** - metrics, logs, analytics
4. **Handle ErrEndOfQueue for graceful shutdown** - return it from Consumer.Consume()
5. **Use goroutine-per-partition model** - Kafka runtime handles this automatically

### DON'T ❌

1. **Don't ignore delivery semantics** - understand at-most-once vs at-least-once
2. **Don't use at-least-once without idempotency** - messages will be duplicated
3. **Don't block in processors** - it affects partition throughput
4. **Don't panic in processors** - return errors instead
5. **Don't forget to handle context cancellation** - for graceful shutdown

## Common Kafka Pitfalls

### Ignoring Delivery Semantics

❌ **Wrong (non-idempotent at-least-once processor):**
```go
type NonIdempotentProcessor struct {
    balance int
}

func (p *NonIdempotentProcessor) Process(ctx context.Context, msg kafka.Message) error {
    p.balance += amount  // DANGER: Will double-count if message is reprocessed!
    return nil
}
```

✅ **Correct (idempotent processor):**
```go
type IdempotentProcessor struct {
    processed map[string]bool
    balance   int
}

func (p *IdempotentProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }
    
    // Check if already processed
    if p.processed[order.ID] {
        return nil  // Skip duplicate
    }
    
    p.balance += order.Amount
    p.processed[order.ID] = true
    return nil
}
```

## Example Projects

Study these examples in the Humus repository:

- **At-Most-Once**: `example/queue/kafka-at-most-once/` - Fast processing
- **At-Least-Once**: `example/queue/kafka-at-least-once/` - Reliable processing
- **mTLS**: `example/queue/kafka-mtls-at-least-once/` - Secure Kafka connections

## Additional Resources

- **Queue Documentation**: https://z5labs.dev/humus/features/queue/
- **Kafka Documentation**: https://z5labs.dev/humus/features/queue/kafka/
- **Common patterns**: See `humus-common.instructions.md`
