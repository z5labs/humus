---
title: 1BRC Job Walkthrough
description: Build a production job to solve the One Billion Row Challenge
weight: 1
type: docs
---

## Overview

This walkthrough teaches you how to build a production-ready job application using Humus to solve the [One Billion Row Challenge](https://github.com/gunnarmorling/1brc). You'll learn how to:

- Build a job that processes large datasets from S3-compatible storage
- Parse and aggregate 1 billion temperature measurements
- Automatically instrument your code with OpenTelemetry (traces, metrics, logs)
- Monitor your job in Grafana with distributed tracing
- Use Humus patterns for configuration, error handling, and graceful shutdown

## What You'll Build

A complete job application that:
1. Fetches temperature measurement data from MinIO (S3-compatible storage)
2. Parses data in the format `city;temperature`
3. Calculates min/mean/max statistics per city
4. Writes formatted results back to MinIO
5. Exports telemetry to Tempo (traces), Mimir (metrics), and Loki (logs)

**Input:** 1 billion lines like `Tokyo;35.6\nJakarta;-6.2\n...`  
**Output:** One line per city: `Jakarta=-10.0/26.5/45.3\nTokyo=-5.2/35.6/50.1\n...`

## Prerequisites

- **Go 1.24+** installed
- **Podman** or Docker for running infrastructure
- Basic understanding of Go (contexts, interfaces, error handling)
- Familiarity with command-line tools

## What You'll Learn

### Humus Framework Patterns
- **Builder + Runner pattern:** How Humus composes apps with middleware
- **Config embedding:** Using `job.Config` with custom configuration
- **Automatic OTel:** Zero-manual-setup observability
- **Graceful shutdown:** OS signal handling and resource cleanup

### OpenTelemetry Integration
- Creating manual spans for fine-grained tracing
- Recording custom metrics
- Structured logging with trace correlation
- Viewing distributed traces in Grafana

### Job Architecture
- Separating concerns (storage, parsing, calculation, orchestration)
- Streaming large files without loading into memory
- Error handling and context propagation

## Time Estimate

- **Setup:** 10 minutes
- **Code walkthrough:** 30-45 minutes
- **Running and monitoring:** 15 minutes

## Walkthrough Sections

1. [Project Setup]({{< ref "project-setup" >}}) - Directory structure and dependencies
2. [Infrastructure Setup]({{< ref "infrastructure" >}}) - Running the observability stack
3. [Building a Basic Job]({{< ref "basic-job" >}}) - Core job structure
4. [MinIO Integration]({{< ref "minio-integration" >}}) - S3 storage client
5. [1BRC Algorithm]({{< ref "1brc-algorithm" >}}) - Parsing and calculating statistics
6. [Observability]({{< ref "observability" >}}) - Adding traces, metrics, and logs
7. [Running and Monitoring]({{< ref "running-monitoring" >}}) - Execute and view telemetry

## Source Code

The complete working example is located at:
```
github.com/z5labs/humus/example/job/1brc-walkthrough
```

## Next Steps

Begin with [Project Setup]({{< ref "project-setup" >}}) to understand the code structure.
