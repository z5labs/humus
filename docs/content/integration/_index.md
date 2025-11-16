---
title: Migration & Integration
description: Migrating to Humus and integrating with other systems
weight: 6
type: docs
---


Guides for migrating existing applications to Humus and integrating with other systems.

## Migration Guides

- [From Vanilla Go HTTP]({{< ref "from-vanilla-go" >}}) - Migrate from `net/http`
- [From chi Router]({{< ref "from-chi" >}}) - Migrate from chi-based applications
- [From gRPC-Go]({{< ref "from-grpc-go" >}}) - Migrate from vanilla gRPC applications
- [Bedrock Integration]({{< ref "bedrock-integration" >}}) - Deep dive into Bedrock framework

## Why Migrate to Humus?

### Consistency
- Standardized patterns across REST, gRPC, and Job services
- Common configuration format
- Unified observability approach

### Built-in Observability
- Automatic OpenTelemetry integration
- No manual instrumentation needed
- Consistent logging with trace correlation

### Production Ready
- Graceful shutdown out of the box
- Health check endpoints
- Panic recovery
- Signal handling

### Developer Experience
- Type-safe handlers (REST/RPC)
- Automatic OpenAPI generation
- Minimal boilerplate
- Clear separation of concerns

## Migration Strategy

1. **Assess Your Application** - Identify service type and dependencies
2. **Start Small** - Migrate one endpoint or service at a time
3. **Test Thoroughly** - Ensure behavior matches original
4. **Deploy Incrementally** - Use feature flags or canary deployments

## Compatibility

Humus is compatible with:

- **Existing HTTP Middleware** - chi middleware works with REST services
- **gRPC Interceptors** - Standard interceptors work alongside Humus interceptors
- **OpenTelemetry Collectors** - Any OTLP-compatible backend
- **Configuration Sources** - YAML files, environment variables, or custom sources

## Next Steps

Choose a migration guide based on your current stack, or explore [Bedrock Integration]({{< ref "bedrock-integration" >}}) to understand the foundation.
