---
title: Queue Framework
description: Core abstractions and patterns for queue processing
weight: 10
type: docs
---


The Humus queue framework implements a three-phase message processing pattern that separates concerns for consuming, processing, and acknowledging messages from a queue.

## Core Interfaces

The queue framework defines four core interfaces that work together to process messages:

### Consumer

Retrieves messages from a queue:

```go
type Consumer[T any] interface {
    Consume(ctx context.Context) (T, error)
}
```

**Responsibilities:**
- Fetch the next message from the queue
- Handle connection management and retries
- Return `ErrEndOfQueue` when the queue is exhausted (for graceful shutdown)

**Example:**
```go
type KafkaConsumer struct {
    client *kgo.Client
}

func (c *KafkaConsumer) Consume(ctx context.Context) (*Message, error) {
    fetches := c.client.PollFetches(ctx)
    if fetches.Empty() {
        return nil, queue.ErrEndOfQueue
    }
    // Return first message
    return &Message{...}, nil
}
```

### Processor

Executes business logic on messages:

```go
type Processor[T any] interface {
    Process(ctx context.Context, msg T) error
}
```

**Responsibilities:**
- Implement business logic for message handling
- Be idempotent (for at-least-once processing)
- Return errors to trigger acknowledgment logic

**Example:**
```go
type OrderProcessor struct {
    db *sql.DB
}

func (p *OrderProcessor) Process(ctx context.Context, msg *OrderMessage) error {
    // Check idempotency
    exists, err := p.orderExists(ctx, msg.OrderID)
    if err != nil {
        return err
    }
    if exists {
        return nil // Already processed
    }

    // Process the order
    return p.createOrder(ctx, msg)
}
```

### Acknowledger

Confirms successful processing back to the queue:

```go
type Acknowledger[T any] interface {
    Acknowledge(ctx context.Context, msg T) error
}
```

**Responsibilities:**
- Commit offsets or delete messages from the queue
- Ensure the queue knows the message was processed
- Handle acknowledgment failures

**Example:**
```go
type KafkaAcknowledger struct {
    client *kgo.Client
}

func (a *KafkaAcknowledger) Acknowledge(ctx context.Context, msg *Message) error {
    // Commit the offset for this message
    return a.client.CommitRecords(ctx, msg.record)
}
```

### Runtime

Orchestrates the three phases and manages the application lifecycle:

```go
type Runtime interface {
    ProcessQueue(ctx context.Context) error
}
```

**Responsibilities:**
- Coordinate Consumer, Processor, and Acknowledger
- Implement the delivery semantics (order of phases)
- Handle graceful shutdown when context is cancelled
- Manage concurrency (e.g., goroutines per partition)

**Example:**
```go
type MyRuntime struct {
    consumer     queue.Consumer[Message]
    processor    queue.Processor[Message]
    acknowledger queue.Acknowledger[Message]
}

func (r *MyRuntime) ProcessQueue(ctx context.Context) error {
    for {
        // Phase 1: Consume
        msg, err := r.consumer.Consume(ctx)
        if errors.Is(err, queue.ErrEndOfQueue) {
            return nil // Graceful shutdown
        }
        if err != nil {
            return err
        }

        // Phase 2: Process
        if err := r.processor.Process(ctx, msg); err != nil {
            return err
        }

        // Phase 3: Acknowledge
        if err := r.acknowledger.Acknowledge(ctx, msg); err != nil {
            return err
        }
    }
}
```

## Built-in Item Processors

The queue package provides two built-in processors that implement different delivery semantics:

### ProcessAtMostOnce

At-most-once processing acknowledges messages before processing:

```go
processor := queue.ProcessAtMostOnce(consumer, processor, acknowledger)

for {
    err := processor.ProcessItem(ctx)
    if errors.Is(err, queue.ErrEndOfQueue) {
        return nil
    }
    // Continue even on errors - message already acknowledged
}
```

**Processing Order:** Consume → Acknowledge → Process

**Guarantees:**
- Each message processed at most once
- Messages may be lost on processing failures
- Fast throughput

### ProcessAtLeastOnce

At-least-once processing acknowledges messages after successful processing:

```go
processor := queue.ProcessAtLeastOnce(consumer, processor, acknowledger)

for {
    err := processor.ProcessItem(ctx)
    if errors.Is(err, queue.ErrEndOfQueue) {
        return nil
    }
    if err != nil {
        return err // Message not acknowledged, will be retried
    }
}
```

**Processing Order:** Consume → Process → Acknowledge

**Guarantees:**
- Each message processed at least once
- Messages may be duplicated on failures
- Requires idempotent processors

See [Delivery Semantics]({{< ref "delivery-semantics" >}}) for a detailed comparison.

## App Wrapper

The `queue.App` type wraps a Runtime and integrates it with the Bedrock framework:

```go
func NewApp(runtime Runtime) *App
```

**Features:**
- Calls `runtime.ProcessQueue(ctx)` on startup
- Handles context cancellation
- Returns errors to the framework for logging

**Example:**
```go
func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    runtime := &MyRuntime{...}
    return queue.NewApp(runtime), nil
}
```

## Builder Pattern

The `queue.Builder` function creates a Bedrock `AppBuilder` with automatic instrumentation:

```go
func Builder[T any](init func(context.Context, T) (*App, error)) bedrock.AppBuilder[T]
```

**Automatic Features:**
- OpenTelemetry SDK initialization
- Panic recovery in handlers
- OS signal handling (SIGTERM, SIGINT, SIGKILL)
- Graceful shutdown coordination

**Usage:**
```go
builder := queue.Builder(Init)
app, err := builder(ctx, cfg)
```

## Run Function

The `queue.Run` function provides a complete entry point for queue services:

```go
func Run[T any](
    reader io.Reader,
    init func(context.Context, T) (*App, error),
    opts ...RunOption,
) error
```

**Workflow:**
1. Read YAML configuration from reader
2. Parse config into type T
3. Call init function to build App
4. Run app until completion or error
5. Log errors and exit

**Example:**
```go
func main() {
    queue.Run(queue.YamlSource("config.yaml"), Init)
}
```

## Graceful Shutdown

The framework handles graceful shutdown automatically:

1. **Signal Handling** - SIGTERM/SIGINT triggers context cancellation
2. **Consumer Stops** - Consumer returns `ErrEndOfQueue`
3. **In-Flight Processing** - Completes current messages
4. **Final Acknowledgment** - Commits final offsets (at-least-once)
5. **Cleanup** - Closes connections and resources

**Implementation:**
```go
func (r *MyRuntime) ProcessQueue(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            // Context cancelled, stop consuming
            return nil
        default:
        }

        msg, err := r.consumer.Consume(ctx)
        if errors.Is(err, queue.ErrEndOfQueue) {
            return nil
        }
        // ... process message
    }
}
```

## Error Handling

The framework provides structured error handling:

**ErrEndOfQueue:**
- Special error signaling queue exhaustion
- Triggers graceful shutdown
- Not treated as a failure

**Processing Errors:**
- Return errors from Processor for at-least-once retry
- Log and continue for at-most-once (message lost)

**Fatal Errors:**
- Consumer/Acknowledger errors typically fatal
- Return from ProcessQueue to shut down

## OpenTelemetry Integration

All queue processing is automatically instrumented:

**Automatic Tracing:**
- Span per message
- Processing order visible in traces
- Context propagation through phases

**Automatic Logging:**
- Structured logs with message metadata
- Error recording in spans
- Performance metrics

No additional configuration needed in your Processor implementation.

## Next Steps

- Learn about [Delivery Semantics]({{< ref "delivery-semantics" >}}) to choose the right processing model
- Build your first processor with the [Kafka Quick Start]({{< ref "kafka/quick-start" >}})
