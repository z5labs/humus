---
title: API Reference
description: Quick reference for Humus packages
weight: 5
type: docs
---


Quick reference guides for Humus packages. For complete API documentation, see [pkg.go.dev/github.com/z5labs/humus](https://pkg.go.dev/github.com/z5labs/humus).

## Package References

- [REST Package]({{< ref "rest-package" >}}) - HTTP services and routing
- [gRPC Package]({{< ref "grpc-package" >}}) - gRPC services
- [Job Package]({{< ref "job-package" >}}) - Job services
- [Health Package]({{< ref "health-package" >}}) - Health monitoring
- [Config Package]({{< ref "config-package" >}}) - Configuration types

## External Documentation

- [Go Package Documentation](https://pkg.go.dev/github.com/z5labs/humus) - Complete API reference
- [Bedrock Documentation](https://pkg.go.dev/github.com/z5labs/bedrock) - Foundation framework
- [OpenTelemetry Go](https://pkg.go.dev/go.opentelemetry.io/otel) - Observability SDK
- [chi Router](https://pkg.go.dev/github.com/go-chi/chi/v5) - HTTP router (used by REST)
- [gRPC-Go](https://pkg.go.dev/google.golang.org/grpc) - gRPC framework

## Quick Links

### REST Services
- [rest.NewApi](https://pkg.go.dev/github.com/z5labs/humus/rest#NewApi)
- [rest.Handle](https://pkg.go.dev/github.com/z5labs/humus/rest#Handle)
- [rpc.NewOperation](https://pkg.go.dev/github.com/z5labs/humus/rest/rpc#NewOperation)
- [rpc.Handle](https://pkg.go.dev/github.com/z5labs/humus/rest/rpc#Handle)

### gRPC Services
- [grpc.NewApi](https://pkg.go.dev/github.com/z5labs/humus/grpc#NewApi)
- [grpc.Run](https://pkg.go.dev/github.com/z5labs/humus/grpc#Run)

### Job Services
- [job.Run](https://pkg.go.dev/github.com/z5labs/humus/job#Run)
- [job.Handler](https://pkg.go.dev/github.com/z5labs/humus/job#Handler)

### Health Monitoring
- [health.Monitor](https://pkg.go.dev/github.com/z5labs/humus/health#Monitor)
- [health.Binary](https://pkg.go.dev/github.com/z5labs/humus/health#Binary)
- [health.AndMonitor](https://pkg.go.dev/github.com/z5labs/humus/health#AndMonitor)
- [health.OrMonitor](https://pkg.go.dev/github.com/z5labs/humus/health#OrMonitor)
