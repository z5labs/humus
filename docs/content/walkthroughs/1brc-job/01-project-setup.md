---
title: Project Setup
description: Create the directory structure and understand the project layout
weight: 1
type: docs
---

Let's start by creating the project structure for our 1BRC job.

## Directory Structure

Create the following directory structure:

```bash
mkdir -p 1brc-walkthrough/{app,service,onebrc,tool}
cd 1brc-walkthrough
```

The final structure will be:

```
1brc-walkthrough/
├── main.go                     # Entry point
├── config.yaml                 # Application configuration
├── go.mod                      # Module definition
├── app/
│   └── app.go                 # Job initialization and config
├── onebrc/
│   ├── handler.go             # Job orchestration
│   ├── parser.go              # Parse "city;temp" format
│   └── calculator.go          # Compute statistics
├── service/
│   └── minio.go               # MinIO/S3 storage abstraction
├── tool/
│   └── main.go                # Generate test data
├── podman-compose.yaml         # Infrastructure orchestration
├── tempo-config.yaml           # Tempo trace backend config
├── mimir-config.yaml           # Mimir metrics backend config
├── loki-config.yaml            # Loki logs backend config
├── otel-collector-config.yaml  # OpenTelemetry collector config
└── grafana-datasources.yaml    # Grafana datasource definitions
```

## Initialize Go Module

Create `go.mod`:

```go
module 1brc-walkthrough

go 1.24.0

require github.com/z5labs/humus v0.20.2
```

## Package Organization

Each package has a specific responsibility:

- **app/** - Job initialization and configuration
- **onebrc/** - Core business logic: orchestration, parsing, and calculation
- **service/** - Storage abstraction layer (MinIO/S3 client)
- **tool/** - Standalone utility to generate test data

The infrastructure YAML files will be created in later sections as we add observability.

## What's Next

In the next section, we'll build a basic "hello world" job to verify everything works.

[Next: Building a Basic Job →]({{< ref "02-basic-job" >}})
