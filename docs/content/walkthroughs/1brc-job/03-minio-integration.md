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

## Storage Client Implementation

Create `storage/minio.go`:

```go
package storage

import (
	"context"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Client struct {
	mc *minio.Client
}

func NewClient(endpoint, accessKey, secretKey string) (*Client, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,  // Use HTTP for local development
	})
	if err != nil {
		return nil, err
	}

	return &Client{mc: mc}, nil
}

func (c *Client) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return c.mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
}

func (c *Client) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64) error {
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

## Updating the Init Function

Now update `app/app.go` to create the MinIO client:

```go
func Init(ctx context.Context, cfg Config) (*job.App, error) {
	// 1. Create MinIO client
	minioClient, err := storage.NewClient(
		cfg.Minio.Endpoint,
		cfg.Minio.AccessKey,
		cfg.Minio.SecretKey,
	)
	if err != nil {
		return nil, err
	}

	// 2. Build handler (we'll implement this next)
	// handler := onebrc.NewHandler(
	//     minioClient,
	//     cfg.Minio.Bucket,
	//     cfg.OneBRC.InputKey,
	//     cfg.OneBRC.OutputKey,
	// )

	// 3. Return job
	return job.NewApp(nil), nil  // nil handler for now
}
```

Don't forget to add the import:

```go
import (
	"context"

	"1brc-walkthrough/storage"
	"github.com/z5labs/humus/job"
)
```

## Usage Pattern

The handler will use this client like this:

```go
// Read from S3
rc, err := storage.GetObject(ctx, bucket, "measurements.txt")
defer rc.Close()
data := Parse(bufio.NewReader(rc))

// Write to S3
results := FormatResults(data)
storage.PutObject(ctx, bucket, "results.txt", bytes.NewReader(results), int64(len(results)))
```

## What's Next

Now we'll implement the core 1BRC algorithm for parsing and calculating statistics.

[Next: 1BRC Algorithm →]({{< ref "04-1brc-algorithm" >}})
