// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"
	"github.com/z5labs/humus/example/job/1brc-walkthrough/onebrc"
	"github.com/z5labs/humus/example/job/1brc-walkthrough/service"
)

// BuildRuntime creates the 1BRC job runtime.
// Configuration values are read from config readers.
func BuildRuntime(
	ctx context.Context,
	minioEndpoint config.Reader[string],
	minioAccessKey config.Reader[string],
	minioSecretKey config.Reader[string],
	minioBucket config.Reader[string],
	inputKey config.Reader[string],
	outputKey config.Reader[string],
) (app.Runtime, error) {
	// Read config values
	endpoint := config.Must(ctx, minioEndpoint)
	accessKey := config.Must(ctx, minioAccessKey)
	secretKey := config.Must(ctx, minioSecretKey)
	bucket := config.Must(ctx, minioBucket)
	input := config.Must(ctx, inputKey)
	output := config.Must(ctx, outputKey)

	// Initialize MinIO client
	minioClient, err := service.NewMinIOClient(endpoint, accessKey, secretKey)
	if err != nil {
		return nil, err
	}

	// Create handler
	handler := onebrc.NewHandler(minioClient, bucket, input, output)

	// The handler's Handle method matches app.Runtime.Run signature
	return app.RuntimeFunc(handler.Handle), nil
}
