---
description: 'Patterns and best practices for gRPC services using Humus'
applyTo: '**/*.go'
---

# Humus Framework - gRPC Service Instructions

This file provides patterns and best practices specific to gRPC services using Humus. Use this file alongside `humus-common.instructions.md` for complete guidance.

## Project Structure

```
my-grpc-service/
├── main.go
├── config.yaml
├── app/
│   └── app.go          # Init function
├── pet/                # Domain package
│   └── registrar/
│       └── registrar.go  # Service registration
├── proto/              # Proto definitions
│   └── pet.proto
├── petpb/              # Generated protobuf code
│   ├── pet.pb.go
│   └── pet_grpc.pb.go
├── go.mod
└── go.sum
```

## gRPC Service Patterns

### Entry Point

**main.go:**
```go
package main

import (
    "bytes"
    _ "embed"
    "github.com/z5labs/humus/grpc"
    "my-grpc-service/app"
)

//go:embed config.yaml
var configBytes []byte

func main() {
    grpc.Run(bytes.NewReader(configBytes), app.Init)
}
```

### Init Function

**app/app.go:**
```go
package app

import (
    "context"
    "my-grpc-service/pet/registrar"
    "github.com/z5labs/humus/grpc"
)

type Config struct {
    grpc.Config `config:",squash"`
    // Add service-specific config here
}

func Init(ctx context.Context, cfg Config) (*grpc.Api, error) {
    api := grpc.NewApi()
    
    // Register your gRPC services
    registrar.Register(api, dependencies)
    
    return api, nil
}
```

### Service Registration

**pet/registrar/registrar.go:**
```go
package registrar

import (
    "context"
    "my-grpc-service/petpb"
    "github.com/z5labs/humus/grpc"
)

func Register(api *grpc.Api, store Store) {
    svc := &service{store: store}
    petpb.RegisterPetServiceServer(api, svc)
}

type service struct {
    petpb.UnimplementedPetServiceServer
    store Store
}

func (s *service) CreatePet(ctx context.Context, req *petpb.CreatePetRequest) (*petpb.Pet, error) {
    // Implementation
    return &petpb.Pet{}, nil
}

func (s *service) GetPet(ctx context.Context, req *petpb.GetPetRequest) (*petpb.Pet, error) {
    // Implementation
    return &petpb.Pet{}, nil
}
```

## Protocol Buffers

### Proto Definition

**proto/pet.proto:**
```proto
syntax = "proto3";

package pet;

option go_package = "my-grpc-service/petpb";

service PetService {
  rpc CreatePet(CreatePetRequest) returns (Pet);
  rpc GetPet(GetPetRequest) returns (Pet);
}

message CreatePetRequest {
  string name = 1;
  string species = 2;
}

message GetPetRequest {
  string id = 1;
}

message Pet {
  string id = 1;
  string name = 2;
  string species = 3;
}
```

### Code Generation

**Makefile:**
```makefile
.PHONY: proto
proto:
    protoc --go_out=. --go_opt=paths=source_relative \
           --go-grpc_out=. --go-grpc_opt=paths=source_relative \
           proto/**/*.proto
```

Run with:
```bash
make proto
```

## gRPC-Specific Best Practices

### DO ✅

1. **Organize services in domain packages** - one package per domain (e.g., pet/, user/)
2. **Use registrar pattern** - keep service registration separate from implementation
3. **Embed UnimplementedServer** - ensures forward compatibility with proto updates
4. **Generate code with protoc** - use Makefile for reproducible builds
5. **Pass dependencies to registrar** - keeps services testable and decoupled

### DON'T ❌

1. **Don't implement services directly in app.go** - use the registrar pattern
2. **Don't forget UnimplementedServer** - helps with forward compatibility
3. **Don't manually register health service** - Humus does this automatically
4. **Don't bypass the grpc.Api** - it provides automatic instrumentation
5. **Don't hardcode server addresses** - use configuration

## Health Service

The gRPC health service is automatically registered by Humus. You can use health monitors to control readiness:

```go
func Init(ctx context.Context, cfg Config) (*grpc.Api, error) {
    api := grpc.NewApi()
    
    // Create a health monitor
    dbMonitor := new(health.Binary)
    
    // Check database connection
    if err := checkDatabase(); err != nil {
        dbMonitor.MarkUnhealthy()
    } else {
        dbMonitor.MarkHealthy()
    }
    
    // Register with API (health endpoints use this)
    api.SetHealthMonitor(dbMonitor)
    
    return api, nil
}
```

## Example Project

Study this example in the Humus repository:

- **gRPC Service**: `example/grpc/petstore/` - gRPC with health monitoring

## Additional Resources

- **gRPC Documentation**: https://z5labs.dev/humus/features/grpc/
- **Protocol Buffers**: https://protobuf.dev/
- **gRPC Go**: https://grpc.io/docs/languages/go/
- **Common patterns**: See `humus-common.instructions.md`
