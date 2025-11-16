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

    "github.com/z5labs/humus/grpc"
    pb "your-module/gen/proto/user"
)

type Config struct {
    grpc.Config `config:",squash"`
}

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
    grpc.Run(grpc.YamlSource("config.yaml"), Init)
}

func Init(ctx context.Context, cfg Config) (*grpc.Api, error) {
    api := grpc.NewApi()

    // Register your service
    pb.RegisterUserServiceServer(api, &userService{})

    return api, nil
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