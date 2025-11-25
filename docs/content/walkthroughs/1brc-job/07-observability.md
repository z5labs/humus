---
title: Adding Observability
description: Retrofitting your job with traces, metrics, and logs
weight: 7
type: docs
---

Now that your job works and the observability stack is running, let's add OpenTelemetry instrumentation.

## Update Configuration

First, update `config.yaml` to add OTel settings:

```yaml
# OpenTelemetry configuration
otel:
  service_name: {{env "OTEL_SERVICE_NAME" | default "1brc-job-walkthrough"}}
  resource_attributes:
    deployment.environment: {{env "OTEL_ENVIRONMENT" | default "development"}}

  sdk:
    disabled: {{env "OTEL_SDK_DISABLED" | default false}}

  exporter:
    otlp:
      protocol: {{env "OTEL_EXPORTER_OTLP_PROTOCOL" | default "grpc"}}
      endpoint: {{env "OTEL_EXPORTER_OTLP_ENDPOINT" | default "localhost:4317"}}
      insecure: {{env "OTEL_EXPORTER_OTLP_INSECURE" | default true}}

# MinIO configuration (unchanged)
minio:
  endpoint: {{env "MINIO_ENDPOINT" | default "localhost:9000"}}
  access_key: {{env "MINIO_ACCESS_KEY" | default "minioadmin"}}
  secret_key: {{env "MINIO_SECRET_KEY" | default "minioadmin"}}
  bucket: {{env "MINIO_BUCKET" | default "onebrc"}}

# 1BRC configuration (unchanged)
onebrc:
  input_key: {{env "INPUT_KEY" | default "measurements.txt"}}
  output_key: {{env "OUTPUT_KEY" | default "results.txt"}}
```

## Update Config Struct

Update `app/app.go` to embed `job.Config`:

```go
package app

import (
	"context"

	"1brc-walkthrough/onebrc"
	"1brc-walkthrough/service"
	"github.com/z5labs/humus/job"
)

type Config struct {
	job.Config `config:",squash"`  // Add this line

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

// Init function remains the same
func Init(ctx context.Context, cfg Config) (*job.App, error) {
	minioClient, err := service.NewMinIOClient(
		cfg.Minio.Endpoint,
		cfg.Minio.AccessKey,
		cfg.Minio.SecretKey,
	)
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
```

The `` `config:",squash"` `` tag flattens the `job.Config` fields into your config YAML.

## Add Instrumentation to Handler

Now update `onebrc/handler.go` to add traces, metrics, and logs:

```go
package onebrc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/z5labs/humus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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
	tracer    func(context.Context, string) (context.Context, func())
	meter     metric.Meter
}

func NewHandler(storage Storage, bucket, inputKey, outputKey string) *Handler {
	tracer := otel.Tracer("onebrc")
	meter := otel.Meter("onebrc")

	return &Handler{
		storage:   storage,
		bucket:    bucket,
		inputKey:  inputKey,
		outputKey: outputKey,
		log:       humus.Logger("onebrc"),
		tracer: func(ctx context.Context, name string) (context.Context, func()) {
			ctx, span := tracer.Start(ctx, name)
			return ctx, func() { span.End() }
		},
		meter: meter,
	}
}

func (h *Handler) Handle(ctx context.Context) error {
	ctx, end := h.tracer(ctx, "handle")
	defer end()

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
	defer rc.Close()

	// 2. Parse (with span)
	parseCtx, parseEnd := h.tracer(ctx, "parse")
	cityStats, err := Parse(bufio.NewReader(rc))
	parseEnd()
	if err != nil {
		h.log.ErrorContext(parseCtx, "failed to parse temperature data", slog.Any("error", err))
		return fmt.Errorf("parse: %w", err)
	}

	// 3. Record metric
	counter, err := h.meter.Int64Counter("onebrc.cities.count")
	if err != nil {
		h.log.ErrorContext(ctx, "failed to create counter", slog.Any("error", err))
		return fmt.Errorf("create counter: %w", err)
	}
	counter.Add(ctx, int64(len(cityStats)),
		metric.WithAttributes(attribute.String("bucket", h.bucket)))

	// 4. Calculate (with span)
	_, calcEnd := h.tracer(ctx, "calculate")
	results := Calculate(cityStats)
	calcEnd()

	// 5. Write results (with span)
	writeCtx, writeEnd := h.tracer(ctx, "write_results")
	output := FormatResults(results)
	outputBytes := []byte(output)

	err = h.storage.PutObject(writeCtx, h.bucket, h.outputKey,
		bytes.NewReader(outputBytes), int64(len(outputBytes)))
	writeEnd()
	if err != nil {
		h.log.ErrorContext(writeCtx, "failed to upload results", slog.Any("error", err))
		return fmt.Errorf("put object: %w", err)
	}

	h.log.InfoContext(ctx, "1BRC processing completed successfully",
		slog.Int("cities_processed", len(cityStats)),
	)

	return nil
}
```

## Key Changes Explained

**Configuration:**
- Added `job.Config` embedding with squash tag
- Added full OTel configuration section
- Humus automatically initializes the OTel SDK using these settings

**Handler Instrumentation:**
- **Logger**: `humus.Logger("onebrc")` gives you a structured logger integrated with OTel
- **Tracer**: `otel.Tracer("onebrc")` creates trace spans
- **Meter**: `otel.Meter("onebrc")` records metrics
- **Child Spans**: Create spans for parse, calculate, and write operations
- **Structured Logs**: Use `InfoContext` and `ErrorContext` for trace correlation

## Observability Patterns

**Spans:**
- Parent span `handle` encompasses all work
- Child spans for each major operation (parse, calculate, write)
- Call `span.End()` immediately after work completes

**Metrics:**
- Create instruments with descriptive names (`onebrc.cities.count`)
- Record with context for trace correlation
- Add attributes for dimensions (bucket name)

**Logs:**
- Use `InfoContext`/`ErrorContext` for automatic trace ID injection
- Add structured fields with `slog.String`, `slog.Int`, etc.
- Log at operation boundaries (start, end, errors)

## What's Next

Now let's run the job and view its telemetry in Grafana!

[Next: Running and Monitoring →]({{< ref "08-running-monitoring" >}})
