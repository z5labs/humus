---
title: Production Configuration
description: Deploying Kafka processors in production
weight: 80
type: docs
---

# Production Configuration

This guide covers best practices for deploying and configuring Kafka processors in production environments.

## Complete Configuration Example

A production-ready configuration:

```yaml
kafka:
  brokers:
    - "{{env "KAFKA_BROKER_1" | default "kafka-1:9092"}}"
    - "{{env "KAFKA_BROKER_2" | default "kafka-2:9092"}}"
    - "{{env "KAFKA_BROKER_3" | default "kafka-3:9092"}}"
  group_id: "{{env "KAFKA_GROUP_ID" | default "order-processors"}}"
  topics:
    - name: "orders"
      semantic: "at-least-once"
    - name: "payments"
      semantic: "at-least-once"
  session_timeout: "{{env "KAFKA_SESSION_TIMEOUT" | default "30s"}}"
  rebalance_timeout: "{{env "KAFKA_REBALANCE_TIMEOUT" | default "45s"}}"
  fetch_max_bytes: {{env "KAFKA_FETCH_MAX_BYTES" | default "52428800"}}
  max_concurrent_fetches: {{env "KAFKA_MAX_CONCURRENT_FETCHES" | default "10"}}

otel:
  service:
    name: "{{env "SERVICE_NAME" | default "order-processor"}}"
    version: "{{env "SERVICE_VERSION" | default "1.0.0"}}"
    environment: "{{env "ENVIRONMENT" | default "production"}}"
  sdk:
    disabled: false
  exporter:
    otlp:
      endpoint: "{{env "OTEL_ENDPOINT" | default "otel-collector:4317"}}"
      protocol: grpc
  traces:
    sampler:
      type: "{{env "OTEL_TRACE_SAMPLER" | default "traceidratio"}}"
      arg: {{env "OTEL_TRACE_SAMPLE_RATE" | default "0.1"}}
  metrics:
    interval: 60s

database:
  host: "{{env "DB_HOST" | default "postgres"}}"
  port: {{env "DB_PORT" | default "5432"}}
  name: "{{env "DB_NAME" | default "orders"}}"
  user: "{{env "DB_USER" | default "orders_user"}}"
  password: "{{env "DB_PASSWORD"}}"
  max_connections: {{env "DB_MAX_CONNECTIONS" | default "25"}}
```

## Kafka Configuration

### Broker Configuration

**Multiple Brokers:**
```yaml
kafka:
  brokers:
    - "kafka-1.prod:9092"
    - "kafka-2.prod:9092"
    - "kafka-3.prod:9092"
```

**Best practices:**
- Use at least 3 brokers for redundancy
- Use DNS names, not IP addresses
- Configure all brokers, not just one

### Consumer Group ID

**Unique per deployment:**
```yaml
kafka:
  group_id: "order-processors-prod"  # Different from staging
```

**Naming conventions:**
```
{service}-{environment}
Examples:
  - order-processors-prod
  - payment-processors-staging
  - analytics-processors-dev
```

### Topic Configuration

**Production topics:**
```yaml
kafka:
  topics:
    - name: "orders"
      semantic: "at-least-once"
    - name: "payments"
      semantic: "at-least-once"
    - name: "analytics"
      semantic: "at-most-once"
```

**Topic naming:**
```
{domain}.{entity}.{event}
Examples:
  - ecommerce.orders.created
  - ecommerce.payments.completed
  - analytics.events.tracked
```

### Timeouts

**Production timeouts:**
```yaml
kafka:
  session_timeout: "30s"      # Balance between failure detection and GC tolerance
  rebalance_timeout: "45s"    # Must be > session_timeout
```

**Guidelines:**
- **Session timeout:** 20-45s (default: 30s)
- **Rebalance timeout:** 45-90s (default: 60s)
- Increase if frequent rebalances occur
- Decrease for faster failure detection

### Fetch Settings

**Production fetch settings:**
```yaml
kafka:
  fetch_max_bytes: 52428800        # 50 MB
  max_concurrent_fetches: 10       # Limit concurrent requests
```

**Tuning guidelines:**
- **Small messages:** 10-25 MB fetch size
- **Large messages:** 100+ MB fetch size
- **High throughput:** Increase concurrent fetches
- **Memory constrained:** Decrease fetch size

## Application Configuration

### Idempotency

**Database-backed idempotency:**
```go
type Config struct {
    queue.Config `config:",squash"`
    Kafka struct {
        // ... kafka config
    } `config:"kafka"`
    Database struct {
        Host           string `config:"host"`
        Port           int    `config:"port"`
        Name           string `config:"name"`
        User           string `config:"user"`
        Password       string `config:"password"`
        MaxConnections int    `config:"max_connections"`
    } `config:"database"`
    IdempotencyWindow time.Duration `config:"idempotency_window"`
}

func Init(ctx context.Context, cfg Config) (*queue.App, error) {
    // Setup database connection pool
    db, err := sql.Open("postgres", fmt.Sprintf(
        "host=%s port=%d user=%s password=%s dbname=%s sslmode=require",
        cfg.Database.Host,
        cfg.Database.Port,
        cfg.Database.User,
        cfg.Database.Password,
        cfg.Database.Name,
    ))
    if err != nil {
        return nil, err
    }

    db.SetMaxOpenConns(cfg.Database.MaxConnections)
    db.SetMaxIdleConns(cfg.Database.MaxConnections / 2)
    db.SetConnMaxLifetime(time.Hour)

    processor := &OrderProcessor{
        db:                db,
        idempotencyWindow: cfg.IdempotencyWindow,
    }

    runtime := kafka.NewRuntime(
        cfg.Kafka.Brokers,
        cfg.Kafka.GroupID,
        kafka.AtLeastOnce(cfg.Kafka.Topic, processor),
        kafka.SessionTimeout(cfg.Kafka.SessionTimeout),
        kafka.RebalanceTimeout(cfg.Kafka.RebalanceTimeout),
    )

    return queue.NewApp(runtime), nil
}
```

### Resource Limits

**Database connection pooling:**
```go
db.SetMaxOpenConns(25)       // Limit total connections
db.SetMaxIdleConns(10)       // Idle connections
db.SetConnMaxLifetime(1 * time.Hour)  // Connection lifetime
```

**Calculation:**
```
Max connections = Partitions × Concurrent operations per partition
Example: 12 partitions × 2 ops = 24 connections (use 25)
```

## Deployment

### Container Configuration

**Dockerfile:**
```dockerfile
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o processor ./cmd/processor

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/

COPY --from=builder /app/processor .
COPY config.yaml .

CMD ["./processor"]
```

**Docker Compose:**
```yaml
version: '3.8'
services:
  order-processor:
    image: order-processor:latest
    environment:
      - KAFKA_BROKER_1=kafka-1:9092
      - KAFKA_BROKER_2=kafka-2:9092
      - KAFKA_GROUP_ID=order-processors-prod
      - KAFKA_SESSION_TIMEOUT=30s
      - DB_HOST=postgres
      - DB_PASSWORD=${DB_PASSWORD}
      - OTEL_ENDPOINT=otel-collector:4317
    depends_on:
      - kafka
      - postgres
    restart: unless-stopped
    deploy:
      replicas: 3  # Scale horizontally
      resources:
        limits:
          cpus: '1'
          memory: 512M
        reservations:
          cpus: '0.5'
          memory: 256M
```

### Kubernetes Deployment

**Deployment manifest:**
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: order-processor
  namespace: production
spec:
  replicas: 3
  selector:
    matchLabels:
      app: order-processor
  template:
    metadata:
      labels:
        app: order-processor
    spec:
      containers:
      - name: processor
        image: order-processor:1.0.0
        env:
        - name: KAFKA_BROKER_1
          value: "kafka-1.kafka.svc:9092"
        - name: KAFKA_BROKER_2
          value: "kafka-2.kafka.svc:9092"
        - name: KAFKA_GROUP_ID
          value: "order-processors-prod"
        - name: DB_PASSWORD
          valueFrom:
            secretKeyRef:
              name: db-credentials
              key: password
        - name: OTEL_ENDPOINT
          value: "otel-collector.monitoring.svc:4317"
        resources:
          requests:
            memory: "256Mi"
            cpu: "500m"
          limits:
            memory: "512Mi"
            cpu: "1000m"
        livenessProbe:
          httpGet:
            path: /health/liveness
            port: 8080
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health/readiness
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 5
```

### Scaling

**Horizontal scaling:**
```bash
# Scale to 6 replicas
kubectl scale deployment order-processor --replicas=6
```

**Scaling guidelines:**
- Max replicas ≤ number of partitions
- Start with 2-3 replicas for HA
- Scale up if consumer lag increases
- Monitor CPU/memory usage

**Example:**
```
12 partitions:
  - 3 replicas: 4 partitions each
  - 6 replicas: 2 partitions each
  - 12 replicas: 1 partition each (max)
```

## Monitoring

### Metrics to Monitor

**Consumer lag:**
```promql
kafka_consumer_lag{group="order-processors",topic="orders"}
```

**Alert when lag > 1000:**
```yaml
- alert: HighConsumerLag
  expr: kafka_consumer_lag > 1000
  for: 5m
  annotations:
    summary: "Consumer group {{ $labels.group }} lagging"
```

**Processing rate:**
```promql
rate(kafka_messages_processed_total[1m])
```

**Error rate:**
```promql
rate(kafka_processing_errors_total[1m])
```

### Health Checks

**Kubernetes probes:**
```yaml
livenessProbe:
  httpGet:
    path: /health/liveness
    port: 8080
  initialDelaySeconds: 30
  periodSeconds: 10
  failureThreshold: 3

readinessProbe:
  httpGet:
    path: /health/readiness
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 5
  failureThreshold: 3
```

**Custom health check:**
```go
type HealthCheck struct {
    processor *OrderProcessor
}

func (h *HealthCheck) Healthy(ctx context.Context) (bool, error) {
    // Check database connection
    if err := h.processor.db.PingContext(ctx); err != nil {
        return false, err
    }

    // Check consumer lag (if available)
    // ...

    return true, nil
}
```

## Security

### TLS Configuration

**Kafka TLS:**
```go
import "github.com/twmb/franz-go/pkg/kgo"

tlsConfig := &tls.Config{
    MinVersion: tls.VersionTLS12,
}

runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", processor),
    kafka.WithKafkaOptions(
        kgo.DialTLSConfig(tlsConfig),
    ),
)
```

### SASL Authentication

**SASL/SCRAM:**
```go
import (
    "github.com/twmb/franz-go/pkg/kgo"
    "github.com/twmb/franz-go/pkg/sasl/scram"
)

scramAuth := scram.Auth{
    User: cfg.Kafka.User,
    Pass: cfg.Kafka.Password,
}

runtime := kafka.NewRuntime(
    brokers,
    groupID,
    kafka.AtLeastOnce("orders", processor),
    kafka.WithKafkaOptions(
        kgo.SASL(scramAuth.AsSha256Mechanism()),
    ),
)
```

### Secrets Management

**Kubernetes secrets:**
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: kafka-credentials
type: Opaque
stringData:
  username: "order-processor-user"
  password: "secure-password-here"
```

**Reference in deployment:**
```yaml
env:
- name: KAFKA_USER
  valueFrom:
    secretKeyRef:
      name: kafka-credentials
      key: username
- name: KAFKA_PASSWORD
  valueFrom:
    secretKeyRef:
      name: kafka-credentials
      key: password
```

## Performance Tuning

### Partition Count

**Optimal partition count:**
```
Partitions = Target throughput / Partition throughput

Example:
  Target: 100k msgs/sec
  Per partition: 10k msgs/sec
  Partitions: 100k / 10k = 10
```

**Recommendations:**
- Start with 3× expected consumer count
- Monitor lag and throughput
- Increase if lag grows under load
- Cannot decrease (Kafka limitation)

### Batch Processing

**Process messages in batches:**
```go
type BatchProcessor struct {
    db        *sql.DB
    batchSize int
}

func (p *BatchProcessor) ProcessBatch(ctx context.Context, messages []kafka.Message) error {
    tx, err := p.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    for _, msg := range messages {
        var order Order
        json.Unmarshal(msg.Value, &order)

        _, err := tx.ExecContext(ctx,
            "INSERT INTO orders (order_id, total) VALUES ($1, $2)",
            order.OrderID, order.Total,
        )
        if err != nil {
            return err
        }
    }

    return tx.Commit()
}
```

### Connection Pooling

**Optimize database connections:**
```go
// Formula: Max connections = Partitions × 2
db.SetMaxOpenConns(cfg.Partitions * 2)
db.SetMaxIdleConns(cfg.Partitions)
db.SetConnMaxLifetime(1 * time.Hour)
db.SetConnMaxIdleTime(10 * time.Minute)
```

## Disaster Recovery

### Consumer Group Reset

**Reset to earliest:**
```bash
kafka-consumer-groups.sh \
  --bootstrap-server kafka:9092 \
  --group order-processors \
  --reset-offsets \
  --to-earliest \
  --all-topics \
  --execute
```

**Reset to specific offset:**
```bash
kafka-consumer-groups.sh \
  --bootstrap-server kafka:9092 \
  --group order-processors \
  --reset-offsets \
  --topic orders:0 \
  --to-offset 12345 \
  --execute
```

### Backup and Restore

**Export offsets:**
```bash
kafka-consumer-groups.sh \
  --bootstrap-server kafka:9092 \
  --group order-processors \
  --describe > offsets-backup.txt
```

**Dead Letter Queue:**
```go
func (p *Processor) Process(ctx context.Context, msg kafka.Message) error {
    if err := p.processMessage(ctx, msg); err != nil {
        // Send to DLQ on failure
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
    return nil  // Don't return error, message handled
}
```

## Troubleshooting

### High Consumer Lag

**Causes:**
- Slow processing
- Insufficient consumers
- Hot partitions

**Solutions:**
```bash
# Scale up consumers
kubectl scale deployment order-processor --replicas=6

# Check for hot partitions
kafka-consumer-groups.sh --describe --group order-processors

# Optimize processing code
# Add database indexes
# Batch operations
```

### Frequent Rebalances

**Causes:**
- Short session timeout
- Long processing time
- Network issues

**Solutions:**
```yaml
kafka:
  session_timeout: "45s"      # Increase
  rebalance_timeout: "90s"    # Increase
```

### Memory Issues

**Causes:**
- Large fetch buffers
- Too many partitions
- Memory leaks

**Solutions:**
```yaml
kafka:
  fetch_max_bytes: 25000000   # Reduce to 25 MB
  max_concurrent_fetches: 5   # Limit concurrent fetches
```

## Next Steps

- Review [Quick Start]({{< ref "quick-start" >}}) for basic setup
- Learn [Idempotency]({{< ref "idempotency" >}}) patterns
- Explore [Observability]({{< ref "observability" >}}) for monitoring
