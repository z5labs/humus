// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"log/slog"

	"github.com/z5labs/humus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func logger() *slog.Logger {
	return humus.Logger("github.com/z5labs/humus/queue/kafka")
}

func tracer() trace.Tracer {
	return otel.Tracer("github.com/z5labs/humus/queue/kafka")
}
