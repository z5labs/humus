---
title: Infrastructure Setup
description: Adding the LGTM observability stack
weight: 6
type: docs
---

Your job is working with MinIO! Now let's add the full observability stack to see traces, logs, and metrics in Grafana.

## Understanding the LGTM Stack

The LGTM stack provides comprehensive observability:

- **Loki** - Log aggregation and querying
- **Grafana** - Unified visualization dashboard
- **Tempo** - Distributed tracing backend
- **Mimir** - Long-term metrics storage

We'll also add the **OpenTelemetry Collector** to receive and route telemetry data from your job.

## Architecture Overview

```
┌─────────────┐
│  Your Job   │ ──OTLP──▶ ┌──────────────────┐
└─────────────┘           │ OTel Collector   │
                          └──────────────────┘
                                   │
                    ┌──────────────┼──────────────┐
                    ▼              ▼              ▼
              ┌─────────┐    ┌─────────┐    ┌─────────┐
              │  Tempo  │    │  Mimir  │    │  Loki   │
              │ (traces)│    │(metrics)│    │  (logs) │
              └─────────┘    └─────────┘    └─────────┘
                    │              │              │
                    └──────────────┼──────────────┘
                                   ▼
                            ┌──────────────┐
                            │   Grafana    │
                            │ (visualize)  │
                            └──────────────┘

┌─────────────┐
│   MinIO     │ ◀───────── Data storage (input/output)
└─────────────┘
```

## Updating Podman Compose

Update your `podman-compose.yaml` to add the observability stack (keeping MinIO):

```yaml
services:
  minio:
    image: docker.io/minio/minio:RELEASE.2024-11-07T00-52-20Z
    command: server /data --console-address ":9001"
    environment:
      - MINIO_ROOT_USER=minioadmin
      - MINIO_ROOT_PASSWORD=minioadmin
    ports:
      - "9000:9000"
      - "9001:9001"
    volumes:
      - minio-data:/data:z

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

volumes:
  minio-data:
```

## Required Configuration Files

You'll need these configuration files in your project directory. These are available in the example directory at `example/job/1brc-walkthrough/`.

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

Routes telemetry from your job to the backend services:

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

## Starting the Stack

Start all services:

```bash
podman-compose up -d
```

**First startup takes 1-2 minutes** to pull images and initialize.

Check status:

```bash
podman-compose ps
```

All 6 services should show `Up` status.

## Service Endpoints

| Service | URL | Credentials |
|---------|-----|-------------|
| Grafana | http://localhost:3000 | None (anonymous admin) |
| MinIO Console | http://localhost:9001 | minioadmin / minioadmin |
| Tempo | http://localhost:3200 | N/A (internal) |
| Mimir | http://localhost:9009 | N/A (internal) |
| Loki | http://localhost:3100 | N/A (internal) |
| OTel Collector | localhost:4317 | N/A (OTLP gRPC) |

## Verifying the Setup

1. **Check Grafana**: Open http://localhost:3000
   - You should see the Grafana welcome page
   - No login required (anonymous access enabled)

2. **Verify Data Sources**: Go to Configuration → Data Sources
   - You should see Tempo, Loki, and Mimir configured

3. **Regenerate test data** (since we restarted MinIO):

```bash
cd tool
go run . -count 10000
cd ..
```

## What's Next

Now let's add OpenTelemetry instrumentation to your job so it sends traces, metrics, and logs to this stack.

[Next: Adding Observability →]({{< ref "07-observability" >}})
