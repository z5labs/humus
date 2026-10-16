// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest

import (
	"context"
	"errors"
	"sync"

	"github.com/z5labs/bedrock"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/z5labs/humus/telemetry"
)

// Telemetry records everything an App emits, so a test can assert on the
// spans, metrics and log records production would export. It uses the real
// OpenTelemetry SDK with in-memory exporters: tracetest's span recorder, an
// sdkmetric.ManualReader and an in-memory log exporter. It records every
// severity, debug included, and samples every span.
//
// Nothing in it starts a goroutine, so it works inside a testing/synctest
// bubble. A span is recorded when it ends and a log record when it is
// emitted, with no batching, so each is visible at once.
//
// Spans and log records emitted before the SDK's Shutdown are still
// returned after it, so a test can assert once the App has exited. Every
// method is safe for concurrent use.
//
// A whole-App test passes [Telemetry.Builder] as the App's telemetry
// Builder in place of the one production uses.
type Telemetry struct {
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
	logs   *logRecorder
	sdk    telemetry.SDK
}

// NewTelemetry returns a Telemetry that has recorded nothing yet.
func NewTelemetry() *Telemetry {
	t := &Telemetry{
		spans:  tracetest.NewSpanRecorder(),
		reader: sdkmetric.NewManualReader(),
		logs:   &logRecorder{},
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		// The recorder is itself a simple, synchronous span processor: it
		// keeps each span as it ends. Unlike tracetest's InMemoryExporter,
		// it does not forget its spans when the provider shuts down.
		sdktrace.WithSpanProcessor(t.spans),
	)
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(t.reader))
	// No severity filter: the processor records every severity.
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(t.logs)))

	t.sdk = telemetry.Noop()
	t.sdk.TracerProvider = tp
	t.sdk.MeterProvider = mp
	t.sdk.LoggerProvider = lp
	t.sdk.Shutdown = func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	return t
}

// Builder returns a telemetry.Builder that builds the SDK t records from:
// a TracerProvider that samples every span, a MeterProvider read by t's
// ManualReader, a LoggerProvider that records every severity, and the W3C
// trace context and baggage propagator. The SDK's Shutdown flushes and
// stops all three providers.
//
// Every call of the Builder returns the same SDK, which ignores the
// process and the Dialer it is given.
func (t *Telemetry) Builder() telemetry.Builder {
	return func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
		return t.sdk, nil
	}
}

// Spans returns a copy of the spans that have ended, in the order they
// ended. A span still running is not returned.
func (t *Telemetry) Spans() []sdktrace.ReadOnlySpan {
	return t.spans.Ended()
}

// Metrics collects the current value of every instrument from t's
// ManualReader. Once the SDK has been shut down, the reader is too, and
// Metrics returns its error.
func (t *Telemetry) Metrics(ctx context.Context) (metricdata.ResourceMetrics, error) {
	var rm metricdata.ResourceMetrics
	err := t.reader.Collect(ctx, &rm)
	return rm, err
}

// Logs returns a copy of the log records emitted so far, in the order they
// were emitted.
func (t *Telemetry) Logs() []sdklog.Record {
	return t.logs.records()
}

// logRecorder is an sdklog.Exporter that keeps every record it is given,
// including after it is shut down.
type logRecorder struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (r *logRecorder) Export(_ context.Context, records []sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range records {
		// The processor reuses the slice and the record's attribute
		// storage, so keep a clone.
		r.recs = append(r.recs, rec.Clone())
	}
	return nil
}

func (r *logRecorder) Shutdown(context.Context) error { return nil }

func (r *logRecorder) ForceFlush(context.Context) error { return nil }

func (r *logRecorder) records() []sdklog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdklog.Record, len(r.recs))
	for i := range r.recs {
		out[i] = r.recs[i].Clone()
	}
	return out
}
