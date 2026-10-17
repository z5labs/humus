// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
)

// kgoScope is the instrumentation scope of franz-go's own logs.
const kgoScope = "github.com/twmb/franz-go/pkg/kgo"

// kgoLevels are franz-go's levels that log something, from the lowest
// severity to the highest.
var kgoLevels = [...]kgo.LogLevel{kgo.LogLevelDebug, kgo.LogLevelInfo, kgo.LogLevelWarn, kgo.LogLevelError}

// kgoLogger is a kgo.Logger that emits each of franz-go's messages as one
// record through an OpenTelemetry Logger, with ctx.
type kgoLogger struct {
	ctx    context.Context
	logger log.Logger
}

// Level implements kgo.Logger. It is the lowest of franz-go's levels the
// Logger is enabled for, or kgo.LogLevelNone when it is enabled for none,
// so franz-go builds no message the Logger would drop.
func (l kgoLogger) Level() kgo.LogLevel {
	for _, level := range kgoLevels {
		if l.logger.Enabled(l.ctx, log.EnabledParameters{Severity: severity(level)}) {
			return level
		}
	}
	return kgo.LogLevelNone
}

// Log implements kgo.Logger. msg is the record's body, level its severity,
// and keyvals its attributes.
func (l kgoLogger) Log(level kgo.LogLevel, msg string, keyvals ...any) {
	sev := severity(level)
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(sev.String())
	r.SetBody(attribute.StringValue(msg))
	r.AddAttributes(kgoAttributes(keyvals)...)
	l.logger.Emit(l.ctx, r)
}

// severity returns the OpenTelemetry severity of franz-go's level.
func severity(level kgo.LogLevel) log.Severity {
	switch level {
	case kgo.LogLevelError:
		return log.SeverityError
	case kgo.LogLevelWarn:
		return log.SeverityWarn
	case kgo.LogLevelInfo:
		return log.SeverityInfo
	case kgo.LogLevelDebug:
		return log.SeverityDebug
	default:
		return log.SeverityUndefined
	}
}

// kgoAttributes returns franz-go's key-value pairs as attributes. A key
// that is not a string is its fmt text, and a key with no value has an
// empty one.
func kgoAttributes(keyvals []any) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, (len(keyvals)+1)/2)
	for i := 0; i < len(keyvals); i += 2 {
		key, ok := keyvals[i].(string)
		if !ok {
			key = fmt.Sprint(keyvals[i])
		}
		var v attribute.Value
		if i+1 < len(keyvals) {
			v = kgoValue(keyvals[i+1])
		} else {
			v = attribute.StringValue("")
		}
		attrs = append(attrs, attribute.KeyValue{Key: attribute.Key(key), Value: v})
	}
	return attrs
}

// kgoValue returns v as an attribute value. A string, a number or a bool
// keeps its type, an error is its text, and any other value is its fmt
// text. So is an unsigned integer too large for an int64.
func kgoValue(v any) attribute.Value {
	switch v := v.(type) {
	case string:
		return attribute.StringValue(v)
	case bool:
		return attribute.BoolValue(v)
	case int:
		return attribute.Int64Value(int64(v))
	case int8:
		return attribute.Int64Value(int64(v))
	case int16:
		return attribute.Int64Value(int64(v))
	case int32:
		return attribute.Int64Value(int64(v))
	case int64:
		return attribute.Int64Value(v)
	case uint8:
		return attribute.Int64Value(int64(v))
	case uint16:
		return attribute.Int64Value(int64(v))
	case uint32:
		return attribute.Int64Value(int64(v))
	case uint:
		return unsignedValue(uint64(v))
	case uint64:
		return unsignedValue(v)
	case uintptr:
		return unsignedValue(uint64(v))
	case float32:
		return attribute.Float64Value(float64(v))
	case float64:
		return attribute.Float64Value(v)
	case error:
		// fmt calls Error, and recovers when Error panics on a nil
		// receiver, so a value can't crash franz-go's goroutine.
		return attribute.StringValue(fmt.Sprint(v))
	default:
		return attribute.StringValue(fmt.Sprint(v))
	}
}

// unsignedValue returns v as an int64 value, or as its text when it is too
// large for one.
func unsignedValue(v uint64) attribute.Value {
	if v > math.MaxInt64 {
		return attribute.StringValue(fmt.Sprint(v))
	}
	return attribute.Int64Value(int64(v))
}
