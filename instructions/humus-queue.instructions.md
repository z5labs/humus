---
description: 'Patterns and best practices for queue processors using Humus (Kafka)'
applyTo: '**/*.go'
---

# Humus Framework - Queue/Kafka Instructions

This file provides patterns and best practices specific to queue processors using Humus (particularly Kafka). Use this file alongside `humus-common.instructions.md` for complete guidance.

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
    "github.com/z5labs/humus"
    "github.com/z5labs/humus/queue"
    "github.com/z5labs/humus/queue/kafka"
)

type Config struct {
    humus.Config `config:",squash"`
    
    Kafka struct {
        Brokers []string `config:"brokers"`
        Topic   string   `config:"topic"`
        GroupID string   `config:"group_id"`
    } `config:"kafka"`
}

func Init(ctx context.Context, cfg Config) (queue.Runtime, error) {
    proc := processor.New()
    
    runtime := kafka.NewAtMostOnceRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.Topic,
        cfg.Kafka.GroupID,
        proc,
    )
    
    return runtime, nil
}
```

## Processing Semantics

### At-Most-Once Processing (fast, may lose messages)

Process order: **Consume → Acknowledge → Process**

```go
processor := queue.ProcessAtMostOnce(consumer, processor, acknowledger)
```

**Use when:**
- Message loss is acceptable
- Speed is critical
- Messages are not critical (e.g., metrics, logs)

### At-Least-Once Processing (reliable, may duplicate)

Process order: **Consume → Process → Acknowledge**

```go
processor := queue.ProcessAtLeastOnce(consumer, processor, acknowledger)

// IMPORTANT: Your processor MUST be idempotent!
```

**Use when:**
- Message loss is unacceptable
- Messages are critical (e.g., payments, orders)
- You can handle duplicates (idempotent processing)

## Kafka Runtime

### At-Most-Once Runtime

```go
func Init(ctx context.Context, cfg Config) (queue.Runtime, error) {
    proc := processor.New()
    
    runtime := kafka.NewAtMostOnceRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.Topic,
        cfg.Kafka.GroupID,
        proc,
    )
    
    return runtime, nil
}
```

### At-Least-Once Runtime

```go
func Init(ctx context.Context, cfg Config) (queue.Runtime, error) {
    proc := processor.New()
    
    runtime := kafka.NewAtLeastOnceRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.Topic,
        cfg.Kafka.GroupID,
        proc,
    )
    
    return runtime, nil
}
```

## Processor Implementation

### Basic Processor

```go
package processor

import (
    "context"
    "encoding/json"
)

type Message struct {
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

func (p *Processor) Process(ctx context.Context, msg []byte) error {
    var m Message
    if err := json.Unmarshal(msg, &m); err != nil {
        return err
    }
    
    // Process the message
    // ...
    
    return nil
}
```

### Idempotent Processor (for At-Least-Once)

```go
func (p *Processor) Process(ctx context.Context, msg []byte) error {
    var m Message
    if err := json.Unmarshal(msg, &m); err != nil {
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

## Graceful Shutdown

```go
type Consumer struct {
    messages chan []byte
    done     chan struct{}
}

func (c *Consumer) Consume(ctx context.Context) ([]byte, error) {
    select {
    case <-ctx.Done():
        return nil, queue.ErrEndOfQueue  // Signals graceful shutdown
    case msg := <-c.messages:
        return msg, nil
    }
}
```

## Queue-Specific Best Practices

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

## Common Queue Pitfalls

### Ignoring Queue Semantics

❌ **Wrong (non-idempotent at-least-once processor):**
```go
processor := queue.ProcessAtLeastOnce(consumer, func(ctx context.Context, msg []byte) error {
    balance += amount  // DANGER: Will double-count if message is reprocessed!
    return nil
}, acknowledger)
```

✅ **Correct (idempotent processor):**
```go
processor := queue.ProcessAtLeastOnce(consumer, func(ctx context.Context, msg []byte) error {
    // Check if already processed
    if alreadyProcessed(messageID) {
        return nil  // Skip duplicate
    }
    balance += amount
    markProcessed(messageID)
    return nil
}, acknowledger)
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
