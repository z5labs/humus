---
title: Message Structure
description: Working with Kafka messages and metadata
weight: 30
type: docs
---

# Message Structure

Kafka messages in Humus are represented by the `kafka.Message` type, which provides access to all message data and metadata.

## Message Type

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

## Message Fields

### Value

The message payload as raw bytes:

```go
type Message struct {
    Value []byte  // The message content
}
```

**Usage:**
```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Deserialize JSON
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }

    // Process the order
    return p.processOrder(ctx, order)
}
```

### Key

Optional message key used for partitioning and compaction:

```go
type Message struct {
    Key []byte  // Optional partition key
}
```

**Usage:**
```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    if msg.Key != nil {
        customerID := string(msg.Key)
        // All messages for this customer go to same partition
    }

    // Process message
    return nil
}
```

**Key purposes:**
- Partition assignment (same key → same partition)
- Log compaction (retain latest message per key)
- Message ordering (within partition)

### Headers

Key-value metadata attached to the message:

```go
type Header struct {
    Key   string
    Value []byte
}

type Message struct {
    Headers []Header
}
```

**Usage:**
```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Find specific header
    for _, header := range msg.Headers {
        if header.Key == "trace-id" {
            traceID := string(header.Value)
            // Use for distributed tracing
        }
        if header.Key == "content-type" {
            contentType := string(header.Value)
            // Determine deserialization format
        }
    }

    return nil
}
```

**Common header uses:**
- Distributed tracing IDs
- Content type/encoding
- Source application
- Schema version
- Correlation IDs

### Timestamp

When the message was produced:

```go
type Message struct {
    Timestamp time.Time
}
```

**Usage:**
```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    age := time.Since(msg.Timestamp)
    if age > 5*time.Minute {
        // Handle stale message
        log.Warn("Processing stale message", "age", age)
    }

    return nil
}
```

**Timestamp types:**
- **CreateTime:** When producer sent the message (default)
- **LogAppendTime:** When broker received the message (if configured)

### Topic

The Kafka topic this message came from:

```go
type Message struct {
    Topic string
}
```

**Usage:**
```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    switch msg.Topic {
    case "orders":
        return p.processOrder(ctx, msg)
    case "payments":
        return p.processPayment(ctx, msg)
    default:
        return fmt.Errorf("unknown topic: %s", msg.Topic)
    }
}
```

Useful when processing multiple topics with the same processor.

### Partition

The partition this message came from:

```go
type Message struct {
    Partition int32
}
```

**Usage:**
```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    log.Info("Processing message",
        "topic", msg.Topic,
        "partition", msg.Partition,
        "offset", msg.Offset,
    )

    return nil
}
```

**Partition guarantees:**
- Messages in a partition are ordered
- Each partition processed by one consumer at a time
- Same key always goes to same partition

### Offset

The message's position within its partition:

```go
type Message struct {
    Offset int64
}
```

**Usage:**
```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Save offset for manual checkpoint recovery
    if err := p.processMessage(ctx, msg); err != nil {
        return err
    }

    // Record last processed offset
    p.recordCheckpoint(msg.Topic, msg.Partition, msg.Offset)
    return nil
}
```

**Offset characteristics:**
- Monotonically increasing within partition
- Unique identifier for message position
- Used for offset commits (acknowledgments)

### Attrs

Message attributes (advanced):

```go
type Message struct {
    Attrs uint8
}
```

Bitmap of message flags (compression, transaction markers, etc.). Rarely used directly in application code.

## Deserialization Patterns

### JSON Deserialization

Most common pattern for JSON messages:

```go
type OrderMessage struct {
    OrderID string  `json:"order_id"`
    Amount  float64 `json:"amount"`
}

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return fmt.Errorf("invalid JSON: %w", err)
    }

    return p.processOrder(ctx, order)
}
```

### Protobuf Deserialization

For protobuf-encoded messages:

```go
import "google.golang.org/protobuf/proto"

func (p *OrderProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order orderpb.Order
    if err := proto.Unmarshal(msg.Value, &order); err != nil {
        return fmt.Errorf("invalid protobuf: %w", err)
    }

    return p.processOrder(ctx, &order)
}
```

### Avro Deserialization

With schema registry:

```go
import "github.com/linkedin/goavro/v2"

type AvroProcessor struct {
    codec *goavro.Codec
}

func (p *AvroProcessor) Process(ctx context.Context, msg kafka.Message) error {
    native, _, err := p.codec.NativeFromBinary(msg.Value)
    if err != nil {
        return fmt.Errorf("invalid avro: %w", err)
    }

    record := native.(map[string]interface{})
    return p.processRecord(ctx, record)
}
```

### Content-Type Routing

Use headers to determine deserialization format:

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    contentType := p.getHeader(msg, "content-type")

    switch contentType {
    case "application/json":
        return p.processJSON(ctx, msg.Value)
    case "application/protobuf":
        return p.processProtobuf(ctx, msg.Value)
    case "application/avro":
        return p.processAvro(ctx, msg.Value)
    default:
        return fmt.Errorf("unsupported content-type: %s", contentType)
    }
}

func (p *Processor) getHeader(msg kafka.Message, key string) string {
    for _, h := range msg.Headers {
        if h.Key == key {
            return string(h.Value)
        }
    }
    return ""
}
```

## Working with Headers

### Reading Headers

Helper function for header access:

```go
func getHeader(msg kafka.Message, key string) (string, bool) {
    for _, h := range msg.Headers {
        if h.Key == key {
            return string(h.Value), true
        }
    }
    return "", false
}

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    if traceID, ok := getHeader(msg, "trace-id"); ok {
        // Use trace ID
        ctx = context.WithValue(ctx, "trace-id", traceID)
    }

    return nil
}
```

### Header-Based Filtering

Skip messages based on headers:

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Skip test messages
    if env, ok := getHeader(msg, "environment"); ok && env == "test" {
        return nil
    }

    // Skip old schema versions
    if version, ok := getHeader(msg, "schema-version"); ok && version != "v2" {
        log.Warn("Skipping old schema version", "version", version)
        return nil
    }

    return p.processMessage(ctx, msg)
}
```

### Trace Context Propagation

Extract distributed tracing context from headers:

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/propagation"
)

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Extract trace context from headers
    carrier := &headerCarrier{headers: msg.Headers}
    ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)

    // Now ctx contains the parent span context
    _, span := tracer.Start(ctx, "process-order")
    defer span.End()

    return p.processOrder(ctx, msg)
}

type headerCarrier struct {
    headers []kafka.Header
}

func (c *headerCarrier) Get(key string) string {
    for _, h := range c.headers {
        if h.Key == key {
            return string(h.Value)
        }
    }
    return ""
}

func (c *headerCarrier) Set(key, value string) {
    // Not needed for extraction
}

func (c *headerCarrier) Keys() []string {
    keys := make([]string, len(c.headers))
    for i, h := range c.headers {
        keys[i] = h.Key
    }
    return keys
}
```

Note: The Kafka runtime automatically handles OTel trace propagation, so this is usually not needed.

## Error Handling

### Validation Errors

Validate messages and decide how to handle invalid data:

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        // Log and skip invalid JSON
        log.Error("Invalid JSON message",
            "topic", msg.Topic,
            "partition", msg.Partition,
            "offset", msg.Offset,
            "error", err,
        )
        return nil // Skip message, don't retry
    }

    // Validate business rules
    if order.Amount < 0 {
        log.Error("Invalid order amount",
            "order_id", order.OrderID,
            "amount", order.Amount,
        )
        return nil // Skip invalid message
    }

    return p.processOrder(ctx, order)
}
```

### Dead Letter Queue

Route failed messages to a DLQ:

```go
type Processor struct {
    producer *kgo.Client  // For DLQ publishing
}

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        // Send to DLQ
        return p.sendToDLQ(ctx, msg, err)
    }

    if err := p.processOrder(ctx, order); err != nil {
        // Send to DLQ on processing error
        return p.sendToDLQ(ctx, msg, err)
    }

    return nil
}

func (p *Processor) sendToDLQ(ctx context.Context, msg kafka.Message, err error) error {
    dlqRecord := &kgo.Record{
        Topic: msg.Topic + ".dlq",
        Key:   msg.Key,
        Value: msg.Value,
        Headers: append(msg.Headers, kafka.Header{
            Key:   "error",
            Value: []byte(err.Error()),
        }),
    }

    p.producer.Produce(ctx, dlqRecord, nil)
    return nil // Don't return error, message handled
}
```

## Message Logging

Log messages for debugging:

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    log.Info("Processing message",
        "topic", msg.Topic,
        "partition", msg.Partition,
        "offset", msg.Offset,
        "key", string(msg.Key),
        "timestamp", msg.Timestamp,
        "headers", len(msg.Headers),
    )

    // Don't log msg.Value in production (may contain PII)
    // Instead log a hash or truncated version
    valueHash := fmt.Sprintf("%x", sha256.Sum256(msg.Value))
    log.Debug("Message content hash", "hash", valueHash)

    return p.processMessage(ctx, msg)
}
```

## Next Steps

- Learn about [Concurrency Model]({{< ref "concurrency" >}}) for partition processing
- Implement [Idempotency]({{< ref "idempotency" >}}) patterns
- Explore [Observability]({{< ref "observability" >}}) for message tracing
