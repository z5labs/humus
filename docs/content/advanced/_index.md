---
title: Advanced Topics
description: Deep dives into Humus internals and patterns
weight: 4
type: docs
---


This section covers advanced patterns, customizations, and deep dives into Humus internals.

## Topics Covered

- [Builder + Runner Pattern]({{< ref "builder-runner-pattern" >}}) - Deep dive into the core architecture
- [Custom Health Monitors]({{< ref "custom-health-monitors" >}}) - Building custom health checks
- [OTel Integration]({{< ref "otel-integration" >}}) - Advanced OpenTelemetry configuration
- [Testing]({{< ref "testing" >}}) - Testing patterns for Humus applications
- [Multi-Source Config]({{< ref "multi-source-config" >}}) - Advanced configuration composition
- [Concurrent Utilities]({{< ref "concurrent-utilities" >}}) - Thread-safe utilities

## Prerequisites

Before diving into advanced topics, ensure you're familiar with:

- [Getting Started]({{< ref "/getting-started" >}})
- [Core Concepts]({{< ref "/concepts" >}})
- At least one service type ([REST]({{< ref "/features/rest" >}}), [gRPC]({{< ref "/features/grpc" >}}), or [Job]({{< ref "/features/job" >}}))

## When to Use Advanced Topics

These topics are useful when:

- You need to customize framework behavior
- You're building reusable components
- You want to understand how Humus works internally
- You need advanced configuration strategies
- You're implementing custom patterns

## Next Steps

Choose a topic based on your needs, or start with [Builder + Runner Pattern]({{< ref "builder-runner-pattern" >}}) to understand the core architecture.
