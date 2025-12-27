// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/example/job/1brc-walkthrough/onebrc"
	"github.com/z5labs/humus/example/job/1brc-walkthrough/service"
)

// Config defines the application configuration.
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

// Init initializes the application runtime.
func Init(ctx context.Context, cfg Config) (app.Runtime, error) {
	minioClient, err := service.NewMinIOClient(cfg.Minio.Endpoint, cfg.Minio.AccessKey, cfg.Minio.SecretKey)
	if err != nil {
		return nil, err
	}

	handler := onebrc.NewHandler(
		minioClient,
		cfg.Minio.Bucket,
		cfg.OneBRC.InputKey,
		cfg.OneBRC.OutputKey,
	)

	// The handler's Handle method matches app.Runtime.Run signature
	return app.RuntimeFunc(handler.Handle), nil
}
