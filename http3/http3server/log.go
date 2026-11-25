// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server

import (
	"context"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
)

// newSlogLogger returns the *slog.Logger the http3.Server logs through. It
// emits each of quic-go's Error records as one ERROR record through logger,
// and drops every lower level: quic-go logs its details at Debug, once per
// connection and request. Left nil, quic-go would log to slog.Default().
func newSlogLogger(logger log.Logger) *slog.Logger {
	return slog.New(&slogHandler{logger: logger})
}

// slogHandler is the slog.Handler newSlogLogger's Logger has. It is never
// changed once built: WithAttrs and WithGroup return a new one.
type slogHandler struct {
	logger log.Logger
	attrs  []attribute.KeyValue
	group  string // the prefix of each attribute's key, with its "."
}

// Enabled reports whether level is Error or above.
func (*slogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelError
}

// Handle emits r as one ERROR record, whose body is r's message, with the
// handler's attributes and then r's.
func (h *slogHandler) Handle(ctx context.Context, r slog.Record) error {
	var rec log.Record
	rec.SetTimestamp(r.Time)
	rec.SetSeverity(log.SeverityError)
	rec.SetSeverityText(log.SeverityError.String())
	rec.SetBody(attribute.StringValue(r.Message))
	rec.AddAttributes(h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		rec.AddAttributes(attributes(h.group, a)...)
		return true
	})
	h.logger.Emit(ctx, rec)
	return nil
}

// WithAttrs returns a handler with attrs after h's own.
func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	all := append([]attribute.KeyValue(nil), h.attrs...)
	for _, a := range attrs {
		all = append(all, attributes(h.group, a)...)
	}
	return &slogHandler{logger: h.logger, attrs: all, group: h.group}
}

// WithGroup returns a handler whose later attributes' keys start with
// name and a ".".
func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &slogHandler{logger: h.logger, attrs: h.attrs, group: h.group + name + "."}
}

// attributes returns a as attributes whose keys start with prefix. A group
// is flattened, its members' keys joined to its own with a ".". A string, a
// bool or a number keeps its type, and anything else is its text.
func attributes(prefix string, a slog.Attr) []attribute.KeyValue {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		if a.Key != "" {
			prefix += a.Key + "."
		}
		var attrs []attribute.KeyValue
		for _, member := range v.Group() {
			attrs = append(attrs, attributes(prefix, member)...)
		}
		return attrs
	}
	if a.Equal(slog.Attr{}) {
		return nil
	}
	key := attribute.Key(strings.TrimSuffix(prefix+a.Key, "."))
	switch v.Kind() {
	case slog.KindString:
		return []attribute.KeyValue{key.String(v.String())}
	case slog.KindBool:
		return []attribute.KeyValue{key.Bool(v.Bool())}
	case slog.KindInt64:
		return []attribute.KeyValue{key.Int64(v.Int64())}
	case slog.KindFloat64:
		return []attribute.KeyValue{key.Float64(v.Float64())}
	default:
		return []attribute.KeyValue{key.String(v.String())}
	}
}
