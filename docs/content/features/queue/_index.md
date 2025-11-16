---
title: Queue Services
description: Processing messages from queues with flexible delivery semantics
weight: 40
type: docs
---

# Queue Services

Humus queue services provide a complete framework for processing messages from message queues with configurable delivery semantics, automatic concurrency management, and built-in observability.

## Overview

Queue services in Humus are built on:

- **Pluggable Runtimes** - Support for different message queue systems (Kafka, and more)
- **Delivery Semantics** - Choose between at-most-once and at-least-once processing
- **Type Safety** - Compile-time type checking for message processors
- **OpenTelemetry** - Automatic tracing and metrics for message processing

## Quick Start

```go
package main

import (
    "context"
    "encoding/json"

    "github.com/z5labs/humus/queue"
    "github.com/z5labs/humus/queue/kafka"
)

type Config struct {
    queue.Config `config:",squash"`
    Kafka struct {
        Brokers []string `config:"brokers"`
        GroupID string   `config:"group_id"`
        Topic   string   `config:"topic"`
    } `config:"kafka"`
}

type OrderMessage struct {
    OrderID string  `json:"order_id"`
    Amount  float64 `json:"amount"`
}

type OrderProcessor struct{}

func (p *OrderProcessor) Process(ctx context.Context, msg *OrderMessage) error {
    // Process the order
    // This should be idempotent for at-least-once processing
    return nil
}

func decodeOrder(data []byte) (*OrderMessage, error) {
    var msg OrderMessage
    err := json.Unmarshal(data, &msg)
    return &msg, err
}

func main() {
    queue.Run(queue.YamlSource("config.yaml"), Init)
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    processor := &OrderProcessor{}
    runtime, err := kafka.NewAtLeastOnceRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.Topic,
        cfg.Kafka.GroupID,
        processor,
        decodeOrder,
    )
    if err != nil {
        return nil, err
    }
    return queue.NewApp(runtime), nil
}
```

## Core Concepts

### Three-Phase Processing Pattern

Queue processing follows a three-phase pattern:

1. **Consumer** - Retrieves messages from the queue
2. **Processor** - Executes business logic on messages
3. **Acknowledger** - Confirms successful processing

The order of these phases determines the delivery semantics.

### Delivery Semantics

**At-Most-Once** (Consume → Acknowledge → Process):
- Messages acknowledged before processing
- Fast throughput, but processing failures lose messages
- Suitable for non-critical data (metrics, logs, caching)

**At-Least-Once** (Consume → Process → Acknowledge):
- Messages acknowledged after successful processing
- Reliable delivery, but may deliver duplicates
- Requires idempotent processors
- Suitable for critical operations (financial, database updates)

### Runtime Interface

All queue implementations provide a `Runtime` that orchestrates the processing phases:

```go
type Runtime interface {
    ProcessQueue(ctx context.Context) error
}
```

### Available Runtimes

Currently supported:
- **[Kafka]({{< ref "kafka" >}})** - Apache Kafka via franz-go client

## Built-in Features

Every queue service automatically includes:

- **Graceful Shutdown** - Clean shutdown on SIGTERM/SIGINT
- **OpenTelemetry Tracing** - Automatic spans for each message
- **Context Propagation** - Distributed tracing across services
- **Lifecycle Management** - Managed by Bedrock framework

## What You'll Learn

This section covers:

- [Queue Framework]({{< ref "queue-framework" >}}) - Core abstractions and patterns
- [Delivery Semantics]({{< ref "delivery-semantics" >}}) - At-most-once vs at-least-once
- [Kafka Runtime]({{< ref "kafka" >}}) - Apache Kafka integration

## Next Steps

Start with the [Kafka Quick Start Guide]({{< ref "kafka/quick-start" >}}) to build your first queue processor.
