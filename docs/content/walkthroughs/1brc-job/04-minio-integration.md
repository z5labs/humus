---
title: MinIO Integration
description: Building the S3 storage client
weight: 4
type: docs
---

The `storage` package wraps the MinIO Go client for S3 operations.

## Client Implementation

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
- Carries trace spans automatically

## Usage in Handler

The handler will use this client:

```go
// Read from S3
rc, err := storage.GetObject(ctx, bucket, "measurements.txt")
defer rc.Close()
data := Parse(bufio.NewReader(rc))

// Write to S3
results := FormatResults(data)
storage.PutObject(ctx, bucket, "results.txt", bytes.NewReader(results), int64(len(results)))
```

## Next Steps

Continue to: [1BRC Algorithm]({{< ref "05-1brc-algorithm" >}})
