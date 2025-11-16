---
title: Idempotency
description: Handling duplicate messages in at-least-once processing
weight: 50
type: docs
---


At-least-once processing guarantees message delivery but may deliver duplicates. Your processor must be **idempotent** - processing the same message multiple times produces the same result.

## Why Idempotency Matters

### At-Least-Once Delivers Duplicates

Common scenarios that cause duplicates:

1. **Processing completes but offset commit fails**
   ```
   Message 100 → Process ✓ → Commit ✗
   [Rebalance or restart]
   Message 100 → Process ✓ → Commit ✓  (duplicate!)
   ```

2. **Consumer crashes after processing**
   ```
   Message 100 → Process ✓ → [Crash before commit]
   [Restart]
   Message 100 → Process ✓ → Commit ✓  (duplicate!)
   ```

3. **Network partition during commit**
   ```
   Message 100 → Process ✓ → Commit [timeout]
   [Retry]
   Message 100 → Process ✓ → Commit ✓  (duplicate!)
   ```

### Without Idempotency

Non-idempotent processors corrupt data:

```go
// NON-IDEMPOTENT: Incrementing a counter
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // First processing: balance = 100 + 50 = 150
    // Duplicate:       balance = 150 + 50 = 200  ← Wrong!
    _, err := p.db.Exec(
        "UPDATE accounts SET balance = balance + $1 WHERE id = $2",
        msg.Amount, msg.AccountID,
    )
    return err
}
```

## Idempotency Patterns

### Pattern 1: Unique ID Tracking

Store processed message IDs in a table:

```go
type Processor struct {
    db *sql.DB
}

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var order Order
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }

    // Check if already processed
    var exists bool
    err := p.db.QueryRowContext(ctx,
        "SELECT EXISTS(SELECT 1 FROM processed_orders WHERE order_id = $1)",
        order.OrderID,
    ).Scan(&exists)
    if err != nil {
        return err
    }

    if exists {
        // Already processed, skip
        return nil
    }

    // Process in transaction
    tx, err := p.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    // Insert order
    _, err = tx.ExecContext(ctx,
        "INSERT INTO orders (order_id, customer_id, total) VALUES ($1, $2, $3)",
        order.OrderID, order.CustomerID, order.Total,
    )
    if err != nil {
        return err
    }

    // Record as processed
    _, err = tx.ExecContext(ctx,
        "INSERT INTO processed_orders (order_id, processed_at) VALUES ($1, NOW())",
        order.OrderID,
    )
    if err != nil {
        return err
    }

    return tx.Commit()
}
```

**Schema:**
```sql
CREATE TABLE processed_orders (
    order_id VARCHAR(255) PRIMARY KEY,
    processed_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_processed_at ON processed_orders(processed_at);
```

**Cleanup old entries:**
```sql
DELETE FROM processed_orders
WHERE processed_at < NOW() - INTERVAL '7 days';
```

### Pattern 2: Upsert Operations

Use database upserts for natural idempotency:

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var order Order
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }

    // Upsert: idempotent operation
    _, err := p.db.ExecContext(ctx,
        `INSERT INTO orders (order_id, customer_id, total, updated_at)
         VALUES ($1, $2, $3, NOW())
         ON CONFLICT (order_id) DO UPDATE SET
           customer_id = EXCLUDED.customer_id,
           total = EXCLUDED.total,
           updated_at = NOW()`,
        order.OrderID, order.CustomerID, order.Total,
    )
    return err
}
```

**Result:** Processing the same message multiple times produces the same database state.

### Pattern 3: SET Operations

Use idempotent SET operations instead of increments:

```go
// IDEMPOTENT: Setting absolute values
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var update AccountUpdate
    if err := json.Unmarshal(msg.Value, &update); err != nil {
        return err
    }

    // Set absolute value (idempotent)
    _, err := p.db.ExecContext(ctx,
        "UPDATE accounts SET balance = $1, updated_at = $2 WHERE id = $3",
        update.NewBalance,  // Absolute value
        update.Timestamp,   // Version/timestamp
        update.AccountID,
    )
    return err
}
```

### Pattern 4: Unique Constraints

Let the database enforce uniqueness:

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var payment Payment
    if err := json.Unmarshal(msg.Value, &payment); err != nil {
        return err
    }

    // Insert with unique constraint
    _, err := p.db.ExecContext(ctx,
        "INSERT INTO payments (transaction_id, amount, status) VALUES ($1, $2, 'completed')",
        payment.TransactionID, payment.Amount,
    )

    // Handle duplicate key error
    if isDuplicateKeyError(err) {
        // Already processed, not an error
        return nil
    }

    return err
}

func isDuplicateKeyError(err error) bool {
    if err == nil {
        return false
    }
    // PostgreSQL duplicate key error code
    return strings.Contains(err.Error(), "duplicate key value")
}
```

**Schema:**
```sql
CREATE TABLE payments (
    transaction_id VARCHAR(255) PRIMARY KEY,
    amount DECIMAL(10,2) NOT NULL,
    status VARCHAR(50) NOT NULL,
    created_at TIMESTAMP DEFAULT NOW()
);
```

### Pattern 5: Offset as Idempotency Key

Use Kafka's natural ordering:

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var event Event
    if err := json.Unmarshal(msg.Value, &event); err != nil {
        return err
    }

    // Composite key: topic + partition + offset
    idempotencyKey := fmt.Sprintf("%s-%d-%d", msg.Topic, msg.Partition, msg.Offset)

    var exists bool
    err := p.db.QueryRowContext(ctx,
        "SELECT EXISTS(SELECT 1 FROM processed_events WHERE idempotency_key = $1)",
        idempotencyKey,
    ).Scan(&exists)
    if err != nil {
        return err
    }

    if exists {
        return nil
    }

    // Process and record
    tx, err := p.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    // Process event
    if err := p.processEvent(ctx, tx, event); err != nil {
        return err
    }

    // Record offset
    _, err = tx.ExecContext(ctx,
        "INSERT INTO processed_events (idempotency_key) VALUES ($1)",
        idempotencyKey,
    )
    if err != nil {
        return err
    }

    return tx.Commit()
}
```

## Message-Level Idempotency

### Producer-Assigned IDs

Ensure messages have unique IDs from the producer:

```go
// Producer side
type OrderMessage struct {
    OrderID    string `json:"order_id"`     // Unique ID
    CustomerID string `json:"customer_id"`
    Amount     float64 `json:"amount"`
}

// Consumer side
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var order OrderMessage
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }

    // Use order.OrderID as idempotency key
    return p.processOrder(ctx, order.OrderID, order)
}
```

### UUID Generation

Generate UUIDs at the producer:

```go
import "github.com/google/uuid"

// Producer
order := OrderMessage{
    OrderID:    uuid.New().String(),  // Globally unique
    CustomerID: "cust-123",
    Amount:     99.99,
}
```

## Advanced Patterns

### Timestamp-Based Deduplication

Accept only newer messages:

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var update Update
    if err := json.Unmarshal(msg.Value, &update); err != nil {
        return err
    }

    // Only apply if newer than current
    result, err := p.db.ExecContext(ctx,
        `UPDATE entities SET
           value = $1,
           updated_at = $2
         WHERE id = $3 AND (updated_at IS NULL OR updated_at < $2)`,
        update.Value,
        update.Timestamp,  // Must be set by producer
        update.EntityID,
    )
    if err != nil {
        return err
    }

    rows, _ := result.RowsAffected()
    if rows == 0 {
        // Stale update, skip (already have newer data)
        return nil
    }

    return nil
}
```

### Event Sourcing

Natural idempotency through event deduplication:

```go
type EventStore interface {
    Append(ctx context.Context, streamID string, event Event) error
}

type Processor struct {
    eventStore EventStore
}

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var event Event
    if err := json.Unmarshal(msg.Value, &event); err != nil {
        return err
    }

    // Event store handles deduplication by event ID
    return p.eventStore.Append(ctx, event.StreamID, event)
}
```

### Distributed Locks

Use distributed locks for complex operations:

```go
import "github.com/go-redsync/redsync/v4"

type Processor struct {
    rs *redsync.Redsync
    db *sql.DB
}

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    var order Order
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }

    // Acquire distributed lock
    mutex := p.rs.NewMutex(
        fmt.Sprintf("order-lock-%s", order.OrderID),
        redsync.WithExpiry(10*time.Second),
    )

    if err := mutex.LockContext(ctx); err != nil {
        return err
    }
    defer mutex.UnlockContext(ctx)

    // Check if processed
    var exists bool
    err := p.db.QueryRowContext(ctx,
        "SELECT EXISTS(SELECT 1 FROM orders WHERE order_id = $1)",
        order.OrderID,
    ).Scan(&exists)
    if err != nil {
        return err
    }

    if exists {
        return nil
    }

    // Process order
    return p.processOrder(ctx, order)
}
```

## Testing Idempotency

### Duplicate Message Test

```go
func TestProcessor_Idempotency(t *testing.T) {
    db := setupTestDB(t)
    processor := &Processor{db: db}

    message := kafka.Message{
        Value: []byte(`{"order_id":"ord-123","customer_id":"cust-456","total":99.99}`),
    }

    // Process first time
    err := processor.Process(context.Background(), message)
    require.NoError(t, err)

    // Verify inserted
    var count int
    db.QueryRow("SELECT COUNT(*) FROM orders WHERE order_id = 'ord-123'").Scan(&count)
    assert.Equal(t, 1, count)

    // Process duplicate
    err = processor.Process(context.Background(), message)
    require.NoError(t, err)

    // Verify still only one record
    db.QueryRow("SELECT COUNT(*) FROM orders WHERE order_id = 'ord-123'").Scan(&count)
    assert.Equal(t, 1, count, "Duplicate message should not create new record")
}
```

### Concurrent Duplicate Test

```go
func TestProcessor_ConcurrentDuplicates(t *testing.T) {
    db := setupTestDB(t)
    processor := &Processor{db: db}

    message := kafka.Message{
        Value: []byte(`{"order_id":"ord-123","customer_id":"cust-456","total":99.99}`),
    }

    // Process same message concurrently
    var wg sync.WaitGroup
    errors := make([]error, 10)

    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(idx int) {
            defer wg.Done()
            errors[idx] = processor.Process(context.Background(), message)
        }(i)
    }

    wg.Wait()

    // All should succeed (or fail with duplicate key)
    for _, err := range errors {
        if err != nil {
            assert.True(t, isDuplicateKeyError(err), "Unexpected error: %v", err)
        }
    }

    // Verify exactly one record
    var count int
    db.QueryRow("SELECT COUNT(*) FROM orders WHERE order_id = 'ord-123'").Scan(&count)
    assert.Equal(t, 1, count, "Concurrent processing should create exactly one record")
}
```

## Performance Considerations

### Index Idempotency Keys

```sql
CREATE INDEX idx_processed_orders_id ON processed_orders(order_id);
CREATE INDEX idx_processed_events_key ON processed_events(idempotency_key);
```

### Cleanup Old Records

Prevent unbounded growth:

```go
// Run cleanup periodically
func (p *Processor) cleanupOldRecords(ctx context.Context) error {
    _, err := p.db.ExecContext(ctx,
        "DELETE FROM processed_orders WHERE processed_at < NOW() - INTERVAL '7 days'",
    )
    return err
}
```

### Batch Lookups

For high throughput, batch idempotency checks:

```go
func (p *Processor) ProcessBatch(ctx context.Context, messages []kafka.Message) error {
    // Extract all order IDs
    orderIDs := make([]string, len(messages))
    for i, msg := range messages {
        var order Order
        json.Unmarshal(msg.Value, &order)
        orderIDs[i] = order.OrderID
    }

    // Batch lookup
    rows, err := p.db.QueryContext(ctx,
        "SELECT order_id FROM processed_orders WHERE order_id = ANY($1)",
        pq.Array(orderIDs),
    )
    if err != nil {
        return err
    }
    defer rows.Close()

    processed := make(map[string]bool)
    for rows.Next() {
        var id string
        rows.Scan(&id)
        processed[id] = true
    }

    // Process only unprocessed messages
    for i, msg := range messages {
        if !processed[orderIDs[i]] {
            if err := p.processSingle(ctx, msg); err != nil {
                return err
            }
        }
    }

    return nil
}
```

## Next Steps

- Explore [Multi-Topic Processing]({{< ref "multi-topic" >}}) patterns
- Learn about [Observability]({{< ref "observability" >}}) for message tracing
- Configure [Production Settings]({{< ref "configuration" >}}) for deployment
