---
title: Infrastructure & Observability
description: Set up Podman Compose with observability stack and Wiremock
weight: 9
type: docs
slug: infrastructure
---

Now that we have a working API, let's add infrastructure for mocking backend services and advanced observability with traces, metrics, and logs.

## Podman Compose Configuration

Create `podman-compose.yaml`:

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

## Supporting Configuration Files

You'll also need configuration files for Tempo, Mimir, OTel Collector, and Grafana datasources. These are available in the example directory:

- `tempo.yaml` - Tempo trace storage configuration
- `mimir.yaml` - Mimir metrics storage configuration
- `otel-collector-config.yaml` - Routes telemetry to backends
- `grafana-datasources.yaml` - Configures Grafana to read from all backends

## Starting the Infrastructure

```bash
podman-compose up -d
```

Verify all services are running:

```bash
podman ps
```

You should see 6 containers: wiremock, tempo, loki, mimir, otel-collector, and grafana.

## Service Ports

- **Wiremock**: http://localhost:8080 - Mock backend services
- **Grafana**: http://localhost:3000 - Observability dashboard
- **OTel Collector**: localhost:4317 - OTLP gRPC receiver
- **Tempo**: http://localhost:3200 - Trace API
- **Loki**: http://localhost:3100 - Log API
- **Mimir**: http://localhost:9009 - Metrics API

## What's Next

Now let's explore the observability features and see our traces, logs, and metrics in Grafana.

[Next: Exploring Traces & Metrics →]({{< ref "/walkthroughs/orders-rest/10-observability" >}})
