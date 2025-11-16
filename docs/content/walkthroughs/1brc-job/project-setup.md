---
title: Project Setup
description: Understanding the project structure and dependencies
weight: 1
type: docs
---

## Directory Structure

The 1BRC walkthrough follows a standard Go project layout:

```
1brc-walkthrough/
├── main.go                  # Entry point
├── config.yaml              # Configuration with OTel settings
├── app/
│   └── app.go              # Config struct and Init function
├── storage/
│   └── minio.go            # MinIO S3 client wrapper
├── onebrc/
│   ├── handler.go          # Job orchestration + OTel instrumentation
│   ├── parser.go           # Parse "city;temp" format
│   └── calculator.go       # Compute statistics
└── tool/
    └── main.go             # Generate test data
```

## Key Files

### Entry Point: `main.go`

The entry point is minimal - it embeds the config and calls `job.Run`:

```go
package main

import (
	"bytes"
	_ "embed"

	"github.com/z5labs/humus/example/job/1brc-walkthrough/app"
	"github.com/z5labs/humus/job"
)

//go:embed config.yaml
var configBytes []byte

func main() {
	job.Run(bytes.NewReader(configBytes), app.Init)
}
```

**What's happening:**
- `//go:embed` embeds the config file at compile time
- `job.Run()` handles everything: config parsing, app initialization, OTel setup, graceful shutdown
- `app.Init` is your custom function that builds the job

### Dependencies

The project uses these key dependencies (already in the root `go.mod`):

```go
require (
	github.com/z5labs/humus v0.20.2          // Job framework
	github.com/minio/minio-go/v7 v7.0.97     // S3 client
	go.opentelemetry.io/otel v1.38.0         // OTel API
)
```

**No manual SDK initialization required** - Humus handles OTel setup automatically.

## Package Organization

### The `app` Package
- Defines the custom `Config` struct (embeds `job.Config`)
- Implements the `Init` function that builds the job

### The `storage` Package
- Wraps the MinIO client for S3 operations
- Provides `GetObject` and `PutObject` methods

### The `onebrc` Package
- **handler.go:** Orchestrates the workflow (fetch → parse → calculate → write)
- **parser.go:** Reads and aggregates temperature data
- **calculator.go:** Computes final statistics and formats output

### The `tool` Package
- Standalone program to generate test data
- Uses real weather stations from the 1BRC repository
- Supports concurrent generation for speed

## Configuration Philosophy

Humus uses **config embedding** for composition:

```go
type Config struct {
	job.Config `config:",squash"`  // Base config with OTel
	
	Minio struct {
		Endpoint  string `config:"endpoint"`
		// ... more fields
	} `config:"minio"`
}
```

The `` `config:",squash"` `` tag flattens the embedded config, so `otel.service.name` in YAML maps to `Config.job.Config.OTel.Service.Name`.

## YAML Templates

All config files support Go templates with helper functions:

```yaml
endpoint: "{{env "MINIO_ENDPOINT" | default "localhost:9000"}}"
```

**Available functions:**
- `env "VAR"` - read environment variable
- `default "value"` - provide fallback if empty

## Next Steps

Continue to: [Infrastructure Setup]({{< ref "infrastructure" >}})
