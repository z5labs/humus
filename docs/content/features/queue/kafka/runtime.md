---
title: Runtime Configuration
description: Advanced Kafka runtime configuration
weight: 20
type: docs
---


The Kafka runtime provides extensive configuration options for controlling consumer behavior, fetch settings, and topic processing.

## Creating a Runtime

The basic runtime constructor:

```go
func NewRuntime(
    brokers []string,
    groupID string,
    opts ...Option,
) Runtime
```

**Parameters:**
- `brokers` - List of Kafka broker addresses (e.g., `[]string{"localhost:9092"}`)
- `groupID` - Consumer group ID for offset management and rebalancing
- `opts` - Variadic configuration options

**Example:**
```go
runtime := kafka.NewRuntime(
    []string{"kafka1:9092", "kafka2:9092", "kafka3:9092"},
    "my-consumer-group",
    kafka.AtLeastOnce("orders", ordersProcessor),
    kafka.SessionTimeout(10 * time.Second),
)
```

## Topic Configuration

Configure which topics to consume and how to process them:

### AtLeastOnce

Reliable processing with message acknowledgment after successful processing:

```go
func AtLeastOnce(topic string, processor queue.Processor[kafka.Message]) Option
```

**Example:**
```go
type OrderProcessor struct{}

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Process message (must be idempotent)
    return nil
}

runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", &OrderProcessor{}),
)
```

**Guarantees:**
- Messages acknowledged only after successful processing
- Failed processing results in redelivery
- Requires idempotent processors

### AtMostOnce

Fast processing with message acknowledgment before processing:

```go
func AtMostOnce(topic string, processor queue.Processor[kafka.Message]) Option
```

**Example:**
```go
type MetricsProcessor struct{}

func (p *MetricsProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Process message (may be lost on failure)
    return nil
}

runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtMostOnce("metrics", &MetricsProcessor{}),
)
```

**Guarantees:**
- Messages acknowledged immediately after consumption
- Processing failures result in message loss
- Higher throughput

### Multiple Topics

Process multiple topics with different semantics:

```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", ordersProcessor),
    kafka.AtLeastOnce("payments", paymentsProcessor),
    kafka.AtMostOnce("metrics", metricsProcessor),
    kafka.AtMostOnce("logs", logsProcessor),
)
```

Each topic gets its own processor and delivery semantics. See [Multi-Topic Processing]({{< ref "multi-topic" >}}) for details.

## Consumer Group Settings

Configure consumer group behavior and rebalancing:

### SessionTimeout

Maximum time between heartbeats before a consumer is considered dead:

```go
func SessionTimeout(d time.Duration) Option
```

**Default:** 45 seconds

**Example:**
```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", processor),
    kafka.SessionTimeout(10 * time.Second),
)
```

**Guidelines:**
- **Short timeout (5-15s):** Fast failure detection, but may cause false positives during GC pauses
- **Long timeout (30-60s):** Tolerates GC pauses, but slower failure detection
- **Production recommendation:** 20-30 seconds

### RebalanceTimeout

Maximum time allowed for rebalance operations:

```go
func RebalanceTimeout(d time.Duration) Option
```

**Default:** 30 seconds

**Example:**
```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", processor),
    kafka.RebalanceTimeout(60 * time.Second),
)
```

**Guidelines:**
- Should be longer than session timeout
- Increase if rebalances frequently timeout
- **Production recommendation:** 45-60 seconds

## Fetch Settings

Control how messages are fetched from Kafka:

### FetchMaxBytes

Maximum total bytes to buffer from fetch responses across all partitions:

```go
func FetchMaxBytes(bytes int32) Option
```

**Default:** 50 MB (52,428,800 bytes)

**Example:**
```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", processor),
    kafka.FetchMaxBytes(100 * 1024 * 1024), // 100 MB
)
```

**Guidelines:**
- **Small messages:** Lower value (10-25 MB) reduces memory usage
- **Large messages:** Higher value (100+ MB) improves throughput
- Must be larger than largest single message
- **Production recommendation:** 50-100 MB

### MaxConcurrentFetches

Maximum number of concurrent fetch requests to Kafka:

```go
func MaxConcurrentFetches(fetches int) Option
```

**Default:** Unlimited (0)

**Example:**
```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", processor),
    kafka.MaxConcurrentFetches(10),
)
```

**Guidelines:**
- **Unlimited (0):** Maximum throughput, higher network load
- **Limited (5-10):** Controlled network load, predictable resource usage
- **Production recommendation:** 5-10 for most workloads

## Configuration Examples

### High-Throughput Configuration

Optimize for maximum message throughput:

```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtMostOnce("events", processor),
    kafka.FetchMaxBytes(200 * 1024 * 1024),  // 200 MB
    kafka.MaxConcurrentFetches(0),           // Unlimited
    kafka.SessionTimeout(45 * time.Second),
    kafka.RebalanceTimeout(60 * time.Second),
)
```

**Use cases:**
- Event streaming
- Log aggregation
- Metrics collection

### High-Reliability Configuration

Optimize for message reliability and ordered processing:

```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("transactions", processor),
    kafka.FetchMaxBytes(10 * 1024 * 1024),   // 10 MB
    kafka.MaxConcurrentFetches(5),
    kafka.SessionTimeout(20 * time.Second),
    kafka.RebalanceTimeout(45 * time.Second),
)
```

**Use cases:**
- Financial transactions
- Database replication
- Critical event processing

### Balanced Configuration

General-purpose configuration for most workloads:

```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", processor),
    kafka.FetchMaxBytes(50 * 1024 * 1024),   // 50 MB (default)
    kafka.MaxConcurrentFetches(10),
    kafka.SessionTimeout(30 * time.Second),
    kafka.RebalanceTimeout(45 * time.Second),
)
```

**Use cases:**
- General message processing
- Microservice communication
- Event-driven workflows

## Environment-Based Configuration

Use YAML templating for environment-specific settings:

**config.yaml:**
```yaml
kafka:
  brokers:
    - "{{env "KAFKA_BROKER_1" | default "localhost:9092"}}"
    - "{{env "KAFKA_BROKER_2" | default "localhost:9093"}}"
  group_id: "{{env "KAFKA_GROUP_ID" | default "my-service"}}"
  topic: "{{env "KAFKA_TOPIC" | default "events"}}"
  session_timeout: "{{env "KAFKA_SESSION_TIMEOUT" | default "30s"}}"
  fetch_max_bytes: {{env "KAFKA_FETCH_MAX_BYTES" | default "52428800"}}

otel:
  service:
    name: "{{env "SERVICE_NAME" | default "queue-processor"}}"
```

**Parsing in code:**
```go
type Config struct {
    queue.Config `config:",squash"`
    Kafka struct {
        Brokers        []string      `config:"brokers"`
        GroupID        string        `config:"group_id"`
        Topic          string        `config:"topic"`
        SessionTimeout time.Duration `config:"session_timeout"`
        FetchMaxBytes  int32         `config:"fetch_max_bytes"`
    } `config:"kafka"`
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce(cfg.Kafka.Topic, processor),
        kafka.SessionTimeout(cfg.Kafka.SessionTimeout),
        kafka.FetchMaxBytes(cfg.Kafka.FetchMaxBytes),
    )
    return queue.NewApp(runtime), nil
}
```

## Monitoring Configuration

Check runtime behavior through logs and metrics:

**Consumer Group Lag:**
```bash
kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 \
  --group my-consumer-group \
  --describe
```

**Key metrics to monitor:**
- Consumer lag per partition
- Messages processed per second
- Processing errors
- Rebalance frequency
- Session timeout violations

See [Observability]({{< ref "observability" >}}) for OpenTelemetry integration.

## Common Configuration Issues

### Frequent Rebalances

**Symptoms:** Consumer group frequently rebalancing

**Solutions:**
```go
// Increase session and rebalance timeouts
kafka.SessionTimeout(45 * time.Second),
kafka.RebalanceTimeout(90 * time.Second),
```

### High Memory Usage

**Symptoms:** Application consuming excessive memory

**Solutions:**
```go
// Reduce fetch buffer size
kafka.FetchMaxBytes(25 * 1024 * 1024),  // 25 MB
kafka.MaxConcurrentFetches(5),
```

### Slow Processing

**Symptoms:** Consumer lag growing, messages processed slowly

**Solutions:**
1. Check processor logic for inefficiencies
2. Increase partition count for more concurrency
3. Scale horizontally (more consumer instances)
4. Consider at-most-once for non-critical messages

### Messages Larger Than FetchMaxBytes

**Symptoms:** Errors fetching messages

**Solutions:**
```go
// Increase fetch buffer
kafka.FetchMaxBytes(200 * 1024 * 1024),  // 200 MB

// Or reduce message size at producer
```

## Next Steps

- Learn about [Message Structure]({{< ref "message" >}}) for working with message metadata
- Understand [Concurrency Model]({{< ref "concurrency" >}}) for partition processing
- Explore [Configuration]({{< ref "configuration" >}}) for production deployment patterns
