// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package noop

import (
	"context"
	"log/slog"
)

type LogHandler struct{}

func (LogHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (LogHandler) Handle(_ context.Context, _ slog.Record) error {
	return nil
}

func (h LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h LogHandler) WithGroup(_ string) slog.Handler {
	return h
}
