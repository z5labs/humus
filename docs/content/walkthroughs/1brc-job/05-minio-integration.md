---
title: Refactoring to MinIO Storage
description: Replace local file I/O with S3-compatible cloud storage
weight: 5
type: docs
---

Your job works with local files! Now let's refactor it to use MinIO (S3-compatible storage) so you can process files in the cloud.

## Why Cloud Storage?

**Benefits of MinIO/S3:**
- Scalable storage for large datasets
- Data durability and redundancy
- Separation of compute and storage
- Production-ready architecture
- Works with any S3-compatible service (AWS S3, MinIO, Backblaze B2, etc.)

## Starting MinIO Locally

First, we need MinIO running. Create `podman-compose.yaml`:

```yaml
services:
  minio:
    image: docker.io/minio/minio:latest
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

## Add Storage Package

We'll create a new `service` package to abstract our storage operations.

First, create the service directory:

```bash
mkdir service
```

Then create `service/minio.go`:

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

## Update Dependencies

Update `go.mod` to add MinIO:

```go
module 1brc-walkthrough

go 1.24.0

require (
	github.com/z5labs/humus v0.20.2
	github.com/minio/minio-go/v7 v7.0.97
)
```

Run `go mod tidy` to download the dependency.

## Refactor Handler to Use Storage Interface

Update `onebrc/handler.go` to accept a storage interface instead of file paths:

```go
package onebrc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
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
	h.log.InfoContext(ctx, "starting 1BRC processing",
		slog.String("bucket", h.bucket),
		slog.String("input_key", h.inputKey),
		slog.String("output_key", h.outputKey),
	)

	// 1. Fetch from S3
	rc, err := h.storage.GetObject(ctx, h.bucket, h.inputKey)
	if err != nil {
		h.log.ErrorContext(ctx, "failed to fetch input object", slog.Any("error", err))
		return fmt.Errorf("get object: %w", err)
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil {
			h.log.WarnContext(ctx, "failed to close input object", slog.Any("error", cerr))
		}
	}()

	// 2. Parse
	cityStats, err := Parse(bufio.NewReader(rc))
	if err != nil {
		h.log.ErrorContext(ctx, "failed to parse temperature data", slog.Any("error", err))
		return fmt.Errorf("parse: %w", err)
	}

	// 3. Calculate
	results := Calculate(cityStats)

	// 4. Write results
	output := FormatResults(results)
	outputBytes := []byte(output)

	err = h.storage.PutObject(ctx, h.bucket, h.outputKey,
		bytes.NewReader(outputBytes), int64(len(outputBytes)))
	if err != nil {
		h.log.ErrorContext(ctx, "failed to upload results", slog.Any("error", err))
		return fmt.Errorf("put object: %w", err)
	}

	h.log.InfoContext(ctx, "1BRC processing completed successfully",
		slog.Int("cities_processed", len(cityStats)),
	)

	return nil
}
```

**Key changes:**
- Changed from file paths to `Storage` interface
- Uses `GetObject` instead of `os.Open`
- Uses `PutObject` instead of `os.WriteFile`
- Core parsing/calculation logic unchanged (that's the beauty of interfaces!)

## Update Configuration

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

Note: We changed `input_file`/`output_file` to `input_key`/`output_key` to reflect S3 terminology.

## Update App Initialization

Update `app/app.go` to create the MinIO client:

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

## Update Data Generation Tool

Update `tool/main.go` to upload directly to MinIO:

```go
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var cities = []string{
	"Tokyo", "Jakarta", "Delhi", "Manila", "Shanghai",
	"Sao Paulo", "Mumbai", "Beijing", "Cairo", "Mexico City",
	"New York", "London", "Paris", "Moscow", "Sydney",
}

func main() {
	count := flag.Int("count", 10000, "number of measurements to generate")
	flag.Parse()

	// Connect to MinIO
	mc, err := minio.New("localhost:9000", &minio.Options{
		Creds:  credentials.NewStaticV4("minioadmin", "minioadmin", ""),
		Secure: false,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Create bucket if needed
	ctx := context.Background()
	exists, err := mc.BucketExists(ctx, "onebrc")
	if err != nil {
		log.Fatal(err)
	}
	if !exists {
		err = mc.MakeBucket(ctx, "onebrc", minio.MakeBucketOptions{})
		if err != nil {
			log.Fatal(err)
		}
		log.Println("Created bucket: onebrc")
	}

	// Generate data
	log.Printf("Generating %d measurements...\n", *count)
	var buf bytes.Buffer
	for i := 0; i < *count; i++ {
		city := cities[rand.Intn(len(cities))]
		temp := -20.0 + rand.Float64()*70.0 // -20 to 50°C
		buf.WriteString(fmt.Sprintf("%s;%.1f\n", city, temp))
	}

	// Upload to MinIO
	data := buf.Bytes()
	_, err = mc.PutObject(ctx, "onebrc", "measurements.txt",
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Uploaded %d bytes to onebrc/measurements.txt\n", len(data))
}
```

## Run the Refactored Job

```bash
# Make sure MinIO is running
podman ps

# Generate test data
go run tool/main.go -count 10000

# Ensure dependencies are installed
go mod tidy

# Run the job
go run .
```

You should see:

```json
{"time":"...","level":"INFO","msg":"starting 1BRC processing","bucket":"onebrc","input_key":"measurements.txt","output_key":"results.txt"}
{"time":"...","level":"INFO","msg":"1BRC processing completed successfully","cities_processed":15}
```

## Verify Results in MinIO Console

1. Open http://localhost:9001
2. Login with minioadmin/minioadmin
3. Browse the `onebrc` bucket
4. You should see both files:
   - `measurements.txt` (input)
   - `results.txt` (output)
5. Download or preview `results.txt`

Expected format:

```
Beijing=-19.5/16.3/49.8
Cairo=-18.2/17.9/48.5
Delhi=-17.9/15.8/47.3
...
```

## What We Refactored

**Before (local files):**
- Direct `os.Open` and `os.WriteFile`
- File paths in configuration
- Simple and fast for development

**After (cloud storage):**
- `Storage` interface with `GetObject`/`PutObject`
- Bucket and key configuration
- Production-ready architecture
- Same core business logic!

## Benefits of This Refactoring

**Testability:**
- Can mock the `Storage` interface
- Unit tests don't need MinIO running
- Easier to test error paths

**Flexibility:**
- Works with any S3-compatible service
- Easy to swap implementations
- Can add caching, retries, etc.

**Production-ready:**
- Scalable storage
- Cloud-native architecture
- Separation of concerns

## What's Next

Now let's add the full LGTM observability stack so you can see traces, metrics, and logs in Grafana.

[Next: Infrastructure Setup →]({{< ref "06-infrastructure" >}})
