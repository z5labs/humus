---
title: Installation
description: Installing Humus and dependencies
weight: 10
type: docs
---


## Installing Humus

Humus is installed as a Go module dependency. Add it to your project using `go get`:

```bash
go get github.com/z5labs/humus
```

This will download Humus and all its dependencies, including:

- [Bedrock](https://github.com/z5labs/bedrock) - Core application lifecycle framework
- OpenTelemetry SDK - For observability
- Service-specific dependencies (chi router for REST, gRPC for gRPC services, etc.)

## Verifying Installation

Create a simple `main.go` file to verify the installation:

```go
package main

import (
    "fmt"

    "github.com/z5labs/humus/rest"
)

func main() {
    fmt.Println("Humus installed successfully!")
}
```

Run it:

```bash
go run main.go
```

If you see "Humus installed successfully!", you're ready to go!

## Dependency Management

Humus follows semantic versioning. To ensure reproducible builds, use Go modules:

```bash
# Initialize a new module (if not already done)
go mod init your-module-name

# Install Humus
go get github.com/z5labs/humus

# Tidy up dependencies
go mod tidy
```

## Version Pinning

To pin to a specific version:

```bash
# Install a specific version
go get github.com/z5labs/humus@v0.1.0

# Or use the latest patch release
go get github.com/z5labs/humus@latest
```

## Service-Specific Dependencies

Depending on which service type you're building, you may need additional tools:

### For REST Services
No additional dependencies required - everything is included with Humus.

### For gRPC Services
You'll need the Protocol Buffers compiler and Go plugins:

```bash
# Install protoc (see https://grpc.io/docs/protoc-installation/)
# On macOS:
brew install protobuf

# On Linux:
apt install -y protobuf-compiler

# Install Go plugins
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

### For Job Services
No additional dependencies required.

## Development Tools

While not required, these tools are recommended for development:

```bash
# golangci-lint for code quality
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Air for hot reloading during development
go install github.com/air-verse/air@latest
```

## Next Steps

Now that Humus is installed, continue to [Your First Service]({{< ref "first-service" >}}) to build your first application.
