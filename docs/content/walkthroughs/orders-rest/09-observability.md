---
title: Observability
description: Explore traces, logs, and metrics in Grafana
weight: 8
type: docs
slug: observability
---

One of Humus's key features is automatic OpenTelemetry instrumentation. Let's see it in action.

## Automatic Instrumentation

Your API is already instrumented! Here's what happens automatically:

1. **HTTP Server** - Every request creates a span with method, path, status code
2. **HTTP Client** - Outgoing calls to services appear as child spans
3. **Trace Propagation** - Context flows through all service calls
4. **Metrics** - Request latency, counts, error rates
5. **Logs** - Correlated with trace IDs

## Viewing Traces in Grafana

1. Open Grafana: http://localhost:3000
2. Go to **Explore** (compass icon in sidebar)
3. Select **Tempo** as data source
4. Run a query or browse recent traces

Make some API calls:

```bash
# Successful order placement
curl -s -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{"customer_id":"CUST-001","account_id":"ACC-001"}'

# List orders
curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001"
```

## Trace Structure

For the POST /v1/order request, you'll see a trace with:

```
orders-api: POST /v1/order
  ├─ RestrictionService call
  │   └─ HTTP GET /restrictions/ACC-001 (to Wiremock)
  ├─ EligibilityService call
  │   └─ HTTP GET /eligibility/ACC-001 (to Wiremock)
  └─ DataService call
      └─ HTTP POST /data/orders (to Wiremock)
```

Each span shows:
- Duration
- HTTP status code
- Any errors
- Attributes (method, URL, etc.)

## Viewing Logs in Loki

1. In Grafana, go to **Explore**
2. Select **Loki** as data source
3. Query: `{job="orders-api"}`

Logs are automatically correlated with traces via trace IDs. Click on a log line to jump to its trace.

## Viewing Metrics in Mimir

1. In Grafana, go to **Explore**
2. Select **Mimir** as data source
3. Query examples:
   - `http_server_request_duration_seconds_bucket` - Request latency histogram
   - `http_server_request_total` - Total request count

## Key Benefits

### No Code Changes Required

You didn't write any telemetry code! The framework:
- Initializes OTel SDK
- Configures exporters from config.yaml
- Wraps HTTP server with instrumentation
- Uses `otelhttp.NewTransport` for client calls

### Distributed Tracing

Even though we're calling mock services, you see the full distributed trace. In production, if those services also use OTel, the trace continues across service boundaries.

### Correlation

All signals (traces, logs, metrics) are correlated:
- Logs contain trace IDs
- Metrics have trace exemplars
- Everything ties back to the same request

## Custom Instrumentation

While automatic instrumentation covers most cases, you can add custom spans:

```go
import "go.opentelemetry.io/otel"

func (h *handler) Handle(ctx context.Context) error {
    tracer := otel.Tracer("mypackage")
    ctx, span := tracer.Start(ctx, "custom-operation")
    defer span.End()

    // Your code here
    return nil
}
```

## What's Next

Let's run through complete end-to-end testing scenarios.

[Next: Running & Testing →]({{< ref "/walkthroughs/orders-rest/10-running-testing" >}})
