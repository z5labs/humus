// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus provides utilities for building bedrock applications
// with integrated OpenTelemetry logging.
//
// The package provides OpenTelemetry-integrated structured logging
// via [Logger] and [LogHandler].
//
// # Basic Usage
//
// Create a logger with OpenTelemetry integration:
//
//	log := humus.Logger("myapp")
//	log.Info("application started")
package humus

import (
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
)

// Logger creates a new structured logger with OpenTelemetry integration.
// The logger automatically bridges log records to OpenTelemetry, enabling
// correlation between logs and traces.
//
// The name parameter identifies the logger and appears in log output,
// making it easier to filter and identify log sources.
//
// Example:
//
//	log := humus.Logger("rest-api")
//	log.Info("server started", slog.Int("port", 8080))
//	log.Error("connection failed", slog.String("error", err.Error()))
func Logger(name string) *slog.Logger {
	return otelslog.NewLogger(name)
}

// LogHandler creates a new slog.Handler with OpenTelemetry integration.
// This is useful when you need to create a custom logger with specific
// handler options while maintaining OpenTelemetry integration.
//
// Example:
//
//	handler := humus.LogHandler("myapp")
//	logger := slog.New(handler)
func LogHandler(name string) slog.Handler {
	return otelslog.NewHandler(name)
}
