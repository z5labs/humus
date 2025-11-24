---
title: MinIO Integration
description: Building the S3 storage client and running MinIO locally
weight: 3
type: docs
---

Let's add storage capabilities using MinIO (S3-compatible storage).

## Starting MinIO Locally

First, we need MinIO running. Create a minimal `podman-compose.yaml`:

```yaml
services:
  minio:
    image: docker.io/minio/minio:RELEASE.2024-11-07T00-52-20Z
    command: server /data --console-address ":9001"
    environment:
      - MINIO_ROOT_USER=minioadmin
      - MINIO_ROOT_PASSWORD=minioadmin
    ports:
      - "9000:9000"   # API
      - "9001:9001"   # Console
    volumes:
      - minio-data:/data:z

volumes:
  minio-data:
```

Start MinIO:

```bash
podman-compose up -d
```

Verify it's running:

```bash
podman ps
```

You should see the minio container running. Access the console at http://localhost:9001 (login: minioadmin/minioadmin).

## Add Configuration

Update `config.yaml` to add MinIO settings:

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

Update `app/app.go` to add the config fields:

```go
package app

import (
	"context"

	"1brc-walkthrough/onebrc"
	"1brc-walkthrough/service"
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

func Init(ctx context.Context, cfg Config) (*job.App, error) {
	// Create MinIO client
	minioClient, err := service.NewMinIOClient(
		cfg.Minio.Endpoint,
		cfg.Minio.AccessKey,
		cfg.Minio.SecretKey,
	)
	if err != nil {
		return nil, err
	}

	// Create handler with MinIO client
	handler := onebrc.NewHandler(
		minioClient,
		cfg.Minio.Bucket,
		cfg.OneBRC.InputKey,
		cfg.OneBRC.OutputKey,
	)

	return job.NewApp(handler), nil
}
```

## Service Client Implementation

Create `service/minio.go`:

```go
package service

import (
	"context"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MinIOClient struct {
	mc *minio.Client
}

func NewMinIOClient(endpoint, accessKey, secretKey string) (*MinIOClient, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,  // Use HTTP for local development
	})
	if err != nil {
		return nil, err
	}

	return &MinIOClient{mc: mc}, nil
}

func (c *MinIOClient) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return c.mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
}

func (c *MinIOClient) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64) error {
	_, err := c.mc.PutObject(ctx, bucket, key, reader, size, minio.PutObjectOptions{})
	return err
}
```

## Key Design Decisions

**Wrapper pattern:**
- Hides MinIO-specific details
- Makes testing easier (mock the interface)
- Provides only needed methods

**Streaming I/O:**
- `GetObject` returns `io.ReadCloser` for streaming reads
- `PutObject` accepts `io.Reader` to stream uploads
- No buffering of entire files in memory

**Context propagation:**
- All methods accept `context.Context`
- Enables cancellation and timeout
- Ready for trace spans (we'll add later)

## Update Handler

Update `onebrc/handler.go` to accept storage dependencies:

```go
package onebrc

import (
	"context"
	"io"
	"log/slog"
	"os"
)

type Storage interface {
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64) error
}

type Handler struct {
	storage   Storage
	bucket    string
	inputKey  string
	outputKey string
	log       *slog.Logger
}

func NewHandler(storage Storage, bucket, inputKey, outputKey string) *Handler {
	return &Handler{
		storage:   storage,
		bucket:    bucket,
		inputKey:  inputKey,
		outputKey: outputKey,
		log:       slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

func (h *Handler) Handle(ctx context.Context) error {
	h.log.InfoContext(ctx, "Job starting",
		slog.String("bucket", h.bucket),
		slog.String("input_key", h.inputKey),
	)

	// We'll implement the actual logic in the next section
	// For now, just log that we have MinIO connectivity

	h.log.InfoContext(ctx, "MinIO client initialized successfully")
	h.log.InfoContext(ctx, "Job completed successfully")
	return nil
}
```

## Run It

```bash
go mod tidy
go run .
```

You should see:

```json
{"time":"...","level":"INFO","msg":"Job starting","bucket":"onebrc","input_key":"measurements.txt"}
{"time":"...","level":"INFO","msg":"MinIO client initialized successfully"}
{"time":"...","level":"INFO","msg":"Job completed successfully"}
```

Your job now has MinIO integration! In the next section, we'll implement the actual 1BRC parsing and calculation logic.

## What's Next

Now we'll implement the core 1BRC algorithm for parsing and calculating statistics.

[Next: 1BRC Algorithm →]({{< ref "04-1brc-algorithm" >}})
