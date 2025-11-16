---
title: Getting Started
description: Get up and running with Humus
weight: 1
type: docs
---


This guide will help you get started with Humus, from installation to building your first service.

## Prerequisites

Before you begin, ensure you have:

- **Go 1.21 or later** installed on your system
- Basic familiarity with Go programming
- Understanding of REST APIs, gRPC, or batch processing (depending on your use case)

## What You'll Learn

This section covers:

- [Installation]({{< ref "installation" >}}) - Installing Humus and setting up your environment
- [Your First Service]({{< ref "first-service" >}}) - Building a Hello World REST service
- [Configuration]({{< ref "configuration" >}}) - Understanding the YAML configuration system
- [Project Structure]({{< ref "project-structure" >}}) - Recommended project layout patterns

## Quick Start

If you want to jump right in:

```bash
# Install Humus
go get github.com/z5labs/humus

# Create a new project
mkdir my-service && cd my-service
go mod init my-service
```

Then follow the [First Service]({{< ref "first-service" >}}) guide to build your first application.

## Next Steps

Once you're comfortable with the basics, explore:

- [Core Concepts]({{< ref "/concepts" >}}) - Understand Humus architecture
- [REST Services]({{< ref "/features/rest" >}}) - Build HTTP APIs
- [gRPC Services]({{< ref "/features/grpc" >}}) - Build gRPC microservices
- [Job Services]({{< ref "/features/job" >}}) - Build batch processors
