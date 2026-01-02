---
title: gRPC Services
description: Building high-performance microservices
weight: 20
type: docs
---


Humus gRPC services provide a complete framework for building high-performance microservices with automatic instrumentation, health checks, and seamless Protocol Buffers integration.

## Overview

gRPC services in Humus are built on:

- **[gRPC-Go](https://github.com/grpc/grpc-go)** - Official gRPC implementation
- **Automatic Health Service** - gRPC health checking protocol
- **OpenTelemetry Interceptors** - Built-in tracing and metrics
- **Service Registration** - Simple API for registering services

## Quick Start

```go
package main

import (
    "context"
    "net"

    "github.com/z5labs/humus/app"
    "github.com/z5labs/humus/config"
    "github.com/z5labs/humus/grpc"
    "github.com/z5labs/humus/otel"
    pb "your-module/gen/proto/user"
    
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/trace"
)

type userService struct {
    pb.UnimplementedUserServiceServer
}

func (s *userService) GetUser(ctx context.Context, req *pb.GetUserRequest) (*pb.User, error) {
    return &pb.User{
        Id:    req.Id,
        Name:  "John Doe",
        Email: "john@example.com",
    }, nil
}

func main() {
    // Configure listener
    listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
        addr := config.MustOr(ctx, ":9090", config.Env("GRPC_ADDR"))
        ln, err := net.Listen("tcp", addr)
        if err != nil {
            return config.Value[net.Listener]{}, err
        }
        return config.ValueOf(ln), nil
    })

    // Build API and register services
    api := grpc.NewApi()
    pb.RegisterUserServiceServer(api, &userService{})

    // Build gRPC application
    grpcBuilder := grpc.Build(listener, api)

    // Configure OpenTelemetry (disabled for simplicity)
    sdk := otel.SDK{
        TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
            return config.Value[trace.TracerProvider]{}, nil
        }),
        MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
            return config.Value[metric.MeterProvider]{}, nil
        }),
    }

    otelBuilder := otel.Build(sdk, grpcBuilder)

    _ = app.Run(context.Background(), otelBuilder)
}
```

## Core Components

### grpc.Api

The main API object that:
- Implements `grpc.ServiceRegistrar`
- Manages interceptors for OTel
- Automatically registers health service
- Monitors registered services

### Automatic Features

Every gRPC service gets:

- **Health Service** - Implements `grpc.health.v1.Health` protocol
- **Tracing** - Automatic span creation for RPCs
- **Metrics** - Request count, duration, status
- **Service Monitoring** - Health checks for services implementing `health.Monitor`

## Built-in Health Service

The gRPC health service is automatically registered and provides:

- `/grpc.health.v1.Health/Check` - Check service health
- `/grpc.health.v1.Health/Watch` - Stream health updates

No configuration needed - it works out of the box.

## What You'll Learn

This section covers:

- [Quick Start]({{< ref "quick-start" >}}) - Build your first gRPC service
- [gRPC API]({{< ref "grpc-api" >}}) - Understanding grpc.Api
- [Health Service]({{< ref "health-service" >}}) - Health checking protocol
- [Interceptors]({{< ref "interceptors" >}}) - OTel instrumentation
- [Petstore Example]({{< ref "petstore-example" >}}) - Complete walkthrough

## Next Steps

Start with the [Quick Start Guide]({{< ref "quick-start" >}}) to build your first gRPC service.