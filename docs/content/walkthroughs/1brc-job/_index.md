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
- **Test your job with minimal infrastructure before adding observability**
- Retrofit OpenTelemetry instrumentation (traces, metrics, logs) to working code
- Monitor your job in Grafana with distributed tracing
- Use Humus patterns for configuration, error handling, and graceful shutdown

This walkthrough follows a **practical development workflow**: get your business logic working first with minimal setup, then add the full observability stack later.

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

1. [Project Setup]({{< ref "01-project-setup" >}}) - Directory structure and dependencies
2. [Building a Basic Job]({{< ref "02-basic-job" >}}) - Core job structure with minimal config
3. [MinIO Integration]({{< ref "03-minio-integration" >}}) - S3 storage client and local MinIO setup
4. [1BRC Algorithm]({{< ref "04-1brc-algorithm" >}}) - Parsing and calculating statistics
5. [Running Without OTel]({{< ref "05-running-without-otel" >}}) - Test your job with minimal infrastructure
6. [Infrastructure Setup]({{< ref "06-infrastructure" >}}) - Adding the LGTM observability stack
7. [Adding Observability]({{< ref "07-observability" >}}) - Retrofitting traces, metrics, and logs
8. [Running and Monitoring]({{< ref "08-running-monitoring" >}}) - Execute and view telemetry in Grafana

## Source Code

The complete working example is located at:
```
github.com/z5labs/humus/example/job/1brc-walkthrough
```

## Next Steps

Begin with [Project Setup]({{< ref "01-project-setup" >}}) to understand the code structure.
