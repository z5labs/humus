// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/log/logtest"
)

type mockProcessor struct {
	emitted []*sdklog.Record
}

func (m *mockProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	m.emitted = append(m.emitted, record)
	return nil
}

func (m *mockProcessor) Shutdown(ctx context.Context) error {
	return nil
}

func (m *mockProcessor) ForceFlush(ctx context.Context) error {
	return nil
}

// newTestRecord creates a test log record with the given severity and logger name.
func newTestRecord(severity log.Severity, loggerName string) *sdklog.Record {
	factory := logtest.RecordFactory{
		Severity:             severity,
		InstrumentationScope: &instrumentation.Scope{Name: loggerName},
	}
	record := factory.NewRecord()
	return &record
}

func TestParseLogLevel(t *testing.T) {
	testCases := []struct {
		Name     string
		Input    string
		Expected log.Severity
	}{
		{
			Name:     "debug",
			Input:    "debug",
			Expected: log.SeverityDebug,
		},
		{
			Name:     "DEBUG uppercase",
			Input:    "DEBUG",
			Expected: log.SeverityDebug,
		},
		{
			Name:     "info",
			Input:    "info",
			Expected: log.SeverityInfo,
		},
		{
			Name:     "INFO uppercase",
			Input:    "INFO",
			Expected: log.SeverityInfo,
		},
		{
			Name:     "warn",
			Input:    "warn",
			Expected: log.SeverityWarn,
		},
		{
			Name:     "warning",
			Input:    "warning",
			Expected: log.SeverityWarn,
		},
		{
			Name:     "WARN uppercase",
			Input:    "WARN",
			Expected: log.SeverityWarn,
		},
		{
			Name:     "error",
			Input:    "error",
			Expected: log.SeverityError,
		},
		{
			Name:     "ERROR uppercase",
			Input:    "ERROR",
			Expected: log.SeverityError,
		},
		{
			Name:     "unknown defaults to debug",
			Input:    "unknown",
			Expected: log.SeverityDebug,
		},
		{
			Name:     "empty string defaults to debug",
			Input:    "",
			Expected: log.SeverityDebug,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			result := parseLogLevel(tc.Input)
			require.Equal(t, tc.Expected, result)
		})
	}
}

func TestFilteringProcessor_OnEmit(t *testing.T) {
	t.Run("should emit records at or above minimum level", func(t *testing.T) {
		mock := &mockProcessor{}
		fp := newFilteringProcessor(mock, map[string]string{
			"test-logger": "info",
		})

		ctx := context.Background()

		// Record at INFO level should be emitted
		infoRecord := newTestRecord(log.SeverityInfo, "test-logger")
		err := fp.OnEmit(ctx, infoRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 1)

		// Record at WARN level should be emitted
		warnRecord := newTestRecord(log.SeverityWarn, "test-logger")
		err = fp.OnEmit(ctx, warnRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 2)

		// Record at ERROR level should be emitted
		errorRecord := newTestRecord(log.SeverityError, "test-logger")
		err = fp.OnEmit(ctx, errorRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 3)
	})

	t.Run("should discard records below minimum level", func(t *testing.T) {
		mock := &mockProcessor{}
		fp := newFilteringProcessor(mock, map[string]string{
			"test-logger": "warn",
		})

		ctx := context.Background()

		// Record at DEBUG level should be discarded
		debugRecord := newTestRecord(log.SeverityDebug, "test-logger")
		err := fp.OnEmit(ctx, debugRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 0)

		// Record at INFO level should be discarded
		infoRecord := newTestRecord(log.SeverityInfo, "test-logger")
		err = fp.OnEmit(ctx, infoRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 0)
	})

	t.Run("should allow all levels for unconfigured loggers", func(t *testing.T) {
		mock := &mockProcessor{}
		fp := newFilteringProcessor(mock, map[string]string{
			"other-logger": "error",
		})

		ctx := context.Background()

		// Record from unconfigured logger should be emitted
		debugRecord := newTestRecord(log.SeverityDebug, "test-logger")
		err := fp.OnEmit(ctx, debugRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 1)
	})

	t.Run("should allow all levels when no config is provided", func(t *testing.T) {
		mock := &mockProcessor{}
		fp := newFilteringProcessor(mock, map[string]string{})

		ctx := context.Background()

		debugRecord := newTestRecord(log.SeverityDebug, "test-logger")
		err := fp.OnEmit(ctx, debugRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 1)
	})

	t.Run("should support prefix matching", func(t *testing.T) {
		mock := &mockProcessor{}
		fp := newFilteringProcessor(mock, map[string]string{
			"github.com/z5labs/humus": "warn",
		})

		ctx := context.Background()

		// Logger with matching prefix should use the configured level
		infoRecord := newTestRecord(log.SeverityInfo, "github.com/z5labs/humus/queue/kafka")
		err := fp.OnEmit(ctx, infoRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 0)

		// Same prefix but at WARN level should be emitted
		warnRecord := newTestRecord(log.SeverityWarn, "github.com/z5labs/humus/queue/kafka")
		err = fp.OnEmit(ctx, warnRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 1)
	})

	t.Run("should prefer exact match over prefix match", func(t *testing.T) {
		mock := &mockProcessor{}
		fp := newFilteringProcessor(mock, map[string]string{
			"github.com/z5labs/humus":             "warn",
			"github.com/z5labs/humus/queue/kafka": "debug",
		})

		ctx := context.Background()

		// Exact match should use debug level
		debugRecord := newTestRecord(log.SeverityDebug, "github.com/z5labs/humus/queue/kafka")
		err := fp.OnEmit(ctx, debugRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 1)

		// Prefix match should use warn level
		infoRecord := newTestRecord(log.SeverityInfo, "github.com/z5labs/humus/rest")
		err = fp.OnEmit(ctx, infoRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 1) // Still 1, INFO < WARN so filtered
	})

	t.Run("should prefer longest prefix match", func(t *testing.T) {
		mock := &mockProcessor{}
		fp := newFilteringProcessor(mock, map[string]string{
			"github.com/z5labs":       "error",
			"github.com/z5labs/humus": "info",
		})

		ctx := context.Background()

		// Should match longer prefix "github.com/z5labs/humus" with info level
		infoRecord := newTestRecord(log.SeverityInfo, "github.com/z5labs/humus/rest")
		err := fp.OnEmit(ctx, infoRecord)
		require.NoError(t, err)
		require.Len(t, mock.emitted, 1)
	})
}

func TestFilteringProcessor_Shutdown(t *testing.T) {
	t.Run("should delegate to inner processor", func(t *testing.T) {
		mock := &mockProcessor{}
		fp := newFilteringProcessor(mock, map[string]string{})

		ctx := context.Background()
		err := fp.Shutdown(ctx)
		require.NoError(t, err)
	})
}

func TestFilteringProcessor_ForceFlush(t *testing.T) {
	t.Run("should delegate to inner processor", func(t *testing.T) {
		mock := &mockProcessor{}
		fp := newFilteringProcessor(mock, map[string]string{})

		ctx := context.Background()
		err := fp.ForceFlush(ctx)
		require.NoError(t, err)
	})
}
