// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"

	"github.com/z5labs/humus/example/job/1brc-walkthrough/onebrc"
	"github.com/z5labs/humus/example/job/1brc-walkthrough/storage"
	"github.com/z5labs/humus/job"
)

// Config defines the application configuration.
type Config struct {
	job.Config `config:",squash"`

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

// Init initializes the job application.
func Init(ctx context.Context, cfg Config) (*job.App, error) {
	minioClient, err := storage.NewClient(cfg.Minio.Endpoint, cfg.Minio.AccessKey, cfg.Minio.SecretKey)
	if err != nil {
		return nil, err
	}

	handler := onebrc.NewHandler(
		minioClient,
		cfg.Minio.Bucket,
		cfg.OneBRC.InputKey,
		cfg.OneBRC.OutputKey,
	)

	return job.NewApp(handler), nil
}
