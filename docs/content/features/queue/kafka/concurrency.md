---
title: Concurrency Model
description: Understanding goroutine-per-partition processing
weight: 40
type: docs
---

# Concurrency Model

The Kafka runtime uses a **goroutine-per-partition** architecture that provides automatic parallelism and partition isolation.

## Architecture Overview

Each Kafka partition is processed in its own goroutine:

```
Topic "orders" (3 partitions):
  ┌─────────────┐
  │ Partition 0 │──────> Goroutine 1 ──> Processor
  ├─────────────┤
  │ Partition 1 │──────> Goroutine 2 ──> Processor
  ├─────────────┤
  │ Partition 2 │──────> Goroutine 3 ──> Processor
  └─────────────┘
```

**Key characteristics:**
- One goroutine per assigned partition
- Partitions processed independently
- No locks needed between partitions
- Scales with partition count

## Consumer Group Coordination

The runtime coordinates with other consumers in the group:

```
Consumer Group "order-processors" with 3 instances:

Instance 1                Instance 2                Instance 3
┌──────────────┐         ┌──────────────┐         ┌──────────────┐
│ Partition 0  │         │ Partition 1  │         │ Partition 2  │
│ Partition 3  │         │ Partition 4  │         │ Partition 5  │
└──────────────┘         └──────────────┘         └──────────────┘
```

**Guarantees:**
- Each partition assigned to exactly one consumer
- Automatic rebalancing when consumers join/leave
- Partition reassignment coordinates gracefully

## Partition Lifecycle

### Assignment

When a partition is assigned to this consumer:

1. **Receive assignment** from consumer group coordinator
2. **Spawn goroutine** for the partition
3. **Start processing** messages from the partition
4. **Continue until** partition is revoked or app shuts down

**Code flow:**
```go
// Internal runtime behavior (conceptual)
func (r *Runtime) onPartitionAssigned(topic string, partition int32) {
    ctx, cancel := context.WithCancel(r.ctx)

    // Spawn goroutine for this partition
    go r.processPartition(ctx, topic, partition)

    // Store cancel function for revocation
    r.partitionCancels[topicPartition{topic, partition}] = cancel
}
```

### Revocation

When a partition is revoked (rebalance):

1. **Receive revocation** signal from coordinator
2. **Cancel context** for partition's goroutine
3. **Wait for goroutine** to finish current message
4. **Complete gracefully** before rebalance proceeds

**Code flow:**
```go
// Internal runtime behavior (conceptual)
func (r *Runtime) onPartitionRevoked(topic string, partition int32) {
    // Cancel the goroutine's context
    cancel := r.partitionCancels[topicPartition{topic, partition}]
    cancel()

    // Wait for goroutine to finish
    r.wait.Wait()

    // Partition now free for reassignment
}
```

## Concurrency Guarantees

### Within a Partition

Messages in a partition are processed **serially** (one at a time):

```go
// For Partition 0:
Message 1 ──> Process ──> Complete
Message 2 ──> Process ──> Complete
Message 3 ──> Process ──> Complete
```

**Guarantees:**
- Messages processed in order
- No concurrent processing within partition
- Simple reasoning about state

### Across Partitions

Messages in different partitions are processed **concurrently**:

```go
// Concurrent processing:
Partition 0: Message 1 ──> Process ──┐
Partition 1: Message 1 ──> Process ──┼──> All concurrent
Partition 2: Message 1 ──> Process ──┘
```

**Implications:**
- No ordering guarantees across partitions
- Shared state needs synchronization
- Independent failure isolation

## Scaling Patterns

### Vertical Scaling (More Partitions)

Increase partition count to enable more parallelism:

```bash
# Create topic with 12 partitions
kafka-topics.sh --create \
  --topic orders \
  --partitions 12 \
  --bootstrap-server localhost:9092
```

One consumer instance can process 12 partitions concurrently:

```
Consumer Instance 1 (12 goroutines):
  Partition 0  ──> Goroutine 1
  Partition 1  ──> Goroutine 2
  ...
  Partition 11 ──> Goroutine 12
```

**Limits:**
- Maximum parallelism = number of partitions
- Cannot exceed partition count with consumers

### Horizontal Scaling (More Consumers)

Add consumer instances to distribute partitions:

```
3 consumers, 12 partitions (4 partitions each):

Consumer 1:          Consumer 2:          Consumer 3:
  Partition 0          Partition 4          Partition 8
  Partition 1          Partition 5          Partition 9
  Partition 2          Partition 6          Partition 10
  Partition 3          Partition 7          Partition 11
```

**Recommendations:**
- Start with partitions = 2-4× expected consumers
- Allows room for scaling
- Example: 12 partitions supports 1-12 consumers

## Shared State Patterns

When processors need shared state:

### Thread-Safe Data Structures

Use concurrent-safe types:

```go
import "sync"

type Processor struct {
    mu    sync.RWMutex
    cache map[string]string
}

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Read from cache
    p.mu.RLock()
    value := p.cache[string(msg.Key)]
    p.mu.RUnlock()

    // Process message
    result := p.compute(value)

    // Write to cache
    p.mu.Lock()
    p.cache[string(msg.Key)] = result
    p.mu.Unlock()

    return nil
}
```

### Partition-Local State

Maintain separate state per partition:

```go
type Processor struct {
    // Map from partition to its state
    partitionState map[int32]*PartitionState
    mu             sync.RWMutex
}

type PartitionState struct {
    // No locks needed - only accessed by one goroutine
    counter int
    lastSeen time.Time
}

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Get state for this partition (no lock needed within partition)
    p.mu.RLock()
    state := p.partitionState[msg.Partition]
    p.mu.RUnlock()

    // Update state without locks
    state.counter++
    state.lastSeen = time.Now()

    return nil
}
```

### Atomic Operations

Use atomic types for counters:

```go
import "sync/atomic"

type Processor struct {
    messagesProcessed atomic.Int64
}

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Atomic increment - safe across goroutines
    p.messagesProcessed.Add(1)

    return nil
}
```

## Rebalancing

### Triggering Rebalances

Rebalances occur when:

1. **Consumer joins** the group (new instance starts)
2. **Consumer leaves** the group (instance stops/crashes)
3. **Topic partitions** are added
4. **Session timeout** expires (consumer considered dead)

### Rebalance Process

```
1. Coordinator initiates rebalance
   ↓
2. All consumers stop fetching
   ↓
3. Current partitions revoked
   ├─> Cancel partition goroutines
   └─> Wait for completion
   ↓
4. New partitions assigned
   ├─> Spawn new goroutines
   └─> Resume processing
   ↓
5. Normal processing resumes
```

### Minimizing Rebalance Impact

**Fast Rebalancing:**
```go
// Use shorter timeouts for faster rebalancing
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", processor),
    kafka.SessionTimeout(10 * time.Second),
    kafka.RebalanceTimeout(20 * time.Second),
)
```

**Graceful Shutdown:**
```go
// Ensure clean shutdown to avoid forced rebalancing
// The framework handles this automatically via context cancellation
```

## Performance Considerations

### Partition Count

**Too few partitions:**
- Limited parallelism
- One slow processor blocks others
- Difficult to scale horizontally

**Too many partitions:**
- Higher overhead per partition
- More goroutines
- More memory usage

**Recommendation:**
- Start with 2-4× expected consumer count
- Example: 3 consumers → 12 partitions
- Allows scaling to 12 consumers without repartitioning

### Message Distribution

**Even distribution across partitions:**
```go
// Producer uses key to distribute evenly
key := fmt.Sprintf("customer-%d", customerID % numPartitions)
```

**Avoid hot partitions:**
- Don't route all traffic to one partition
- Use random or round-robin for keyless messages
- Monitor partition lag for imbalance

### Processing Time

**Fast processors:**
- Can handle more partitions per consumer
- Higher throughput
- Lower latency

**Slow processors:**
- Limit partitions per consumer
- Scale horizontally
- Consider async I/O

## Monitoring Concurrency

### Consumer Lag per Partition

```bash
kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 \
  --group my-group \
  --describe
```

Output shows lag per partition:
```
TOPIC    PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG
orders   0          1000            1000            0
orders   1          950             1020            70    # Lagging!
orders   2          1000            1000            0
```

**Hot partition detected:** Partition 1 is lagging.

### Goroutine Count

Monitor active goroutines:

```go
import "runtime"

func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Periodically log goroutine count
    if msg.Offset % 1000 == 0 {
        log.Info("Runtime stats",
            "goroutines", runtime.NumGoroutine(),
            "partition", msg.Partition,
        )
    }

    return nil
}
```

## Error Handling

### Partition-Level Errors

Errors in one partition don't affect others:

```
Partition 0: Processing ✓
Partition 1: Error!      ← Partition 1 stops
Partition 2: Processing ✓ ← Continues normally
```

**At-least-once behavior:**
- Failed partition stops processing
- Offset not committed
- Messages will be redelivered after rebalance

**At-most-once behavior:**
- Failed partition stops processing
- Offset already committed
- Messages lost

### Context Cancellation

All partition goroutines respect context cancellation:

```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    // Check context before expensive operation
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }

    // Process message
    return p.doWork(ctx, msg)
}
```

This ensures clean shutdown and rebalancing.

## Next Steps

- Learn [Idempotency]({{< ref "idempotency" >}}) patterns for at-least-once processing
- Explore [Multi-Topic Processing]({{< ref "multi-topic" >}}) patterns
- Configure [Production Settings]({{< ref "configuration" >}}) for optimal performance
