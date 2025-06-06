// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type slogExporter struct {
	handler slog.Handler
}

// Export implements log.Exporter.
func (s *slogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	const sevOffset = log.SeverityDebug - log.Severity(slog.LevelDebug)
	for _, record := range records {
		sr := slog.Record{
			Time:    record.Timestamp(),
			Level:   slog.Level(record.Severity() - sevOffset),
			Message: record.Body().AsString(),
		}

		record.WalkAttributes(func(kv log.KeyValue) bool {
			sr.AddAttrs(slog.Attr{
				Key:   kv.Key,
				Value: mapLogValue(kv.Value),
			})
			return true
		})

		sr.AddAttrs(slog.Group(
			"otel",
			slog.String("trace.id", record.TraceID().String()),
			slog.String("span.id", record.SpanID().String()),
		))

		err := s.handler.Handle(ctx, sr)
		if err != nil {
			return err
		}
	}

	return nil
}

func mapLogValue(v log.Value) slog.Value {
	switch v.Kind() {
	case log.KindBool:
		return slog.BoolValue(v.AsBool())
	case log.KindBytes:
		return slog.AnyValue(v.AsBytes())
	case log.KindFloat64:
		return slog.Float64Value(v.AsFloat64())
	case log.KindInt64:
		return slog.Int64Value(v.AsInt64())
	case log.KindMap:
		kvs := v.AsMap()
		attrs := make([]slog.Attr, len(kvs))

		for i, kv := range kvs {
			attrs[i] = slog.Attr{
				Key:   kv.Key,
				Value: mapLogValue(kvs[i].Value),
			}
		}

		return slog.GroupValue(attrs...)
	case log.KindSlice:
		vs := v.AsSlice()
		vals := make([]slog.Value, len(vs))

		for i := range vs {
			vals[i] = mapLogValue(vs[i])
		}

		return slog.AnyValue(vals)
	case log.KindString:
		return slog.StringValue(v.AsString())
	default:
		return slog.StringValue(v.String())
	}
}

// ForceFlush implements log.Exporter.
func (s *slogExporter) ForceFlush(ctx context.Context) error {
	return nil
}

// Shutdown implements log.Exporter.
func (s *slogExporter) Shutdown(ctx context.Context) error {
	return nil
}
