---
title: Building a Basic Job
description: Understanding the Config struct and Init function
weight: 2
type: docs
---

Let's build the core of your Humus job - just the essentials to get started.

## The Config Struct

Create `app/app.go`:

```go
package app

import (
	"context"

	"github.com/z5labs/humus/job"
)

type Config struct {
	Minio struct {
		Endpoint  string `config:"endpoint"`
		AccessKey string `config:"access_key"`
		SecretKey string `config:"secret_key"`
		Bucket    string `config:"bucket"`
	} `config:"minio"`

	OneBRC struct {
		InputKey  string `config:"input_key"`
		OutputKey string `config:"output_key"`
	} `config:"onebrc"`
}
```

**Key points:**
- Custom fields map to `config.yaml`
- No OpenTelemetry configuration yet - we'll add that later

## Minimal Configuration File

Create `config.yaml`:

```yaml
minio:
  endpoint: {{env "MINIO_ENDPOINT" | default "localhost:9000"}}
  access_key: {{env "MINIO_ACCESS_KEY" | default "minioadmin"}}
  secret_key: {{env "MINIO_SECRET_KEY" | default "minioadmin"}}
  bucket: {{env "MINIO_BUCKET" | default "onebrc"}}

onebrc:
  input_key: {{env "INPUT_KEY" | default "measurements.txt"}}
  output_key: {{env "OUTPUT_KEY" | default "results.txt"}}
```

This minimal config just sets up MinIO connectivity. The `{{env "VAR" | default "value"}}` syntax uses Go templating to read environment variables with fallbacks.

## The Init Function

Add this to `app/app.go`:

```go
func Init(ctx context.Context, cfg Config) (*job.App, error) {
	// 1. Create dependencies (we'll add MinIO client in next section)
	// minioClient, err := storage.NewClient(...)

	// 2. Build handler (we'll add this after MinIO integration)
	// handler := onebrc.NewHandler(...)

	// 3. Return job
	return job.NewApp(nil), nil  // nil handler for now
}
```

**Responsibilities:**
1. Dependency injection (create clients)
2. Handler construction
3. Error handling

**DON'T:**
- ❌ Start goroutines
- ❌ Call `Run()` on the handler
- ❌ Initialize OTel manually (Humus does this automatically)

## The Handler Interface

Your business logic will implement:

```go
type Handler interface {
	Handle(context.Context) error
}
```

## Entry Point

Create `main.go`:

```go
package main

import (
	"bytes"
	_ "embed"

	"1brc-walkthrough/app"
	"github.com/z5labs/humus/job"
)

//go:embed config.yaml
var configBytes []byte

func main() {
	job.Run(bytes.NewReader(configBytes), app.Init)
}
```

Key points:
- `//go:embed` embeds config.yaml at compile time
- `job.Run()` handles server lifecycle and graceful shutdown
- Logs go to stdout by default (no external infrastructure needed yet)

## How job.Run Works

When you call `job.Run(configReader, initFunc)`:

1. Parses config (YAML templates → struct)
2. Calls your `Init` function
3. Wraps with middleware (panic recovery, signals)
4. Runs `handler.Handle(ctx)`
5. Graceful shutdown

## What's Next

Now we'll add MinIO integration to read and write files.

[Next: MinIO Integration →]({{< ref "03-minio-integration" >}})
