// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry

import (
	"context"
	"encoding/base64"
	"io"
	"math"
	"strconv"
	"sync"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

// timestampLayout is RFC 3339 with exactly nine fractional digits, so every
// timestamp has the same width.
const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// jsonLogExporter is an sdklog.Exporter writing each record as one line of
// JSON, in the format the package doc specifies.
type jsonLogExporter struct {
	mu sync.Mutex
	w  io.Writer
	// buf is reused across Export calls, and guarded by mu.
	buf []byte
}

var _ sdklog.Exporter = (*jsonLogExporter)(nil)

// newJSONLogExporter returns an exporter writing to w. Writes are
// serialized, so w need not be safe for concurrent use.
func newJSONLogExporter(w io.Writer) *jsonLogExporter {
	return &jsonLogExporter{w: w}
}

// Export writes records to the exporter's writer, one line each, in a single
// Write call, so the lines of concurrent calls never interleave.
func (e *jsonLogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	b := e.buf[:0]
	for i := range records {
		b = appendRecord(b, &records[i])
	}
	_, err := e.w.Write(b)
	e.buf = b
	return err
}

// ForceFlush does nothing: Export writes through to the writer.
func (*jsonLogExporter) ForceFlush(context.Context) error { return nil }

// Shutdown does nothing: the exporter does not own its writer.
func (*jsonLogExporter) Shutdown(context.Context) error { return nil }

func appendRecord(b []byte, r *sdklog.Record) []byte {
	ts := r.Timestamp()
	if ts.IsZero() {
		ts = r.ObservedTimestamp()
	}
	b = append(b, `{"timestamp":"`...)
	b = ts.UTC().AppendFormat(b, timestampLayout)
	b = append(b, '"')

	if sev := r.Severity(); sev >= log.SeverityTrace1 && sev <= log.SeverityFatal4 {
		text := r.SeverityText()
		if text == "" {
			text = sev.String()
		}
		b = append(b, `,"severity_text":`...)
		b = appendString(b, text)
		b = append(b, `,"severity_number":`...)
		b = strconv.AppendInt(b, int64(sev), 10)
	}

	if name := r.EventName(); name != "" {
		b = append(b, `,"event_name":`...)
		b = appendString(b, name)
	}

	if body := r.Body(); body.Type() != attribute.EMPTY {
		b = append(b, `,"body":`...)
		b = appendValue(b, body)
	}

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    r.TraceID(),
		SpanID:     r.SpanID(),
		TraceFlags: r.TraceFlags(),
	})
	if sc.IsValid() {
		b = append(b, `,"trace_id":"`...)
		b = append(b, sc.TraceID().String()...)
		b = append(b, `","span_id":"`...)
		b = append(b, sc.SpanID().String()...)
		b = append(b, `","trace_flags":"`...)
		b = append(b, sc.TraceFlags().String()...)
		b = append(b, '"')
	}

	b = append(b, `,"scope":`...)
	b = appendString(b, r.InstrumentationScope().Name)

	if r.AttributesLen() > 0 {
		b = append(b, `,"attributes":{`...)
		first := true
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			if !first {
				b = append(b, ',')
			}
			first = false
			b = appendKeyValue(b, kv)
			return true
		})
		b = append(b, '}')
	}

	return append(b, '}', '\n')
}

func appendKeyValue(b []byte, kv attribute.KeyValue) []byte {
	b = appendString(b, string(kv.Key))
	b = append(b, ':')
	return appendValue(b, kv.Value)
}

func appendValue(b []byte, v attribute.Value) []byte {
	switch v.Type() {
	case attribute.BOOL:
		return strconv.AppendBool(b, v.AsBool())
	case attribute.INT64:
		return strconv.AppendInt(b, v.AsInt64(), 10)
	case attribute.FLOAT64:
		return appendFloat(b, v.AsFloat64())
	case attribute.STRING:
		return appendString(b, v.AsString())
	case attribute.BYTESLICE:
		bs := v.AsByteSlice()
		b = append(b, '"')
		b = base64.StdEncoding.AppendEncode(b, bs)
		return append(b, '"')
	case attribute.BOOLSLICE:
		return appendArray(b, v.AsBoolSlice(), strconv.AppendBool)
	case attribute.INT64SLICE:
		return appendArray(b, v.AsInt64Slice(), func(b []byte, n int64) []byte {
			return strconv.AppendInt(b, n, 10)
		})
	case attribute.FLOAT64SLICE:
		return appendArray(b, v.AsFloat64Slice(), appendFloat)
	case attribute.STRINGSLICE:
		return appendArray(b, v.AsStringSlice(), appendString)
	case attribute.SLICE:
		return appendArray(b, v.AsSlice(), appendValue)
	case attribute.MAP:
		b = append(b, '{')
		for i, kv := range v.AsMap() {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendKeyValue(b, kv)
		}
		return append(b, '}')
	default:
		// attribute.EMPTY, and any type a later OpenTelemetry adds before
		// this format says how to write it.
		return append(b, "null"...)
	}
}

func appendArray[T any](b []byte, vs []T, f func([]byte, T) []byte) []byte {
	b = append(b, '[')
	for i, v := range vs {
		if i > 0 {
			b = append(b, ',')
		}
		b = f(b, v)
	}
	return append(b, ']')
}

// appendFloat writes f as encoding/json would, except that NaN and the
// infinities, which JSON cannot represent, are written as strings.
func appendFloat(b []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(b, `"NaN"`...)
	case math.IsInf(f, 1):
		return append(b, `"+Inf"`...)
	case math.IsInf(f, -1):
		return append(b, `"-Inf"`...)
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, f, format, -1, 64)
	if format == 'e' {
		// Shorten e-09 to e-9, as encoding/json does.
		if n := len(b); n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}

const hexDigits = "0123456789abcdef"

// appendString writes s as a JSON string, escaping only what JSON requires
// and replacing invalid UTF-8 with U+FFFD.
func appendString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, `�`...)
			i += size
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}
