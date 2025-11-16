// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

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

// Storage defines the interface for S3 operations.
type Storage interface {
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64) error
}

// Handler implements the 1BRC challenge processing logic.
type Handler struct {
	storage   Storage
	bucket    string
	inputKey  string
	outputKey string
	log       *slog.Logger
	tracer    func(context.Context, string) (context.Context, func())
	meter     metric.Meter
}

// NewHandler creates a new Handler.
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

// Handle executes the 1BRC processing workflow.
func (h *Handler) Handle(ctx context.Context) error {
	ctx, end := h.tracer(ctx, "handle")
	defer end()

	h.log.InfoContext(ctx, "starting 1BRC processing",
		slog.String("bucket", h.bucket),
		slog.String("input_key", h.inputKey),
		slog.String("output_key", h.outputKey),
	)

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

	parseCtx, parseEnd := h.tracer(ctx, "parse")
	cityStats, err := Parse(bufio.NewReader(rc))
	parseEnd()
	if err != nil {
		h.log.ErrorContext(parseCtx, "failed to parse temperature data", slog.Any("error", err))
		return fmt.Errorf("parse: %w", err)
	}

	counter, err := h.meter.Int64Counter("onebrc.cities.count")
	if err != nil {
		h.log.ErrorContext(ctx, "failed to create counter", slog.Any("error", err))
		return fmt.Errorf("create counter: %w", err)
	}
	counter.Add(ctx, int64(len(cityStats)), metric.WithAttributes(attribute.String("bucket", h.bucket)))

	_, calcEnd := h.tracer(ctx, "calculate")
	results := Calculate(cityStats)
	calcEnd()

	writeCtx, writeEnd := h.tracer(ctx, "write_results")
	output := FormatResults(results)
	outputBytes := []byte(output)

	err = h.storage.PutObject(writeCtx, h.bucket, h.outputKey, bytes.NewReader(outputBytes), int64(len(outputBytes)))
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
