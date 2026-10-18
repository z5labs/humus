// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// loggerNameKey is the attribute that names the logger a record of
// client-go's came from, as logr's WithName names it, such as a reflector.
const loggerNameKey = "logger"

// newLogger returns a logr.Logger that emits each of client-go's records as
// one record through logger. The controller puts it in the context of
// everything of client-go's it runs, which takes its logger from there, so
// none of client-go's records falls back to klog's global logger and the
// real stderr.
//
// Only the records klog writes by default reach logger: an Info at
// verbosity 0, as an INFO record, and every Error, as an ERROR record with
// the error's text. client-go logs its details at verbosity 2 and above,
// which klog drops unless its -v flag is raised.
func newLogger(logger log.Logger) logr.Logger {
	return logr.New(&logSink{logger: logger})
}

// logSink is the logr.LogSink newLogger returns. It is never changed once
// built: WithName and WithValues return a new one.
type logSink struct {
	logger log.Logger
	name   string
	values []attribute.KeyValue
}

// Init implements logr.LogSink. The sink records no caller, so it needs
// nothing from info.
func (*logSink) Init(logr.RuntimeInfo) {}

// Enabled implements logr.LogSink. Only verbosity 0 is enabled, as klog's
// default -v is 0, and only when the Logger is enabled for INFO.
func (s *logSink) Enabled(level int) bool {
	return level == 0 && s.logger.Enabled(context.Background(), log.EnabledParameters{Severity: log.SeverityInfo})
}

// Info implements logr.LogSink. msg is the record's body, and keysAndValues
// its attributes, after the sink's own.
func (s *logSink) Info(_ int, msg string, keysAndValues ...any) {
	s.emit(log.SeverityInfo, msg, keysAndValues)
}

// Error implements logr.LogSink. It is an ERROR record, whose
// exception.message is err's text when err isn't nil.
func (s *logSink) Error(err error, msg string, keysAndValues ...any) {
	var extra []attribute.KeyValue
	if err != nil {
		extra = append(extra, semconv.ExceptionMessage(err.Error()))
	}
	s.emit(log.SeverityError, msg, keysAndValues, extra...)
}

// WithValues implements logr.LogSink.
func (s *logSink) WithValues(keysAndValues ...any) logr.LogSink {
	values := make([]attribute.KeyValue, 0, len(s.values)+len(keysAndValues)/2+1)
	values = append(values, s.values...)
	values = append(values, attributes(keysAndValues)...)
	return &logSink{logger: s.logger, name: s.name, values: values}
}

// WithName implements logr.LogSink. A name is appended to the sink's own
// with a '/', as logr's own sinks join them.
func (s *logSink) WithName(name string) logr.LogSink {
	if s.name != "" {
		name = s.name + "/" + name
	}
	return &logSink{logger: s.logger, name: name, values: s.values}
}

func (s *logSink) emit(sev log.Severity, msg string, keysAndValues []any, extra ...attribute.KeyValue) {
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(sev.String())
	r.SetBody(attribute.StringValue(msg))
	if s.name != "" {
		r.AddAttributes(attribute.String(loggerNameKey, s.name))
	}
	r.AddAttributes(s.values...)
	r.AddAttributes(attributes(keysAndValues)...)
	r.AddAttributes(extra...)
	s.logger.Emit(context.Background(), r)
}

// attributes returns logr's key-value pairs as attributes. A key that is
// not a string is its fmt text, and a key with no value has an empty one.
func attributes(keysAndValues []any) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, (len(keysAndValues)+1)/2)
	for i := 0; i < len(keysAndValues); i += 2 {
		key, ok := keysAndValues[i].(string)
		if !ok {
			key = fmt.Sprint(keysAndValues[i])
		}
		v := attribute.StringValue("")
		if i+1 < len(keysAndValues) {
			v = value(keysAndValues[i+1])
		}
		attrs = append(attrs, attribute.KeyValue{Key: attribute.Key(key), Value: v})
	}
	return attrs
}

// value returns v as an attribute value. A string, a bool or a number keeps
// its type, and anything else, an error included, is its fmt text. So is an
// unsigned integer too large for an int64.
func value(v any) attribute.Value {
	switch v := v.(type) {
	case string:
		return attribute.StringValue(v)
	case bool:
		return attribute.BoolValue(v)
	case int:
		return attribute.Int64Value(int64(v))
	case int32:
		return attribute.Int64Value(int64(v))
	case int64:
		return attribute.Int64Value(v)
	case uint32:
		return attribute.Int64Value(int64(v))
	case uint64:
		if v > math.MaxInt64 {
			return attribute.StringValue(fmt.Sprint(v))
		}
		return attribute.Int64Value(int64(v))
	case float64:
		return attribute.Float64Value(v)
	default:
		// fmt recovers when a String or Error method panics on a nil
		// receiver, so a value can't crash client-go's goroutine.
		return attribute.StringValue(fmt.Sprint(v))
	}
}
