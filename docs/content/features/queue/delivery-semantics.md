---
title: Delivery Semantics
description: Understanding at-most-once and at-least-once processing
weight: 20
type: docs
---


Queue processing services must choose between two fundamental delivery guarantees: **at-most-once** and **at-least-once**. This choice affects how your application handles failures and determines the reliability guarantees you can provide.

## Quick Comparison

| Aspect | At-Most-Once | At-Least-Once |
|--------|--------------|---------------|
| **Processing Order** | Consume → Acknowledge → Process | Consume → Process → Acknowledge |
| **On Failure** | Message lost | Message retried |
| **Duplicates** | Never | Possible |
| **Throughput** | Higher | Lower |
| **Processor Requirements** | None | Must be idempotent |
| **Use Cases** | Metrics, logs, caching | Transactions, database updates |

## At-Most-Once Processing

At-most-once processing acknowledges messages **before** processing them. This provides fast throughput but means messages can be lost if processing fails.

### Processing Flow

```
1. Consume message from queue
2. Acknowledge message (commit offset)
3. Process message
```

If step 3 fails, the message has already been acknowledged and is permanently lost.

### When to Use

At-most-once is appropriate when:

- **Performance is critical** - Lower latency and higher throughput
- **Data loss is acceptable** - Occasional message loss won't impact your application
- **Messages are non-critical** - Informational data that can be recreated or ignored

### Common Use Cases

**Metrics Collection:**
```go
type MetricsProcessor struct {
    client *prometheus.Client
}

func (p *MetricsProcessor) Process(ctx context.Context, msg *MetricMessage) error {
    // Send metric to monitoring system
    // If this fails, we can tolerate losing a few data points
    return p.client.RecordMetric(ctx, msg.Name, msg.Value)
}
```

**Log Aggregation:**
```go
type LogProcessor struct {
    writer *LogWriter
}

func (p *LogProcessor) Process(ctx context.Context, msg *LogMessage) error {
    // Write log to aggregation system
    // Missing a few log entries is acceptable
    return p.writer.Write(ctx, msg.Level, msg.Message)
}
```

**Cache Updates:**
```go
type CacheProcessor struct {
    cache *redis.Client
}

func (p *CacheProcessor) Process(ctx context.Context, msg *CacheUpdate) error {
    // Update cache entry
    // Cache can be rebuilt if some updates are lost
    return p.cache.Set(ctx, msg.Key, msg.Value, msg.TTL)
}
```

### Advantages

- **Higher throughput** - No waiting for processing to complete before acknowledging
- **Lower latency** - Messages acknowledged immediately
- **Simpler implementation** - No idempotency requirements
- **Faster recovery** - Failures don't block message consumption

### Disadvantages

- **Data loss** - Processing failures result in lost messages
- **No retry logic** - Failed messages are not retried
- **Weaker guarantees** - Cannot ensure all messages are processed

### Implementation

```go
processor := queue.ProcessAtMostOnce(consumer, processor, acknowledger)

for {
    err := processor.ProcessItem(ctx)
    if errors.Is(err, queue.ErrEndOfQueue) {
        return nil // Graceful shutdown
    }
    // Continue processing even on errors
    // Message already acknowledged and lost
}
```

## At-Least-Once Processing

At-least-once processing acknowledges messages **after** successful processing. This provides reliable delivery but means messages may be processed multiple times.

### Processing Flow

```
1. Consume message from queue
2. Process message
3. Acknowledge message (commit offset)
```

If step 2 fails, the message is not acknowledged and will be redelivered for retry.

### When to Use

At-least-once is appropriate when:

- **Reliability is critical** - Every message must be processed successfully
- **Data loss is unacceptable** - Missing messages would corrupt data or business logic
- **Idempotency is achievable** - Your processor can handle duplicate messages safely

### Common Use Cases

**Financial Transactions:**
```go
type PaymentProcessor struct {
    db *sql.DB
}

func (p *PaymentProcessor) Process(ctx context.Context, msg *Payment) error {
    // Check if already processed (idempotency)
    var exists bool
    err := p.db.QueryRowContext(ctx,
        "SELECT EXISTS(SELECT 1 FROM payments WHERE transaction_id = $1)",
        msg.TransactionID,
    ).Scan(&exists)
    if err != nil {
        return err
    }
    if exists {
        return nil // Already processed, skip
    }

    // Process payment
    _, err = p.db.ExecContext(ctx,
        "INSERT INTO payments (transaction_id, amount, status) VALUES ($1, $2, 'completed')",
        msg.TransactionID, msg.Amount,
    )
    return err
}
```

**Database Updates:**
```go
type OrderProcessor struct {
    db *sql.DB
}

func (p *OrderProcessor) Process(ctx context.Context, msg *Order) error {
    // Upsert: idempotent database operation
    _, err := p.db.ExecContext(ctx,
        `INSERT INTO orders (order_id, customer_id, total)
         VALUES ($1, $2, $3)
         ON CONFLICT (order_id) DO UPDATE SET
         customer_id = EXCLUDED.customer_id,
         total = EXCLUDED.total`,
        msg.OrderID, msg.CustomerID, msg.Total,
    )
    return err
}
```

**Event Sourcing:**
```go
type EventProcessor struct {
    store EventStore
}

func (p *EventProcessor) Process(ctx context.Context, msg *Event) error {
    // Event store handles deduplication
    return p.store.Append(ctx, msg.StreamID, msg)
}
```

### Advantages

- **Reliable delivery** - All messages are processed successfully
- **Automatic retry** - Failed messages are retried automatically
- **Stronger guarantees** - Can ensure critical operations complete
- **Data integrity** - No messages lost or skipped

### Disadvantages

- **Lower throughput** - Must wait for processing to complete before acknowledging
- **Higher latency** - Acknowledgment delayed until processing succeeds
- **Duplicate processing** - Messages may be processed multiple times
- **Idempotency required** - Processors must handle duplicates correctly

### Implementation

```go
processor := queue.ProcessAtLeastOnce(consumer, processor, acknowledger)

for {
    err := processor.ProcessItem(ctx)
    if errors.Is(err, queue.ErrEndOfQueue) {
        return nil // Graceful shutdown
    }
    if err != nil {
        return err // Stop processing on error
    }
}
```

## Choosing the Right Semantic

Use this decision tree to choose the appropriate semantic:

```
Can your application tolerate message loss?
├─ Yes → Is performance critical?
│         ├─ Yes → At-Most-Once
│         └─ No  → Either (prefer At-Most-Once for simplicity)
└─ No  → Can you implement idempotent processing?
          ├─ Yes → At-Least-Once
          └─ No  → Redesign to support idempotency or accept data loss
```

### Questions to Ask

1. **What happens if a message is lost?**
   - Critical failure → At-Least-Once
   - Acceptable loss → At-Most-Once

2. **Can your processor handle duplicate messages?**
   - Yes (idempotent) → At-Least-Once is safe
   - No → At-Most-Once or redesign

3. **What are your performance requirements?**
   - High throughput needed → At-Most-Once
   - Reliability more important → At-Least-Once

4. **What is the cost of duplicate processing?**
   - Low (read-only, idempotent) → At-Least-Once is safe
   - High (side effects, non-idempotent) → At-Most-Once or redesign

## Idempotency Strategies

At-least-once processing requires idempotent processors. Common strategies:

### Unique ID Tracking

Store processed message IDs in a database:

```go
func (p *Processor) Process(ctx context.Context, msg *Message) error {
    // Check if already processed
    var exists bool
    err := p.db.QueryRowContext(ctx,
        "SELECT EXISTS(SELECT 1 FROM processed_messages WHERE message_id = $1)",
        msg.ID,
    ).Scan(&exists)
    if err != nil {
        return err
    }
    if exists {
        return nil
    }

    // Process and record in same transaction
    tx, err := p.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    // Do work
    if err := p.doWork(ctx, tx, msg); err != nil {
        return err
    }

    // Record processed
    _, err = tx.ExecContext(ctx,
        "INSERT INTO processed_messages (message_id) VALUES ($1)",
        msg.ID,
    )
    if err != nil {
        return err
    }

    return tx.Commit()
}
```

### Natural Idempotency

Design operations to be naturally idempotent:

```go
// Idempotent: Setting a value
UPDATE users SET email = 'new@example.com' WHERE id = 123

// NOT idempotent: Incrementing a value
UPDATE accounts SET balance = balance + 100 WHERE id = 456
```

### Upsert Operations

Use database upserts for idempotent writes:

```go
_, err := db.ExecContext(ctx,
    `INSERT INTO orders (order_id, total)
     VALUES ($1, $2)
     ON CONFLICT (order_id) DO UPDATE SET total = EXCLUDED.total`,
    msg.OrderID, msg.Total,
)
```

See [Kafka Idempotency]({{< ref "kafka/idempotency" >}}) for Kafka-specific patterns.

## Mixed Semantics

Some applications may need different semantics for different message types:

```go
func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    // Critical orders: at-least-once
    ordersRuntime, err := kafka.NewAtLeastOnceRuntime(
        cfg.Kafka.Brokers,
        "orders",
        cfg.Kafka.GroupID,
        ordersProcessor,
        decodeOrder,
    )
    if err != nil {
        return nil, err
    }

    // Non-critical metrics: at-most-once
    metricsRuntime, err := kafka.NewAtMostOnceRuntime(
        cfg.Kafka.Brokers,
        "metrics",
        cfg.Kafka.GroupID,
        metricsProcessor,
        decodeMetric,
    )
    if err != nil {
        return nil, err
    }

    // Combine runtimes (implementation-specific)
    runtime := newMultiRuntime(ordersRuntime, metricsRuntime)
    return queue.NewApp(runtime), nil
}
```

## Next Steps

- Implement idempotent processors with [Kafka Idempotency]({{< ref "kafka/idempotency" >}})
- Learn about Kafka-specific features in [Kafka Runtime]({{< ref "kafka/runtime" >}})
- Build your first processor with [Kafka Quick Start]({{< ref "kafka/quick-start" >}})
