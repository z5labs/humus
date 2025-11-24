---
title: Observability Infrastructure
description: Add LGTM stack and OpenTelemetry Collector
weight: 8
type: docs
slug: infrastructure
---

Your API is working with Wiremock. Now let's add the full observability stack to see traces, logs, and metrics in Grafana.

## Understanding the LGTM Stack

The LGTM stack provides comprehensive observability:

- **Loki** - Log aggregation and querying
- **Grafana** - Unified visualization dashboard
- **Tempo** - Distributed tracing backend
- **Mimir** - Long-term metrics storage

We'll also add the **OpenTelemetry Collector** to receive and route telemetry data from your API.

## Updating Podman Compose

Update your `podman-compose.yaml` to add the observability stack:

```yaml
services:
  wiremock:
    image: docker.io/wiremock/wiremock:3.10.0
    ports:
      - "8080:8080"
    volumes:
      - ./wiremock:/home/wiremock:z
    command: --verbose

  tempo:
    image: docker.io/grafana/tempo:2.6.1
    command: ["-config.file=/etc/tempo.yaml"]
    volumes:
      - ./tempo.yaml:/etc/tempo.yaml:ro,z
    ports:
      - "3200:3200"

  loki:
    image: docker.io/grafana/loki:3.3.2
    ports:
      - "3100:3100"
    command: -config.file=/etc/loki/local-config.yaml

  mimir:
    image: docker.io/grafana/mimir:2.14.2
    command: ["-config.file=/etc/mimir.yaml"]
    volumes:
      - ./mimir.yaml:/etc/mimir.yaml:ro,z
    ports:
      - "9009:9009"

  otel-collector:
    image: docker.io/otel/opentelemetry-collector-contrib:0.115.1
    command: ["--config=/etc/otel-collector-config.yaml"]
    volumes:
      - ./otel-collector-config.yaml:/etc/otel-collector-config.yaml:ro,z
    ports:
      - "4317:4317"
    depends_on:
      - tempo
      - loki
      - mimir

  grafana:
    image: docker.io/grafana/grafana:11.4.0
    environment:
      - GF_AUTH_ANONYMOUS_ENABLED=true
      - GF_AUTH_ANONYMOUS_ORG_ROLE=Admin
      - GF_AUTH_DISABLE_LOGIN_FORM=true
    ports:
      - "3000:3000"
    volumes:
      - ./grafana-datasources.yaml:/etc/grafana/provisioning/datasources/datasources.yaml:ro,z
    depends_on:
      - tempo
      - loki
      - mimir
```

## Required Configuration Files

You'll need these configuration files in your project directory. These are available in the example directory at `example/rest/orders-walkthrough/`:

### tempo.yaml

Configures Tempo to accept OTLP traces:

```yaml
server:
  http_listen_port: 3200

distributor:
  receivers:
    otlp:
      protocols:
        grpc:
          endpoint: 0.0.0.0:4317

storage:
  trace:
    backend: local
    local:
      path: /tmp/tempo/blocks
```

### mimir.yaml

Configures Mimir for metrics storage:

```yaml
target: all

server:
  http_listen_port: 9009

ingester:
  ring:
    replication_factor: 1

blocks_storage:
  backend: filesystem
  filesystem:
    dir: /tmp/mimir/blocks
```

### otel-collector-config.yaml

Routes telemetry from your API to the backend services:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

exporters:
  otlp/tempo:
    endpoint: tempo:4317
    tls:
      insecure: true

  loki:
    endpoint: http://loki:3100/loki/api/v1/push

  otlphttp/mimir:
    endpoint: http://mimir:9009/otlp

processors:
  batch:

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/tempo]
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [loki]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/mimir]
```

### grafana-datasources.yaml

Configures Grafana to read from all backends:

```yaml
apiVersion: 1

datasources:
  - name: Tempo
    type: tempo
    access: proxy
    url: http://tempo:3200
    isDefault: true

  - name: Loki
    type: loki
    access: proxy
    url: http://loki:3100

  - name: Mimir
    type: prometheus
    access: proxy
    url: http://mimir:9009/prometheus
```

## Restarting the Stack

If Wiremock is still running from the previous step:

```bash
podman-compose down
```

Start the full stack:

```bash
podman-compose up -d
```

Verify all 6 containers are running:

```bash
podman ps --format "table {{.Names}}\t{{.Status}}"
```

Expected output:

```
NAMES                             STATUS
orders-walkthrough-wiremock       Up
orders-walkthrough-tempo          Up
orders-walkthrough-loki           Up
orders-walkthrough-mimir          Up
orders-walkthrough-otel-collector Up
orders-walkthrough-grafana        Up
```

## Service Endpoints

- **Wiremock**: http://localhost:8080 - Mock backend services
- **Grafana**: http://localhost:3000 - Observability dashboard
- **OTel Collector**: localhost:4317 - OTLP gRPC receiver
- **Tempo**: http://localhost:3200 - Trace API
- **Loki**: http://localhost:3100 - Log API
- **Mimir**: http://localhost:9009 - Metrics API

## Verifying the Setup

1. **Check Grafana**: Open http://localhost:3000
   - You should see the Grafana welcome page
   - No login required (anonymous access enabled)

2. **Verify Data Sources**: Go to Configuration → Data Sources
   - You should see Tempo, Loki, and Mimir configured

3. **Restart Your API**: The API needs to restart to connect to the OTel Collector:

```bash
# Stop the API (Ctrl+C)
go run .
```

The API will now send traces, logs, and metrics to the OTel Collector.

## What's Next

Now let's explore the observability features and see your API's telemetry data in Grafana.

[Next: Exploring Observability →]({{< ref "/walkthroughs/orders-rest/10-observability" >}})
