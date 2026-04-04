// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus provides utilities for building bedrock applications
// with integrated OpenTelemetry logging.
package humus

import (
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
)

// Logger creates a new structured logger with OpenTelemetry integration.
// The logger automatically bridges log records to OpenTelemetry, enabling
// correlation between logs and traces.
func Logger(name string) *slog.Logger {
	return otelslog.NewLogger(name)
}

// LogHandler creates a new slog.Handler with OpenTelemetry integration.
func LogHandler(name string) slog.Handler {
	return otelslog.NewHandler(name)
}
