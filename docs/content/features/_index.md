---
title: Features
description: Service types and capabilities
weight: 3
type: docs
---

# Features

Humus provides three distinct service types, each optimized for different use cases.

## Service Types

### [REST/HTTP Services]({{< ref "rest" >}})

Build OpenAPI-compliant HTTP APIs with automatic schema generation and type-safe handlers.

**Best For:**
- Web APIs and microservices
- Public-facing services
- Services requiring OpenAPI documentation

**Key Features:**
- Automatic OpenAPI 3.0 generation
- Built-in health endpoints
- Type-safe request/response handling
- Path parameter and query validation

[Get Started with REST →]({{< ref "rest" >}})

### [gRPC Services]({{< ref "grpc" >}})

Create high-performance gRPC microservices with automatic instrumentation and health checks.

**Best For:**
- Internal microservices
- High-performance service-to-service communication
- Services with strongly-typed contracts

**Key Features:**
- Automatic gRPC health service
- Built-in interceptors for OTel
- Service registration
- Protocol Buffers support

[Get Started with gRPC →]({{< ref "grpc" >}})

### [Job Services]({{< ref "job" >}})

Build one-off job executors for batch processing, migrations, and scheduled tasks.

**Best For:**
- Database migrations
- Batch processing
- Scheduled tasks (with external scheduler)
- One-time operations

**Key Features:**
- Simple Handler interface
- Full observability support
- Same lifecycle management as long-running services
- Context-aware execution

[Get Started with Jobs →]({{< ref "job" >}})

## Common Features

All service types include:

- **OpenTelemetry Integration** - Automatic traces, metrics, and logs
- **Graceful Shutdown** - Clean shutdown on SIGTERM/SIGINT
- **Configuration Management** - YAML with template support
- **Panic Recovery** - Automatic panic recovery in handlers
- **Lifecycle Management** - Managed by Bedrock framework