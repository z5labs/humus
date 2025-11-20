---
title: Kafka Runtime
description: Apache Kafka integration for message processing
weight: 10
type: docs
---


The Humus Kafka runtime provides a production-ready integration with Apache Kafka, offering concurrent per-partition processing, automatic OpenTelemetry instrumentation, and flexible delivery semantics.

## Overview

The Kafka runtime is built on:

- **[franz-go](https://github.com/twmb/franz-go)** - Modern, high-performance Kafka client
- **Goroutine-per-partition** - Automatic concurrency with partition isolation
- **OpenTelemetry Integration** - Built-in tracing via franz-go kotel plugin
- **Delivery Semantics** - Both at-most-once and at-least-once processing

## Quick Start

```go
package main

import (
    "context"
    "encoding/json"

    "github.com/z5labs/humus/queue"
    "github.com/z5labs/humus/queue/kafka"
)

type OrderMessage struct {
    OrderID string  `json:"order_id"`
    Amount  float64 `json:"amount"`
}

type OrderProcessor struct{}

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Deserialize
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }

    // Process order (should be idempotent)
    return nil
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    processor := &OrderProcessor{}

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

## Core Components

### Runtime

The main Kafka runtime that manages the Kafka client, consumer group, and partition processing:

```go
runtime := kafka.NewRuntime(
    brokers []string,
    groupID string,
    opts ...Option,
)
```

**Features:**
- Consumer group management
- Automatic rebalancing
- Graceful shutdown
- OpenTelemetry integration

### Message

Represents a Kafka message with all metadata:

```go
type Message struct {
    Key       []byte
    Value     []byte
    Headers   []Header
    Timestamp time.Time
    Topic     string
    Partition int32
    Offset    int64
    Attrs     uint8
}
```

Your processor receives this type and must deserialize `Value` into your application's message format.

### Delivery Semantics

Configure processing semantics per topic:

**At-Least-Once:**
```go
kafka.AtLeastOnce(topic, processor)
```

Messages acknowledged after successful processing. Requires idempotent processors.

**At-Most-Once:**
```go
kafka.AtMostOnce(topic, processor)
```

Messages acknowledged before processing. Fast but may lose messages on failures.

## Configuration Options

### Consumer Group Settings

**SessionTimeout:**
```go
kafka.SessionTimeout(10 * time.Second)
```
Default: 45 seconds. Maximum time between heartbeats before considered dead.

**RebalanceTimeout:**
```go
kafka.RebalanceTimeout(30 * time.Second)
```
Default: 30 seconds. Maximum time for rebalance operations.

### Fetch Settings

**FetchMaxBytes:**
```go
kafka.FetchMaxBytes(100 * 1024 * 1024) // 100 MB
```
Default: 50 MB. Maximum bytes to fetch across all partitions per request.

**MaxConcurrentFetches:**
```go
kafka.MaxConcurrentFetches(5)
```
Default: unlimited. Limit concurrent fetch requests to Kafka.

## Multi-Topic Processing

Process multiple topics in a single runtime:

```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", ordersProcessor),
    kafka.AtLeastOnce("events", eventsProcessor),
    kafka.AtMostOnce("metrics", metricsProcessor),
)
```

Each topic can have different processors and delivery semantics.

## Concurrency Model

The Kafka runtime uses a **goroutine-per-partition** architecture:

```
Topic "orders" with 3 partitions:
  ├─ Partition 0 → Goroutine 1
  ├─ Partition 1 → Goroutine 2
  └─ Partition 2 → Goroutine 3
```

**Benefits:**
- Natural parallelism scaling with partition count
- Partition isolation (one slow partition doesn't block others)
- Automatic coordination via consumer group

**Rebalancing:**
- Assigned partitions spawn new goroutines
- Revoked partitions gracefully shut down
- Context cancellation coordinates all goroutines

See [Concurrency Model]({{< ref "concurrency" >}}) for details.

## Built-in Features

Every Kafka runtime automatically includes:

- **Consumer Group Management** - Automatic partition assignment and rebalancing
- **OpenTelemetry Tracing** - Spans per message with context propagation
- **OpenTelemetry Metrics** - Message processing, commits, and failure metrics
- **Graceful Shutdown** - Clean shutdown on SIGTERM/SIGINT with offset commits
- **Error Handling** - Structured logging with message metadata

## What You'll Learn

This section covers:

- [Quick Start]({{< ref "quick-start" >}}) - Build your first Kafka processor
- [Runtime Configuration]({{< ref "runtime" >}}) - Advanced configuration options
- [Message Structure]({{< ref "message" >}}) - Working with Kafka messages
- [Concurrency Model]({{< ref "concurrency" >}}) - Understanding partition processing
- [Idempotency]({{< ref "idempotency" >}}) - Handling duplicate messages
- [Multi-Topic Processing]({{< ref "multi-topic" >}}) - Processing multiple topics
- [Observability]({{< ref "observability" >}}) - OpenTelemetry integration
- [Configuration]({{< ref "configuration" >}}) - Production deployment

## Next Steps

Start with the [Quick Start Guide]({{< ref "quick-start" >}}) to build your first Kafka message processor.
