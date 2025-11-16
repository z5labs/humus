---
title: Infrastructure Setup
description: Running the observability stack with Podman Compose
weight: 2
type: docs
---

# Infrastructure Setup

The 1BRC walkthrough includes a complete observability stack using Podman Compose.

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

## Services

### MinIO
**S3-compatible object storage** for input data and results.
- **API Port:** 9000
- **Console:** http://localhost:9001 (minioadmin/minioadmin)
- **Usage:** Store `measurements.txt` and `results.txt`

### OpenTelemetry Collector
**Central telemetry hub** that receives OTLP signals and routes them to backends.
- **OTLP gRPC:** localhost:4317 (your job sends here)
- **OTLP HTTP:** localhost:4318
- **Config:** `otel-collector-config.yaml`

### Grafana Tempo
**Distributed tracing backend** stores and queries traces.
- **HTTP:** localhost:3200
- **Usage:** View trace spans, waterfall charts, trace search

### Grafana Mimir
**Prometheus-compatible metrics storage**.
- **HTTP:** localhost:8080
- **Usage:** Query custom metrics like `onebrc_cities_count`

### Grafana Loki
**Log aggregation system** for structured logs.
- **HTTP:** localhost:3100
- **Usage:** Query logs by service, trace ID, or any label

### Grafana
**Unified observability UI** to visualize all signals.
- **Web UI:** http://localhost:3000
- **Auth:** Anonymous (disabled login, auto-admin)
- **Features:** TraceQL editor, trace-log correlation
- **Datasources:** Pre-configured for Tempo, Mimir, Loki

## Starting the Stack

Navigate to the example directory and start the services:

```bash
cd example/job/1brc-walkthrough
podman-compose up -d
```

**First startup takes 1-2 minutes** to pull images and initialize.

Check status:
```bash
podman-compose ps
```

All services should show `Up` status.

## Accessing Services

| Service | URL | Credentials |
|---------|-----|-------------|
| Grafana | http://localhost:3000 | None (anonymous admin) |
| MinIO Console | http://localhost:9001 | minioadmin / minioadmin |
| Tempo | http://localhost:3200 | N/A (internal) |
| Mimir | http://localhost:8080 | N/A (internal) |
| Loki | http://localhost:3100 | N/A (internal) |

## Stopping the Stack

```bash
# Stop containers
podman-compose down

# Stop and remove volumes (delete all data)
podman-compose down -v
```

## Troubleshooting

**Container won't start:**
```bash
podman logs <container-name>
podman logs otel-collector
```

**Port conflicts:**
Edit `podman-compose.yaml` and change the left side of port mappings:
```yaml
ports:
  - "9000:9000"  # Change 9000 to another port like 9090:9000
```

## Next Steps

Continue to: [Building a Basic Job]({{< ref "basic-job" >}})
