---
title: Observability
description: OpenTelemetry integration for Kafka processing
weight: 70
type: docs
---


The Kafka runtime provides comprehensive OpenTelemetry integration with automatic tracing, metrics, and structured logging for message processing.

## Overview

Observability is built-in at every level:

- **Automatic Tracing** - Spans created for each message via franz-go kotel plugin
- **Context Propagation** - Distributed tracing across services
- **Structured Logging** - Message metadata in log entries
- **Metrics** - Consumer lag, processing rates, errors (via OTel SDK)

No manual instrumentation needed in your processor code.

## Tracing

### Automatic Span Creation

Every message gets a processing span automatically:

```
Span: kafka.process
  ├─ topic: "orders"
  ├─ partition: 0
  ├─ offset: 12345
  ├─ group_id: "order-processors"
  └─ duration: 45ms
```

### Trace Propagation

Trace context is automatically extracted from Kafka headers:

```
Producer (orders-api):
  HTTP Request → Span A (trace-id: abc123)
    └─> Publish to Kafka (inject trace-id into headers)

Consumer (order-processor):
  Consume from Kafka → Extract trace-id from headers
    └─> Span B (trace-id: abc123, parent: Span A)
```

This creates a distributed trace across services.

### Custom Spans in Processor

Add child spans for detailed tracing:

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("order-processor")

type OrderProcessor struct {
    db *sql.DB
}

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Parent span already created by kafka runtime

    // Add custom child span
    ctx, span := tracer.Start(ctx, "deserialize-order")
    var order Order
    err := json.Unmarshal(msg.Value, &order)
    span.End()
    if err != nil {
        return err
    }

    // Another span for database operation
    ctx, span = tracer.Start(ctx, "save-order")
    defer span.End()

    _, err = p.db.ExecContext(ctx,
        "INSERT INTO orders (order_id, total) VALUES ($1, $2)",
        order.OrderID, order.Total,
    )

    if err != nil {
        span.RecordError(err)
    }

    return err
}
```

### Span Attributes

Add custom attributes to spans:

```go
import "go.opentelemetry.io/otel/attribute"

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    ctx, span := tracer.Start(ctx, "process-order")
    defer span.End()

    var order Order
    json.Unmarshal(msg.Value, &order)

    // Add business context to span
    span.SetAttributes(
        attribute.String("order.id", order.OrderID),
        attribute.String("customer.id", order.CustomerID),
        attribute.Float64("order.total", order.Total),
        attribute.String("order.status", order.Status),
    )

    return p.processOrder(ctx, order)
}
```

## Logging

### Structured Logging with slog

Use the built-in logger with Kafka attributes:

```go
import (
    "log/slog"
    "github.com/z5labs/humus/queue/kafka"
)

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order Order
    json.Unmarshal(msg.Value, &order)

    log.InfoContext(ctx, "Processing order",
        kafka.TopicAttr(msg.Topic),
        kafka.PartitionAttr(msg.Partition),
        kafka.OffsetAttr(msg.Offset),
        slog.String("order_id", order.OrderID),
        slog.Float64("amount", order.Total),
    )

    if err := p.saveOrder(ctx, order); err != nil {
        log.ErrorContext(ctx, "Failed to save order",
            kafka.TopicAttr(msg.Topic),
            kafka.PartitionAttr(msg.Partition),
            kafka.OffsetAttr(msg.Offset),
            slog.String("order_id", order.OrderID),
            slog.Any("error", err),
        )
        return err
    }

    return nil
}
```

### Available Kafka Attributes

The `kafka` package provides slog attributes:

```go
// Kafka-specific attributes
kafka.GroupIDAttr(groupID string)      // Consumer group ID
kafka.TopicAttr(topic string)          // Topic name
kafka.PartitionAttr(partition int32)   // Partition number
kafka.OffsetAttr(offset int64)         // Message offset
```

**Example:**
```go
log.Info("Consumer group started",
    kafka.GroupIDAttr("order-processors"),
    kafka.TopicAttr("orders"),
)
```

### Log Correlation with Traces

Logs are automatically correlated with traces when using `log/slog` with context:

```go
func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Log with context - automatically includes trace ID
    log.InfoContext(ctx, "Processing message",
        kafka.TopicAttr(msg.Topic),
        kafka.OffsetAttr(msg.Offset),
    )

    // This log will have the same trace ID in your logging backend
    return nil
}
```

## Metrics

### Automatic Metrics

The Kafka runtime automatically exports metrics for monitoring consumer health and throughput:

**Consumer Metrics (via franz-go kotel plugin):**
- Consumer lag per partition
- Messages consumed per second
- Bytes consumed per second
- Fetch latency
- Commit latency

**Processing Metrics (via Humus Kafka runtime):**

The following metrics are automatically recorded:

- **`messaging.client.processed.messages`** (counter)
  - Description: Total number of Kafka messages successfully processed
  - Unit: `{message}`
  - Labels:
    - `topic`: Kafka topic name
    - `partition`: Partition number

- **`messaging.client.committed.messages`** (counter)
  - Description: Total number of Kafka messages committed
  - Unit: `{message}`
  - Labels:
    - `topic`: Kafka topic name
    - `partition`: Partition number

- **`messaging.process.failures`** (counter)
  - Description: Total number of Kafka message processing failures
  - Unit: `{failure}`
  - Labels:
    - `topic`: Kafka topic name
    - `partition`: Partition number

**Using These Metrics:**

These metrics help identify:
- Processing bottlenecks (low processed count)
- Commit failures (committed count lower than processed for at-least-once)
- Error rates (high failure count)
- Partition imbalances (uneven distribution across partitions)

**Example PromQL Queries:**

```promql
# Messages processed per second by topic
rate(messaging_client_processed_messages_total{topic="orders"}[1m])

# Processing failure rate
rate(messaging_process_failures_total[5m])

# Commit success ratio (at-least-once only)
messaging_client_committed_messages_total / messaging_client_processed_messages_total

# Processing by partition (identify hotspots)
sum by (partition) (messaging_client_processed_messages_total{topic="orders"})
```

### Custom Metrics

Add application-specific metrics:

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/metric"
)

type OrderProcessor struct {
    db              *sql.DB
    ordersProcessed metric.Int64Counter
    orderValue      metric.Float64Histogram
}

func NewOrderProcessor(db *sql.DB) (*OrderProcessor, error) {
    meter := otel.Meter("order-processor")

    ordersProcessed, err := meter.Int64Counter(
        "orders.processed",
        metric.WithDescription("Number of orders processed"),
    )
    if err != nil {
        return nil, err
    }

    orderValue, err := meter.Float64Histogram(
        "orders.value",
        metric.WithDescription("Order total value"),
        metric.WithUnit("USD"),
    )
    if err != nil {
        return nil, err
    }

    return &OrderProcessor{
        db:              db,
        ordersProcessed: ordersProcessed,
        orderValue:      orderValue,
    }, nil
}

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order Order
    json.Unmarshal(msg.Value, &order)

    // Process order
    if err := p.saveOrder(ctx, order); err != nil {
        return err
    }

    // Record metrics
    p.ordersProcessed.Add(ctx, 1,
        metric.WithAttributes(
            attribute.String("status", order.Status),
        ),
    )
    p.orderValue.Record(ctx, order.Total,
        metric.WithAttributes(
            attribute.String("customer.id", order.CustomerID),
        ),
    )

    return nil
}
```

## Configuration

### Enable OpenTelemetry

Configure OTel in your `config.yaml`:

```yaml
otel:
  service:
    name: "order-processor"
    version: "1.0.0"
  sdk:
    disabled: false  # Enable OTel
  exporter:
    otlp:
      endpoint: "localhost:4317"
      protocol: grpc
      insecure: true
  traces:
    sampler:
      type: "always_on"  # Or "traceidratio" for sampling
  metrics:
    interval: 60s  # Export interval
```

### OTLP Exporter

Export to an OTLP collector (Jaeger, Tempo, etc.):

```yaml
otel:
  exporter:
    otlp:
      endpoint: "otel-collector:4317"
      protocol: grpc
      headers:
        - key: "authorization"
          value: "Bearer {{env \"OTEL_TOKEN\"}}"
```

### Sampling

Configure trace sampling for high-throughput scenarios:

```yaml
otel:
  traces:
    sampler:
      type: "traceidratio"
      arg: 0.1  # Sample 10% of traces
```

## Visualization

### Jaeger UI

View distributed traces in Jaeger:

```
Trace: Process Order (trace-id: abc123)
  │
  ├─ HTTP POST /orders [orders-api] 250ms
  │  └─ kafka.publish [orders-api] 5ms
  │
  └─ kafka.process [order-processor] 45ms
     ├─ deserialize-order 2ms
     ├─ save-order 40ms
     │  └─ sql.insert 38ms
     └─ publish-event 3ms
```

### Grafana Dashboard

Monitor Kafka consumer metrics:

**Key Dashboard Panels:**

1. **Consumer Lag**
   ```promql
   kafka_consumer_lag{group="order-processors",topic="orders"}
   ```

2. **Messages Processed per Second**
   ```promql
   rate(messaging_client_processed_messages_total{topic="orders"}[1m])
   ```

3. **Messages Committed per Second**
   ```promql
   rate(messaging_client_committed_messages_total{topic="orders"}[1m])
   ```

4. **Processing Error Rate**
   ```promql
   rate(messaging_process_failures_total{topic="orders"}[1m])
   ```

5. **Processing Failure Ratio**
   ```promql
   rate(messaging_process_failures_total{topic="orders"}[5m]) 
   / 
   rate(messaging_client_processed_messages_total{topic="orders"}[5m])
   ```

## Debugging

### Find Slow Messages

Use trace queries to find slow processing:

**Jaeger Query:**
```
service="order-processor"
minDuration=1s
```

This finds all messages that took over 1 second to process.

### Identify Error Patterns

Find errors in logs correlated with traces:

**Log Query (Loki):**
```
{service="order-processor"} |= "error" | json | trace_id="abc123"
```

### Monitor Partition Lag

Check lag per partition:

```bash
kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 \
  --group order-processors \
  --describe
```

Correlate with processing traces to find bottlenecks.

## Best Practices

### Always Use Context

Pass context through your call chain for trace propagation:

```go
func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Good: Pass context
    return p.saveOrder(ctx, msg)
}

func (p *OrderProcessor) saveOrder(ctx context.Context, msg kafka.Message) error {
    // Good: Use context in DB calls
    _, err := p.db.ExecContext(ctx, ...)
    return err
}
```

### Add Business Context

Include business IDs in spans and logs:

```go
span.SetAttributes(
    attribute.String("order.id", order.OrderID),
    attribute.String("customer.id", order.CustomerID),
)

log.InfoContext(ctx, "Order processed",
    slog.String("order_id", order.OrderID),
    slog.String("customer_id", order.CustomerID),
)
```

### Sample High-Volume Topics

For high-throughput topics, use sampling:

```yaml
otel:
  traces:
    sampler:
      type: "traceidratio"
      arg: 0.01  # 1% sampling for high volume
```

### Monitor Consumer Lag

Set up alerts for increasing lag:

```yaml
# Prometheus alert
- alert: KafkaConsumerLag
  expr: kafka_consumer_lag{group="order-processors"} > 1000
  for: 5m
  annotations:
    summary: "Consumer group {{ $labels.group }} is lagging"
```

### Use Structured Logging

Always use structured logs (slog), not formatted strings:

```go
// Good: Structured
log.InfoContext(ctx, "Order processed",
    slog.String("order_id", order.OrderID),
    slog.Int64("partition", msg.Partition),
)

// Bad: Unstructured
log.Printf("Order %s processed on partition %d", order.OrderID, msg.Partition)
```

## Example: Complete Observability

```go
package main

import (
    "context"
    "encoding/json"
    "log/slog"

    "github.com/z5labs/humus/queue"
    "github.com/z5labs/humus/queue/kafka"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/metric"
)

var tracer = otel.Tracer("order-processor")

type OrderProcessor struct {
    db              *sql.DB
    ordersProcessed metric.Int64Counter
}

func NewOrderProcessor(db *sql.DB) (*OrderProcessor, error) {
    meter := otel.Meter("order-processor")

    counter, err := meter.Int64Counter("orders.processed")
    if err != nil {
        return nil, err
    }

    return &OrderProcessor{
        db:              db,
        ordersProcessed: counter,
    }, nil
}

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Create custom span
    ctx, span := tracer.Start(ctx, "process-order")
    defer span.End()

    // Deserialize
    var order Order
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        slog.ErrorContext(ctx, "Failed to deserialize",
            kafka.TopicAttr(msg.Topic),
            kafka.OffsetAttr(msg.Offset),
            slog.Any("error", err),
        )
        span.RecordError(err)
        return err
    }

    // Add attributes
    span.SetAttributes(
        attribute.String("order.id", order.OrderID),
        attribute.Float64("order.total", order.Total),
    )

    // Log processing
    slog.InfoContext(ctx, "Processing order",
        kafka.TopicAttr(msg.Topic),
        kafka.PartitionAttr(msg.Partition),
        kafka.OffsetAttr(msg.Offset),
        slog.String("order_id", order.OrderID),
    )

    // Save to database
    if err := p.saveOrder(ctx, order); err != nil {
        slog.ErrorContext(ctx, "Failed to save order",
            kafka.TopicAttr(msg.Topic),
            kafka.OffsetAttr(msg.Offset),
            slog.String("order_id", order.OrderID),
            slog.Any("error", err),
        )
        span.RecordError(err)
        return err
    }

    // Record metrics
    p.ordersProcessed.Add(ctx, 1,
        metric.WithAttributes(
            attribute.String("status", order.Status),
        ),
    )

    slog.InfoContext(ctx, "Order processed successfully",
        slog.String("order_id", order.OrderID),
    )

    return nil
}

func (p *OrderProcessor) saveOrder(ctx context.Context, order Order) error {
    ctx, span := tracer.Start(ctx, "save-order-db")
    defer span.End()

    _, err := p.db.ExecContext(ctx,
        "INSERT INTO orders (order_id, total) VALUES ($1, $2)",
        order.OrderID, order.Total,
    )

    if err != nil {
        span.RecordError(err)
    }

    return err
}
```

## Next Steps

- Configure [Production Settings]({{< ref "configuration" >}}) for deployment
- Review [Quick Start]({{< ref "quick-start" >}}) for complete examples
- Explore [Message Structure]({{< ref "message" >}}) for additional context
