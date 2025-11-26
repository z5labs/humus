// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package service

import (
	"context"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOClient wraps a MinIO client for S3 operations.
type MinIOClient struct {
	mc *minio.Client
}

// NewMinIOClient creates a new MinIO client.
func NewMinIOClient(endpoint, accessKey, secretKey string) (*MinIOClient, error) {
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	if err != nil {
		return nil, err
	}

	return &MinIOClient{mc: mc}, nil
}

// GetObject retrieves an object from the specified bucket.
func (c *MinIOClient) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return c.mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
}

// PutObject uploads an object to the specified bucket.
func (c *MinIOClient) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64) error {
	_, err := c.mc.PutObject(ctx, bucket, key, reader, size, minio.PutObjectOptions{})
	return err
}
