---
title: Multi-Topic Processing
description: Processing multiple Kafka topics in one runtime
weight: 60
type: docs
---

# Multi-Topic Processing

The Kafka runtime supports consuming and processing multiple topics simultaneously, each with its own processor and delivery semantics.

## Basic Multi-Topic Configuration

Configure multiple topics in a single runtime:

```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", ordersProcessor),
    kafka.AtLeastOnce("payments", paymentsProcessor),
    kafka.AtMostOnce("metrics", metricsProcessor),
)
```

**Key features:**
- Each topic has its own processor
- Different delivery semantics per topic
- All topics share the same consumer group
- Partitions from all topics processed concurrently

## Processor per Topic

Define separate processors for each topic:

```go
type OrdersProcessor struct {
    db *sql.DB
}

func (p *OrdersProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order Order
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }
    return p.processOrder(ctx, order)
}

type PaymentsProcessor struct {
    db *sql.DB
}

func (p *PaymentsProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var payment Payment
    if err := json.Unmarshal(msg.Value, &payment); err != nil {
        return err
    }
    return p.processPayment(ctx, payment)
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    ordersProc := &OrdersProcessor{db: cfg.DB}
    paymentsProc := &PaymentsProcessor{db: cfg.DB}

    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce("orders", ordersProc),
        kafka.AtLeastOnce("payments", paymentsProc),
    )

    return queue.NewApp(runtime), nil
}
```

## Shared State Between Topics

Processors can share state:

```go
type SharedProcessor struct {
    mu    sync.RWMutex
    cache map[string]string
    db    *sql.DB
}

type OrdersProcessor struct {
    *SharedProcessor
}

func (p *OrdersProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var order Order
    json.Unmarshal(msg.Value, &order)

    // Access shared cache
    p.mu.RLock()
    customerData := p.cache[order.CustomerID]
    p.mu.RUnlock()

    return p.processOrder(ctx, order, customerData)
}

type PaymentsProcessor struct {
    *SharedProcessor
}

func (p *PaymentsProcessor) Process(ctx context.Context, msg kafka.Message) error {
    var payment Payment
    json.Unmarshal(msg.Value, &payment)

    // Update shared cache
    p.mu.Lock()
    p.cache[payment.CustomerID] = payment.Status
    p.mu.Unlock()

    return p.processPayment(ctx, payment)
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    shared := &SharedProcessor{
        cache: make(map[string]string),
        db:    cfg.DB,
    }

    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce("orders", &OrdersProcessor{shared}),
        kafka.AtLeastOnce("payments", &PaymentsProcessor{shared}),
    )

    return queue.NewApp(runtime), nil
}
```

## Topic-Based Routing

Route messages by topic to a unified handler:

```go
type UnifiedProcessor struct {
    db *sql.DB
}

func (p *UnifiedProcessor) Process(ctx context.Context, msg kafka.Message) error {
    switch msg.Topic {
    case "orders":
        return p.processOrder(ctx, msg)
    case "payments":
        return p.processPayment(ctx, msg)
    case "shipments":
        return p.processShipment(ctx, msg)
    default:
        return fmt.Errorf("unknown topic: %s", msg.Topic)
    }
}

func (p *UnifiedProcessor) processOrder(ctx context.Context, msg kafka.Message) error {
    var order Order
    if err := json.Unmarshal(msg.Value, &order); err != nil {
        return err
    }
    // Process order
    return nil
}

func (p *UnifiedProcessor) processPayment(ctx context.Context, msg kafka.Message) error {
    var payment Payment
    if err := json.Unmarshal(msg.Value, &payment); err != nil {
        return err
    }
    // Process payment
    return nil
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    processor := &UnifiedProcessor{db: cfg.DB}

    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce("orders", processor),
        kafka.AtLeastOnce("payments", processor),
        kafka.AtLeastOnce("shipments", processor),
    )

    return queue.NewApp(runtime), nil
}
```

## Mixed Delivery Semantics

Use different semantics for different topics:

```go
runtime := kafka.NewRuntime(
    brokers,
    groupID,
    // Critical topics: at-least-once
    kafka.AtLeastOnce("orders", ordersProcessor),
    kafka.AtLeastOnce("payments", paymentsProcessor),
    kafka.AtLeastOnce("inventory", inventoryProcessor),

    // Non-critical topics: at-most-once
    kafka.AtMostOnce("metrics", metricsProcessor),
    kafka.AtMostOnce("logs", logsProcessor),
    kafka.AtMostOnce("analytics", analyticsProcessor),
)
```

**Rationale:**
- **Orders, payments, inventory:** Cannot lose data → at-least-once
- **Metrics, logs, analytics:** Can tolerate loss → at-most-once

## Configuration

### YAML Configuration

```yaml
kafka:
  brokers:
    - "localhost:9092"
  group_id: "multi-topic-processor"
  topics:
    orders:
      semantic: "at-least-once"
    payments:
      semantic: "at-least-once"
    metrics:
      semantic: "at-most-once"
```

### Dynamic Topic Configuration

```go
type TopicConfig struct {
    Name     string
    Semantic string
}

type Config struct {
    queue.Config `config:",squash"`
    Kafka struct {
        Brokers []string      `config:"brokers"`
        GroupID string        `config:"group_id"`
        Topics  []TopicConfig `config:"topics"`
    } `config:"kafka"`
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    opts := make([]kafka.Option, 0, len(cfg.Kafka.Topics))

    for _, topic := range cfg.Kafka.Topics {
        switch topic.Semantic {
        case "at-least-once":
            processor := newProcessor(topic.Name)
            opts = append(opts, kafka.AtLeastOnce(topic.Name, processor))
        case "at-most-once":
            processor := newProcessor(topic.Name)
            opts = append(opts, kafka.AtMostOnce(topic.Name, processor))
        }
    }

    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        opts...,
    )

    return queue.NewApp(runtime), nil
}
```

## Workflow Patterns

### Sequential Processing

Process related messages across topics:

```go
type WorkflowProcessor struct {
    db *sql.DB
}

// Orders topic
func (p *WorkflowProcessor) ProcessOrder(ctx context.Context, msg kafka.Message) error {
    var order Order
    json.Unmarshal(msg.Value, &order)

    // 1. Save order
    if err := p.saveOrder(ctx, order); err != nil {
        return err
    }

    // 2. Check payment (from payments topic)
    // This will be processed by ProcessPayment when payment arrives
    return nil
}

// Payments topic
func (p *WorkflowProcessor) ProcessPayment(ctx context.Context, msg kafka.Message) error {
    var payment Payment
    json.Unmarshal(msg.Value, &payment)

    // 1. Save payment
    if err := p.savePayment(ctx, payment); err != nil {
        return err
    }

    // 2. Update order status
    return p.updateOrderStatus(ctx, payment.OrderID, "paid")
}
```

### Event Aggregation

Aggregate events from multiple topics:

```go
type AggregationProcessor struct {
    mu         sync.RWMutex
    aggregates map[string]*Aggregate
}

type Aggregate struct {
    OrderReceived  bool
    PaymentReceived bool
    ShipmentReceived bool
}

func (p *AggregationProcessor) ProcessOrder(ctx context.Context, msg kafka.Message) error {
    var order Order
    json.Unmarshal(msg.Value, &order)

    p.mu.Lock()
    defer p.mu.Unlock()

    agg := p.getAggregate(order.OrderID)
    agg.OrderReceived = true

    if p.isComplete(agg) {
        return p.finalizeOrder(ctx, order.OrderID)
    }

    return nil
}

func (p *AggregationProcessor) ProcessPayment(ctx context.Context, msg kafka.Message) error {
    var payment Payment
    json.Unmarshal(msg.Value, &payment)

    p.mu.Lock()
    defer p.mu.Unlock()

    agg := p.getAggregate(payment.OrderID)
    agg.PaymentReceived = true

    if p.isComplete(agg) {
        return p.finalizeOrder(ctx, payment.OrderID)
    }

    return nil
}

func (p *AggregationProcessor) isComplete(agg *Aggregate) bool {
    return agg.OrderReceived && agg.PaymentReceived && agg.ShipmentReceived
}
```

### Topic Fan-In

Multiple topics feed into one aggregator:

```go
type EventAggregator struct {
    db *sql.DB
}

func (p *EventAggregator) Process(ctx context.Context, msg kafka.Message) error {
    // Common event envelope
    type Event struct {
        Type      string          `json:"type"`
        Timestamp time.Time       `json:"timestamp"`
        Data      json.RawMessage `json:"data"`
    }

    var event Event
    if err := json.Unmarshal(msg.Value, &event); err != nil {
        return err
    }

    // Store in unified events table
    _, err := p.db.ExecContext(ctx,
        `INSERT INTO events (topic, type, timestamp, data)
         VALUES ($1, $2, $3, $4)`,
        msg.Topic, event.Type, event.Timestamp, event.Data,
    )

    return err
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    aggregator := &EventAggregator{db: cfg.DB}

    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        // All topics use the same aggregator
        kafka.AtLeastOnce("user-events", aggregator),
        kafka.AtLeastOnce("order-events", aggregator),
        kafka.AtLeastOnce("payment-events", aggregator),
    )

    return queue.NewApp(runtime), nil
}
```

## Partition Distribution

### How Partitions Are Assigned

With multiple topics, partitions from all topics are distributed:

```
Consumer Group "processors" with 2 consumers:

Consumer 1:                   Consumer 2:
  orders (partition 0)          orders (partition 1)
  orders (partition 2)          payments (partition 0)
  payments (partition 1)        payments (partition 2)
```

**Key points:**
- Partitions assigned across all topics
- Load balanced across consumers
- No guarantee which consumer gets which topic

### Topic Affinity

To ensure a consumer processes specific topics, use separate consumer groups:

```go
// Separate runtimes for different topics
func Init(ctx context.Context, cfg Config) ([]*queue.App, error) {
    // Runtime 1: Critical topics
    runtime1 := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        "critical-processor",  // Different group ID
        kafka.AtLeastOnce("orders", ordersProcessor),
        kafka.AtLeastOnce("payments", paymentsProcessor),
    )

    // Runtime 2: Analytics topics
    runtime2 := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        "analytics-processor",  // Different group ID
        kafka.AtMostOnce("metrics", metricsProcessor),
        kafka.AtMostOnce("logs", logsProcessor),
    )

    return []*queue.App{
        queue.NewApp(runtime1),
        queue.NewApp(runtime2),
    }, nil
}
```

Note: This requires running multiple apps, which is outside the standard pattern. Consider separate deployments instead.

## Monitoring Multi-Topic Processing

### Topic-Level Metrics

Monitor lag per topic:

```bash
kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 \
  --group multi-topic-processor \
  --describe
```

Output:
```
TOPIC      PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG
orders     0          1000            1000            0
orders     1          1050            1050            0
payments   0          500             520             20   # Lagging
payments   1          510             510             0
metrics    0          5000            5000            0
```

### Processor-Level Logging

Log which processor handles each message:

```go
func (p *OrdersProcessor) Process(ctx context.Context, msg kafka.Message) error {
    log.Info("Processing order",
        "topic", msg.Topic,
        "partition", msg.Partition,
        "offset", msg.Offset,
        "processor", "orders",
    )

    return nil
}
```

## Best Practices

### Separate Concerns

Use different processors for different business domains:

```go
// Good: Separate processors
kafka.AtLeastOnce("orders", ordersProcessor)
kafka.AtLeastOnce("payments", paymentsProcessor)

// Avoid: One processor for everything
kafka.AtLeastOnce("orders", genericProcessor)
kafka.AtLeastOnce("payments", genericProcessor)
```

### Match Semantics to Criticality

```go
// Critical: At-least-once
kafka.AtLeastOnce("financial-transactions", processor)

// Non-critical: At-most-once
kafka.AtMostOnce("usage-metrics", processor)
```

### Shared State Needs Locks

```go
type Processor struct {
    mu    sync.RWMutex
    state map[string]int
}

// Safe: Uses locks
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    p.mu.Lock()
    p.state[string(msg.Key)]++
    p.mu.Unlock()
    return nil
}
```

### Independent Topic Processing

Avoid dependencies between topics when possible:

```go
// Good: Independent processing
func (p *OrdersProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Only processes orders, no dependencies
    return p.saveOrder(ctx, msg)
}

// Avoid: Cross-topic dependencies
func (p *OrdersProcessor) Process(ctx context.Context, msg kafka.Message) error {
    // Waiting for payment from another topic creates coupling
    payment := p.waitForPayment(msg.OrderID)  // Anti-pattern
    return p.saveOrder(ctx, msg, payment)
}
```

## Next Steps

- Learn about [Observability]({{< ref "observability" >}}) for multi-topic tracing
- Configure [Production Settings]({{< ref "configuration" >}}) for optimal performance
- Review [Idempotency]({{< ref "idempotency" >}}) patterns for each topic
