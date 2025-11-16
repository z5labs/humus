---
title: Building a Basic Job
description: Understanding the Config struct and Init function
weight: 3
type: docs
---

This section covers `app/app.go` - the core of your Humus job.

## The Config Struct

```go
package app

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
```

**Key points:**
- Embeds `job.Config` for OTel configuration
- `` `config:",squash"` `` flattens fields in YAML
- Custom fields map to `config.yaml`

## The Init Function

```go
func Init(ctx context.Context, cfg Config) (*job.App, error) {
	// 1. Create dependencies
	minioClient, err := storage.NewClient(
		cfg.Minio.Endpoint,
		cfg.Minio.AccessKey,
		cfg.Minio.SecretKey,
	)
	if err != nil {
		return nil, err
	}

	// 2. Build handler
	handler := onebrc.NewHandler(
		minioClient,
		cfg.Minio.Bucket,
		cfg.OneBRC.InputKey,
		cfg.OneBRC.OutputKey,
	)

	// 3. Return job
	return job.NewApp(handler), nil
}
```

**Responsibilities:**
1. Dependency injection (create clients)
2. Handler construction
3. Error handling

**DON'T:**
- ❌ Start goroutines
- ❌ Call `Run()` on the handler
- ❌ Initialize OTel manually

## The Handler Interface

Your business logic implements:

```go
type Handler interface {
	Handle(context.Context) error
}
```

## How job.Run Works

When you call `job.Run(configReader, initFunc)`:

1. Parses config (YAML templates → struct)
2. Initializes OTel SDK automatically
3. Calls your `Init` function
4. Wraps with middleware (panic recovery, signals)
5. Runs `handler.Handle(ctx)`
6. Graceful shutdown with OTel flush

## Next Steps

Continue to: [MinIO Integration]({{< ref "minio-integration" >}})
