---
title: Observability
description: OpenTelemetry integration for traces, metrics, and logs
weight: 20
type: docs
---


Humus provides built-in observability through automatic OpenTelemetry (OTel) integration. Every service gets traces, metrics, and logs out of the box.

## Overview

OpenTelemetry is automatically initialized when your Humus service starts. You get:

- **Distributed Tracing** - Automatic HTTP/gRPC tracing plus manual instrumentation
- **Metrics** - Built-in HTTP/gRPC metrics plus custom metrics
- **Structured Logging** - Integrated `slog` logger with trace correlation

## Automatic Instrumentation

### REST Services

HTTP handlers are automatically instrumented with:

- **Request tracing** - Each request creates a span
- **HTTP metrics** - Request count, duration, status codes
- **Error tracking** - Automatic error recording in spans

```go
// No extra code needed - automatic instrumentation!
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    handler := rest.ProducerFunc[Response](handleRequest)  // Automatically traced

    api := rest.NewApi(
        "My Service",
        "1.0.0",
        rest.Handle(http.MethodGet, rest.BasePath("/users"), rest.ProduceJson(handler)),
    )
    return api, nil
}
```

### gRPC Services

gRPC methods are automatically instrumented via interceptors:

- **RPC tracing** - Each RPC creates a span
- **gRPC metrics** - Call count, duration, status
- **Error tracking** - Automatic error recording

```go
// gRPC instrumentation is automatic via interceptors
func Init(ctx context.Context, cfg Config) (*grpc.Api, error) {
    api := grpc.NewApi()
    userpb.RegisterUserServiceServer(api, &userService{})  // Automatically traced
    return api, nil
}
```

### Job Services

Jobs are traced from start to finish:

```go
type MyJob struct{}

func (j *MyJob) Handle(ctx context.Context) error {
    // The entire job execution is automatically traced
    // ctx already contains trace context
    return processJob(ctx)
}
```

## Configuration

### Basic OTel Config

Minimal configuration for local development:

```yaml
otel:
  service:
    name: my-service
  sdk:
    disabled: false  # Enable OTel
```

### Production Config

Full configuration for production:

```yaml
otel:
  service:
    name: my-service
    version: 1.0.0
    namespace: production
    instance_id: {{env "POD_NAME"}}

  sdk:
    disabled: false

  resource:
    attributes:
      deployment.environment: production
      service.team: platform
      k8s.cluster.name: prod-cluster

  traces:
    sampler:
      type: parentbased_traceidratio
      arg: 0.1  # Sample 10% of traces

    exporter:
      otlp:
        endpoint: {{env "OTEL_EXPORTER_OTLP_ENDPOINT"}}
        protocol: grpc
        headers:
          api-key: {{env "OTEL_API_KEY"}}

  metrics:
    exporter:
      otlp:
        endpoint: {{env "OTEL_EXPORTER_OTLP_ENDPOINT"}}
        protocol: grpc

  logs:
    exporter:
      otlp:
        endpoint: {{env "OTEL_EXPORTER_OTLP_ENDPOINT"}}
        protocol: grpc
```

### Disabling OTel

For development or testing:

```yaml
otel:
  sdk:
    disabled: true  # No telemetry overhead
```

## Manual Instrumentation

### Creating Spans

Use the standard OTel SDK to create custom spans:

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/codes"
)

func processOrder(ctx context.Context, orderID string) error {
    // Get a tracer
    tracer := otel.Tracer("my-service")

    // Create a span
    ctx, span := tracer.Start(ctx, "processOrder")
    defer span.End()

    // Add attributes
    span.SetAttributes(
        attribute.String("order.id", orderID),
        attribute.Int("order.items", 5),
    )

    // Do work...
    if err := validateOrder(ctx, orderID); err != nil {
        // Record error
        span.RecordError(err)
        span.SetStatus(codes.Error, "validation failed")
        return err
    }

    span.SetStatus(codes.Ok, "order processed")
    return nil
}
```

### Nested Spans

Create hierarchical traces:

```go
func processOrder(ctx context.Context, orderID string) error {
    tracer := otel.Tracer("my-service")

    ctx, span := tracer.Start(ctx, "processOrder")
    defer span.End()

    // Child span 1
    if err := validateOrder(ctx, orderID); err != nil {
        return err
    }

    // Child span 2
    if err := chargePayment(ctx, orderID); err != nil {
        return err
    }

    return nil
}

func validateOrder(ctx context.Context, orderID string) error {
    tracer := otel.Tracer("my-service")

    // This will be a child of "processOrder" span
    ctx, span := tracer.Start(ctx, "validateOrder")
    defer span.End()

    // Validation logic...
    return nil
}
```

### Recording Metrics

Create custom metrics:

```go
import (
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/metric"
)

var (
    orderCounter metric.Int64Counter
    orderDuration metric.Float64Histogram
)

func init() {
    meter := otel.Meter("my-service")

    orderCounter, _ = meter.Int64Counter(
        "orders.processed",
        metric.WithDescription("Number of orders processed"),
    )

    orderDuration, _ = meter.Float64Histogram(
        "orders.duration",
        metric.WithDescription("Order processing duration"),
        metric.WithUnit("ms"),
    )
}

func processOrder(ctx context.Context, orderID string) error {
    start := time.Now()

    // Process order...

    // Record metrics
    orderCounter.Add(ctx, 1,
        metric.WithAttributes(
            attribute.String("status", "success"),
        ),
    )

    duration := time.Since(start).Milliseconds()
    orderDuration.Record(ctx, float64(duration))

    return nil
}
```

## Structured Logging

### Using Humus Logger

Get an OpenTelemetry-integrated logger:

```go
import "github.com/z5labs/humus"

func processOrder(ctx context.Context, orderID string) error {
    log := humus.Logger("order-processor")

    log.InfoContext(ctx, "processing order",
        "order_id", orderID,
        "user_id", "user123",
    )

    if err := validateOrder(ctx, orderID); err != nil {
        log.ErrorContext(ctx, "validation failed",
            "order_id", orderID,
            "error", err,
        )
        return err
    }

    log.InfoContext(ctx, "order processed successfully",
        "order_id", orderID,
    )

    return nil
}
```

### Log-Trace Correlation

Logs automatically include trace context when using `InfoContext`, `ErrorContext`, etc.:

```go
func handleRequest(ctx context.Context, req Request) (Response, error) {
    log := humus.Logger("handler")

    // This log will include trace_id and span_id
    log.InfoContext(ctx, "handling request",
        "request_id", req.ID,
    )

    // Trace and logs are correlated automatically
    tracer := otel.Tracer("my-service")
    ctx, span := tracer.Start(ctx, "processRequest")
    defer span.End()

    // This log also includes the same trace context
    log.InfoContext(ctx, "processing",
        "step", "validation",
    )

    return processRequest(ctx, req)
}
```

### Log Levels

Use appropriate log levels:

```go
log := humus.Logger("my-service")

// Debug - verbose information for debugging
log.DebugContext(ctx, "cache hit", "key", cacheKey)

// Info - normal operational messages
log.InfoContext(ctx, "request processed", "duration_ms", duration)

// Warn - warning messages
log.WarnContext(ctx, "rate limit approaching", "current", current, "limit", limit)

// Error - error messages
log.ErrorContext(ctx, "failed to connect", "error", err)
```

### Minimum Log Level Filtering

You can configure minimum log levels per logger name to filter out verbose logs from specific packages:

```yaml
otel:
  log:
    processor:
      type: batch
      batch:
        export_interval: 1s
        max_size: 512
    exporter:
      type: otlp
      otlp:
        type: grpc
        target: localhost:4317
    minimum_log_level:
      github.com/z5labs/humus/queue/kafka: info   # Filter DEBUG logs
      github.com/twmb/franz-go/pkg/kgo: warn      # Filter DEBUG and INFO logs
      github.com/some/verbose-lib: error           # Only ERROR logs
```

**How it works:**

- **Logger name matching**: Uses the instrumentation scope name (package path) from `humus.Logger("package/name")`
- **Prefix matching**: If "github.com/z5labs/humus" is configured, it matches "github.com/z5labs/humus/queue/kafka" and all subpackages
- **Longest prefix wins**: More specific configurations take precedence
- **Fail-open**: Unconfigured loggers allow all log levels

**Supported levels:**
- `debug` - All logs (most verbose)
- `info` - INFO and above
- `warn` or `warning` - WARN and above
- `error` - Only ERROR logs (least verbose)

This is useful for reducing noise from third-party libraries or specific internal packages without affecting other loggers.

## Sampling

Control trace volume with sampling:

### Always On (Development)

```yaml
otel:
  traces:
    sampler:
      type: always_on  # Capture all traces
```

### Ratio-Based (Production)

```yaml
otel:
  traces:
    sampler:
      type: traceidratio
      arg: 0.1  # Sample 10% of traces
```

### Parent-Based (Recommended)

```yaml
otel:
  traces:
    sampler:
      type: parentbased_traceidratio
      arg: 0.1  # Sample 10%, but respect parent decisions
```

This ensures distributed traces aren't broken by different sampling decisions.

## Exporters

### OTLP (Recommended)

Send to any OTLP-compatible collector:

```yaml
otel:
  traces:
    exporter:
      otlp:
        endpoint: http://localhost:4318
        protocol: http/protobuf  # or grpc
```

### Common Backends

**Jaeger:**
```yaml
otel:
  traces:
    exporter:
      otlp:
        endpoint: http://jaeger:4318
        protocol: http/protobuf
```

**Grafana Tempo:**
```yaml
otel:
  traces:
    exporter:
      otlp:
        endpoint: https://tempo.example.com
        protocol: grpc
        headers:
          authorization: {{env "GRAFANA_API_KEY"}}
```

**Honeycomb:**
```yaml
otel:
  traces:
    exporter:
      otlp:
        endpoint: https://api.honeycomb.io
        protocol: grpc
        headers:
          x-honeycomb-team: {{env "HONEYCOMB_API_KEY"}}
```

**Cloud Providers:**
```yaml
# AWS X-Ray (via OTLP)
otel:
  traces:
    exporter:
      otlp:
        endpoint: localhost:4317
        protocol: grpc

# Google Cloud Trace
otel:
  traces:
    exporter:
      otlp:
        endpoint: cloudtrace.googleapis.com:443
        protocol: grpc
```

## Resource Attributes

Add metadata to all telemetry:

```yaml
otel:
  service:
    name: my-service
    version: 1.0.0
    namespace: production

  resource:
    attributes:
      # Deployment info
      deployment.environment: production
      deployment.region: us-east-1

      # Team info
      service.team: platform
      service.owner: team-platform@example.com

      # Kubernetes info (from env)
      k8s.pod.name: {{env "POD_NAME"}}
      k8s.namespace.name: {{env "POD_NAMESPACE"}}
      k8s.node.name: {{env "NODE_NAME"}}
```

These attributes appear in all traces, metrics, and logs.

## Best Practices

### 1. Use Context Everywhere

Always pass `context.Context` to propagate traces:

```go
// Good
func processOrder(ctx context.Context, orderID string) error {
    result := validateOrder(ctx, orderID)  // Context propagates trace
    return saveOrder(ctx, result)
}

// Bad - traces won't connect
func processOrder(ctx context.Context, orderID string) error {
    result := validateOrder(context.Background(), orderID)  // New trace!
    return saveOrder(ctx, result)
}
```

### 2. Meaningful Span Names

Use clear, hierarchical names:

```go
// Good
tracer.Start(ctx, "order.validate")
tracer.Start(ctx, "payment.charge")
tracer.Start(ctx, "inventory.reserve")

// Less useful
tracer.Start(ctx, "step1")
tracer.Start(ctx, "process")
```

### 3. Add Relevant Attributes

Include contextual information:

```go
span.SetAttributes(
    attribute.String("order.id", orderID),
    attribute.String("user.id", userID),
    attribute.Int("order.total_cents", totalCents),
    attribute.String("payment.method", "credit_card"),
)
```

### 4. Record Errors

Always record errors in spans:

```go
if err := doWork(ctx); err != nil {
    span.RecordError(err)
    span.SetStatus(codes.Error, "work failed")
    return err
}
```

### 5. Use Structured Logging

Prefer structured fields over formatted strings:

```go
// Good
log.InfoContext(ctx, "order processed",
    "order_id", orderID,
    "duration_ms", duration,
)

// Less useful for querying
log.InfoContext(ctx, fmt.Sprintf("Order %s processed in %dms", orderID, duration))
```

## Next Steps

- Learn about [Lifecycle Management]({{< ref "lifecycle-management" >}}) for service execution
- Explore [REST Services]({{< ref "/features/rest" >}}) for HTTP-specific instrumentation
- See [gRPC Services]({{< ref "/features/grpc" >}}) for gRPC-specific instrumentation
- Read [Advanced Topics]({{< ref "/advanced/otel-integration" >}}) for advanced OTel patterns
