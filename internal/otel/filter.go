// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// filteringProcessor wraps an existing log.Processor to filter log records
// based on minimum severity levels configured per logger name.
type filteringProcessor struct {
	inner      sdklog.Processor
	levels     map[string]log.Severity
	sortedKeys []string // Keys sorted by length (longest first) for prefix matching
}

// newFilteringProcessor creates a new filteringProcessor that wraps the given
// processor and applies minimum log level filtering based on logger name.
//
// The levels map keys are logger names (instrumentation scope names), and values
// are minimum log level strings: "debug", "info", "warn", "warning", "error".
// Logger names support prefix matching - if "github.com/z5labs/humus" is configured,
// it will match "github.com/z5labs/humus/queue/kafka".
//
// Records below the configured minimum level are discarded. Records at or above
// the minimum level are passed to the wrapped processor. If a logger name is not
// in the configuration, all records are allowed (fail-open behavior).
func newFilteringProcessor(inner sdklog.Processor, levels map[string]string) *filteringProcessor {
	severityLevels := make(map[string]log.Severity, len(levels))
	sortedKeys := make([]string, 0, len(levels))

	for name, levelStr := range levels {
		severityLevels[name] = parseLogLevel(levelStr)
		sortedKeys = append(sortedKeys, name)
	}

	// Sort keys by length descending for longest prefix match
	for i := 0; i < len(sortedKeys)-1; i++ {
		for j := i + 1; j < len(sortedKeys); j++ {
			if len(sortedKeys[j]) > len(sortedKeys[i]) {
				sortedKeys[i], sortedKeys[j] = sortedKeys[j], sortedKeys[i]
			}
		}
	}

	return &filteringProcessor{
		inner:      inner,
		levels:     severityLevels,
		sortedKeys: sortedKeys,
	}
}

// parseLogLevel converts a log level string to an OpenTelemetry log.Severity.
// Supported values: "debug", "info", "warn", "warning", "error".
// Unknown values default to DEBUG (most permissive).
func parseLogLevel(level string) log.Severity {
	switch strings.ToLower(level) {
	case "debug":
		return log.SeverityDebug
	case "info":
		return log.SeverityInfo
	case "warn", "warning":
		return log.SeverityWarn
	case "error":
		return log.SeverityError
	default:
		// Unknown level defaults to DEBUG (allow all)
		return log.SeverityDebug
	}
}

// OnEmit filters log records based on configured minimum severity levels.
// Records below the minimum level for their logger name are discarded.
func (p *filteringProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	if !p.shouldEmit(record) {
		return nil
	}
	return p.inner.OnEmit(ctx, record)
}

// shouldEmit determines if a log record should be emitted based on its
// instrumentation scope (logger name) and severity level.
func (p *filteringProcessor) shouldEmit(record *sdklog.Record) bool {
	if len(p.levels) == 0 {
		return true
	}

	loggerName := record.InstrumentationScope().Name
	minSeverity, found := p.findMinimumLevel(loggerName)
	if !found {
		// No configuration for this logger, allow all records
		return true
	}

	return record.Severity() >= minSeverity
}

// findMinimumLevel finds the minimum severity level for the given logger name.
// It uses longest prefix matching to find the most specific configuration.
func (p *filteringProcessor) findMinimumLevel(loggerName string) (log.Severity, bool) {
	// First check for exact match
	if level, ok := p.levels[loggerName]; ok {
		return level, true
	}

	// Then check for prefix match (longest prefix first)
	for _, prefix := range p.sortedKeys {
		if strings.HasPrefix(loggerName, prefix) {
			return p.levels[prefix], true
		}
	}

	return 0, false
}

// Shutdown shuts down the wrapped processor.
func (p *filteringProcessor) Shutdown(ctx context.Context) error {
	return p.inner.Shutdown(ctx)
}

// ForceFlush forces the wrapped processor to flush its data.
func (p *filteringProcessor) ForceFlush(ctx context.Context) error {
	return p.inner.ForceFlush(ctx)
}
