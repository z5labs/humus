---
title: Observability
description: Adding traces, metrics, and logs
weight: 6
type: docs
---

The `onebrc/handler.go` orchestrates the workflow and adds OpenTelemetry instrumentation.

## Handler with OTel

```go
package onebrc

import (
	"github.com/z5labs/humus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

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
```

## Instrumented Workflow

```go
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

## Observability Patterns

**Spans:**
- Create child spans from parent context (not reassigning `ctx`)
- End spans immediately after work completes
- Parent span encompasses all children

**Metrics:**
- Use `otel.Meter` to create instruments
- Record with context for trace correlation
- Add attributes for dimensions

**Logs:**
- Use `humus.Logger` (auto-integrated with OTel)
- Use `InfoContext`/`ErrorContext` for trace correlation
- Add structured fields with `slog.String`, `slog.Int`, etc.

## Viewing in Grafana

**Traces:** See waterfall of spans showing duration of parse/calculate/write  
**Metrics:** Query `onebrc_cities_count` to see processed cities over time  
**Logs:** Filter by service name and see correlated trace IDs

## Next Steps

Continue to: [Running and Monitoring]({{< ref "07-running-monitoring" >}})
